package app

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/s3"

	"s4browser/internal/browse"
	"s4browser/internal/objects"
)

// The bindings for object commands: delete, create folder, presigned links,
// the metadata view and tag editing. A delete runs in two calls: PrepareDelete
// counts what the selection holds (listing every folder in it) and keeps the
// keys in Go, then RunDelete deletes exactly those keys once the user has
// confirmed the counts. Keys never travel through the page.

// objectTimeout bounds the small object calls.
const objectTimeout = 2 * time.Minute

// deleteTimeout bounds counting and deleting a large selection.
const deleteTimeout = 60 * time.Minute

// planLife is how long a counted delete may wait for confirmation.
const planLife = 15 * time.Minute

// ObjectSelection names rows selected in a listing, as DownloadSelection does.
type ObjectSelection struct {
	ProfileID string   `json:"profileId"`
	Bucket    string   `json:"bucket"`
	ListingID string   `json:"listingId"`
	Field     string   `json:"field"`
	Desc      bool     `json:"desc"`
	Version   uint64   `json:"version"`
	Ranges    [][2]int `json:"ranges"`
}

// DeletePreview is what the confirmation shows.
type DeletePreview struct {
	ID      string `json:"id"`
	Bucket  string `json:"bucket"`
	Prefix  string `json:"prefix"`
	Objects int    `json:"objects"`
	Bytes   int64  `json:"bytes"`
	// Folders counts selected folders; Selected counts selected rows.
	Folders  int `json:"folders"`
	Selected int `json:"selected"`
	// Placeholders counts zero-byte folder placeholder objects among Objects.
	Placeholders int `json:"placeholders"`
	// Sample is up to 8 of the keys, in order.
	Sample []string `json:"sample"`
	// Versioning is off, enabled, suspended or unknown; VersioningError says
	// why it is unknown.
	Versioning      string `json:"versioning"`
	VersioningError string `json:"versioningError,omitempty"`
	// Transfers counts unfinished transfers whose key is among the selection.
	Transfers int `json:"transfers"`
}

// ObjectProgress is the "s4:objects" event sent while a delete is counted or run.
type ObjectProgress struct {
	Phase string `json:"phase"` // counting or deleting
	Done  int    `json:"done"`
	Total int    `json:"total"`
}

type pendingDelete struct {
	profile string
	plan    *objects.Plan
	expires time.Time
}

// objectWork holds the counted deletes and the cancel of the one running.
type objectWork struct {
	mu      sync.Mutex
	plans   map[string]pendingDelete
	cancel  context.CancelFunc
	running bool
}

func (w *objectWork) start(parent context.Context, timeout time.Duration) (context.Context, func(), error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.running {
		return nil, nil, errors.New("another delete is being counted or run; wait for it or cancel it")
	}
	ctx, cancel := context.WithTimeout(parent, timeout)
	w.running, w.cancel = true, cancel
	return ctx, func() {
		cancel()
		w.mu.Lock()
		w.running, w.cancel = false, nil
		w.mu.Unlock()
	}, nil
}

func (a *App) bucketClient(ctx context.Context, profileID, bucket string) (*s3.Client, error) {
	b, err := a.browser()
	if err != nil {
		return nil, err
	}
	c, err := b.BucketClient(ctx, devProfile(profileID), bucket)
	if err != nil {
		return nil, friendly(err)
	}
	return c, nil
}

// friendly turns an S3 or SDK error into a sentence; the errors this file and
// package objects make are already sentences.
func friendly(err error) error {
	if err == nil {
		return nil
	}
	for _, own := range []error{objects.ErrFolderName, objects.ErrFolderExists, objects.ErrTooMany, objects.ErrTagsChanged, objects.ErrCredentialsExpiring, browse.ErrSelectionChanged} {
		if errors.Is(err, own) {
			return err
		}
	}
	return errors.New(browse.Message(err))
}

func (a *App) emitObjects(p ObjectProgress) {
	if a.emitHook != nil {
		a.emitHook("s4:objects", p)
		return
	}
	a.emit("s4:objects", p)
}

// PrepareDelete counts the selection: the selected objects plus every object
// under the selected folders. It deletes nothing. Progress arrives as
// "s4:objects" events, and CancelObjectWork stops it.
func (a *App) PrepareDelete(sel ObjectSelection) (DeletePreview, error) {
	b, err := a.browser()
	if err != nil {
		return DeletePreview{}, err
	}
	got, err := b.Selection(sel.ListingID, browse.SortSpec{Field: browse.SortField(sel.Field), Desc: sel.Desc}, sel.Version, sel.Ranges)
	switch {
	case errors.Is(err, browse.ErrListingNotFound):
		return DeletePreview{}, errors.New("the folder was closed; open it and select the items again")
	case err != nil:
		return DeletePreview{}, err
	case len(got.Entries) == 0:
		return DeletePreview{}, errors.New("nothing is selected")
	case got.Bucket != sel.Bucket || got.Profile != devProfile(sel.ProfileID):
		return DeletePreview{}, errors.New("the selection belongs to another bucket or profile; select the items again")
	}
	ctx, done, err := a.objects.start(a.ctx, deleteTimeout)
	if err != nil {
		return DeletePreview{}, err
	}
	defer done()
	c, err := a.bucketClient(ctx, sel.ProfileID, sel.Bucket)
	if err != nil {
		return DeletePreview{}, err
	}
	items := make([]objects.Item, len(got.Entries))
	for i, e := range got.Entries {
		items[i] = objects.Item{Key: e.Key, Folder: e.Folder, Size: e.Size}
	}
	a.emitObjects(ObjectProgress{Phase: "counting"})
	plan, err := objects.PlanDelete(ctx, c, sel.Bucket, items, a.maxDelete, func(n int) {
		a.emitObjects(ObjectProgress{Phase: "counting", Done: n})
	})
	if err != nil {
		if ctx.Err() == context.Canceled {
			return DeletePreview{}, errors.New("counting was cancelled; nothing was deleted")
		}
		return DeletePreview{}, friendly(err)
	}
	if len(plan.Keys) == 0 {
		return DeletePreview{}, errors.New("the selected folders are empty in S3 now; reload the folder")
	}
	for _, key := range plan.Keys {
		if err := a.checkWrite(key); err != nil {
			return DeletePreview{}, err
		}
	}
	p := DeletePreview{
		Bucket: sel.Bucket, Prefix: got.Prefix, Objects: plan.Objects, Bytes: plan.Bytes, Folders: plan.Folders,
		Selected: len(got.Entries), Placeholders: plan.Placeholders, Sample: append([]string(nil), plan.Keys[:min(8, len(plan.Keys))]...),
	}
	p.Versioning, err = objects.Versioning(ctx, c, sel.Bucket)
	if err != nil {
		p.VersioningError = browse.Message(err)
	}
	p.Transfers = a.transfersInto(ctx, sel.Bucket, got.Entries)

	var id [12]byte
	_, _ = rand.Read(id[:])
	p.ID = hex.EncodeToString(id[:])
	a.objects.mu.Lock()
	if a.objects.plans == nil {
		a.objects.plans = map[string]pendingDelete{}
	}
	now := time.Now()
	for k, v := range a.objects.plans {
		if now.After(v.expires) {
			delete(a.objects.plans, k)
		}
	}
	a.objects.plans[p.ID] = pendingDelete{profile: sel.ProfileID, plan: plan, expires: now.Add(planLife)}
	a.objects.mu.Unlock()
	return p, nil
}

// transfersInto counts unfinished transfers on the selected keys or below the
// selected folders, so the confirmation can warn about them.
func (a *App) transfersInto(ctx context.Context, bucket string, entries []browse.Entry) int {
	q, err := a.transfers()
	if err != nil {
		return 0
	}
	jobs, err := q.Jobs(ctx)
	if err != nil {
		return 0
	}
	n := 0
	for _, j := range jobs {
		if j.Bucket != bucket || j.State == "completed" || j.State == "failed" || j.State == "cancelled" {
			continue
		}
		for _, e := range entries {
			if j.Key == e.Key || (e.Folder && strings.HasPrefix(j.Key, e.Key)) {
				n++
				break
			}
		}
	}
	return n
}

// RunDelete deletes the keys PrepareDelete counted. A plan runs once; an
// expired or unknown plan is refused, so a confirmation left open cannot
// delete what the folder holds later.
func (a *App) RunDelete(id string) (objects.Result, error) {
	a.objects.mu.Lock()
	pd, ok := a.objects.plans[id]
	delete(a.objects.plans, id)
	a.objects.mu.Unlock()
	if !ok || time.Now().After(pd.expires) {
		return objects.Result{}, errors.New("this delete was not confirmed in time; select the items and delete again")
	}
	ctx, done, err := a.objects.start(a.ctx, deleteTimeout)
	if err != nil {
		return objects.Result{}, err
	}
	defer done()
	c, err := a.bucketClient(ctx, pd.profile, pd.plan.Bucket)
	if err != nil {
		return objects.Result{}, err
	}
	total := len(pd.plan.Keys)
	a.emitObjects(ObjectProgress{Phase: "deleting", Total: total})
	res := pd.plan.Run(ctx, c, func(n int) {
		a.emitObjects(ObjectProgress{Phase: "deleting", Done: n, Total: total})
	})
	if res.Stopped != "" && res.Stopped != "Cancelled." {
		res.Stopped = friendly(errors.New(res.Stopped)).Error()
	}
	return res, nil
}

// DiscardDelete forgets a counted delete the user did not confirm.
func (a *App) DiscardDelete(id string) {
	a.objects.mu.Lock()
	delete(a.objects.plans, id)
	a.objects.mu.Unlock()
}

// CancelObjectWork stops the delete being counted or run. Batches S3 already
// accepted stay deleted.
func (a *App) CancelObjectWork() {
	a.objects.mu.Lock()
	defer a.objects.mu.Unlock()
	if a.objects.cancel != nil {
		a.objects.cancel()
	}
}

// CreateFolder makes an empty folder named name under prefix and returns its key.
func (a *App) CreateFolder(profileID, bucket, prefix, name string) (string, error) {
	ctx, cancel := context.WithTimeout(a.ctx, objectTimeout)
	defer cancel()
	c, err := a.bucketClient(ctx, profileID, bucket)
	if err != nil {
		return "", err
	}
	if key, err := objects.FolderKey(prefix, name); err == nil {
		if err := a.checkWrite(key); err != nil {
			return "", err
		}
	}
	key, err := objects.CreateFolder(ctx, c, bucket, prefix, name)
	return key, friendly(err)
}

// PresignObject signs a download link for one object that lasts the given
// number of seconds, or less: see objects.Presign. The link is not logged.
func (a *App) PresignObject(profileID, bucket, key string, seconds int64) (objects.Presigned, error) {
	ctx, cancel := context.WithTimeout(a.ctx, browseTimeout)
	defer cancel()
	c, err := a.bucketClient(ctx, profileID, bucket)
	if err != nil {
		return objects.Presigned{}, err
	}
	p, err := objects.Presign(ctx, c, bucket, key, time.Duration(seconds)*time.Second, time.Now())
	return p, friendly(err)
}

// ObjectMetadata reads one object's metadata.
func (a *App) ObjectMetadata(profileID, bucket, key string) (objects.Metadata, error) {
	ctx, cancel := context.WithTimeout(a.ctx, objectTimeout)
	defer cancel()
	c, err := a.bucketClient(ctx, profileID, bucket)
	if err != nil {
		return objects.Metadata{}, err
	}
	m, err := objects.Head(ctx, c, bucket, key)
	return m, friendly(err)
}

// ObjectTags reads one object's tags.
func (a *App) ObjectTags(profileID, bucket, key string) ([]objects.Tag, error) {
	ctx, cancel := context.WithTimeout(a.ctx, objectTimeout)
	defer cancel()
	c, err := a.bucketClient(ctx, profileID, bucket)
	if err != nil {
		return nil, err
	}
	t, err := objects.Tags(ctx, c, bucket, key)
	return t, friendly(err)
}

// SaveObjectTags replaces one object's tags. before is the set that was shown
// for editing: if the tags changed since, nothing is saved.
func (a *App) SaveObjectTags(profileID, bucket, key string, before, after []objects.Tag) ([]objects.Tag, error) {
	if err := a.checkWrite(key); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(a.ctx, objectTimeout)
	defer cancel()
	c, err := a.bucketClient(ctx, profileID, bucket)
	if err != nil {
		return nil, err
	}
	if err := objects.SetTags(ctx, c, bucket, key, before, after); err != nil {
		return nil, friendly(err)
	}
	t, err := objects.Tags(ctx, c, bucket, key)
	return t, friendly(err)
}
