package browse

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"

	"s4browser/internal/s3x"
)

// Delimiter separates folder levels in keys.
const Delimiter = "/"

// MaxWindow is the most rows one Window call returns.
const MaxWindow = 1000

// State is where a listing is in its life.
type State string

// Listing states. Loading is the only one that changes later.
const (
	StateLoading   State = "loading"
	StateDone      State = "done"
	StateError     State = "error"
	StateCancelled State = "cancelled"
)

// Snapshot describes a listing at one moment.
type Snapshot struct {
	ID      string `json:"id"`
	Profile string `json:"profile"`
	Bucket  string `json:"bucket"`
	Prefix  string `json:"prefix"`
	Region  string `json:"region,omitempty"`
	State   State  `json:"state"`
	// Count is the number of rows loaded so far: Folders + Objects.
	Count   int `json:"count"`
	Folders int `json:"folders"`
	Objects int `json:"objects"`
	Pages   int `json:"pages"`
	// Truncated is true when the listing stopped at Options.MaxRows.
	Truncated bool   `json:"truncated,omitempty"`
	Error     string `json:"error,omitempty"`
	// Version changes whenever rows are added or the state changes.
	Version   uint64  `json:"version"`
	ElapsedMs float64 `json:"elapsedMs"`
}

// SortField names a column to sort by.
type SortField string

// Sort fields.
const (
	SortName     SortField = "name"
	SortSize     SortField = "size"
	SortModified SortField = "modified"
	SortClass    SortField = "class"
)

// SortSpec is a sort order. Folders always come before objects, and rows that
// compare equal are ordered by name.
type SortSpec struct {
	Field SortField `json:"field"`
	Desc  bool      `json:"desc"`
}

// Row is one folder or object as the UI shows it.
type Row struct {
	Key string `json:"key"`
	// Name is the part of the key after the listing's prefix, without a folder's
	// trailing slash.
	Name         string     `json:"name"`
	Folder       bool       `json:"folder"`
	Size         int64      `json:"size"`
	Modified     *time.Time `json:"modified,omitempty"`
	StorageClass string     `json:"storageClass,omitempty"`
}

// Window is a slice of a listing's rows in one sort order.
type Window struct {
	Listing Snapshot `json:"listing"`
	Sort    SortSpec `json:"sort"`
	Offset  int      `json:"offset"`
	Rows    []Row    `json:"rows"`
}

// row is the compact form kept for every listed entry.
type row struct {
	key    string
	size   int64
	mod    int64 // Unix nanoseconds, 0 for folders
	class  uint8
	folder bool
}

// Listing loads one prefix of one bucket in the background.
type Listing struct {
	svc     *Service
	id      string
	profile string
	bucket  string
	prefix  string
	cancel  context.CancelFunc
	done    chan struct{}
	started time.Time

	mu        sync.Mutex
	region    string
	rows      []row
	folders   int
	classes   []string
	classIdx  map[string]uint8
	state     State
	err       string
	pages     int
	truncated bool
	version   uint64
	elapsed   time.Duration
	views     map[SortSpec][]int32
}

func newID() string {
	var b [12]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// OpenListing starts listing the folders and objects directly under prefix. An
// empty prefix is the bucket's top level. The listing runs until it is done,
// fails, or is closed.
func (s *Service) OpenListing(profile, bucket, prefix string) (Snapshot, error) {
	if _, err := parseProfile(profile); err != nil {
		return Snapshot{}, err
	}
	bucket, err := NormalizeBucketName(bucket)
	if err != nil {
		return Snapshot{}, err
	}
	if len(prefix) > 1024 {
		return Snapshot{}, fmt.Errorf("the prefix is longer than S3's 1024-byte key limit")
	}
	ctx, cancel := context.WithCancel(context.Background())
	l := &Listing{
		svc: s, id: newID(), profile: profile, bucket: bucket, prefix: prefix,
		cancel: cancel, done: make(chan struct{}), started: time.Now(),
		state: StateLoading, classIdx: map[string]uint8{}, views: map[SortSpec][]int32{},
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		cancel()
		return Snapshot{}, ErrClosed
	}
	s.listings[l.id] = l
	s.order = append(s.order, l.id)
	var evicted []*Listing
	for len(s.order) > s.opts.MaxListings {
		old := s.listings[s.order[0]]
		delete(s.listings, s.order[0])
		s.order = s.order[1:]
		evicted = append(evicted, old)
	}
	s.wg.Add(1)
	s.mu.Unlock()
	for _, old := range evicted {
		old.cancel()
	}
	go l.run(ctx)
	return l.snapshot(), nil
}

func (s *Service) listing(id string) (*Listing, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if l := s.listings[id]; l != nil {
		return l, nil
	}
	return nil, ErrListingNotFound
}

// CloseListing stops a listing and frees its rows. Closing an unknown ID is not
// an error.
func (s *Service) CloseListing(id string) {
	s.mu.Lock()
	l := s.listings[id]
	if l != nil {
		delete(s.listings, id)
		for i, x := range s.order {
			if x == id {
				s.order = append(s.order[:i], s.order[i+1:]...)
				break
			}
		}
	}
	s.mu.Unlock()
	if l != nil {
		l.cancel()
	}
}

// Snapshot returns a listing's current state.
func (s *Service) Snapshot(id string) (Snapshot, error) {
	l, err := s.listing(id)
	if err != nil {
		return Snapshot{}, err
	}
	return l.snapshot(), nil
}

// Wait blocks until the listing stops loading or ctx ends.
func (s *Service) Wait(ctx context.Context, id string) (Snapshot, error) {
	l, err := s.listing(id)
	if err != nil {
		return Snapshot{}, err
	}
	select {
	case <-l.done:
		return l.snapshot(), nil
	case <-ctx.Done():
		return l.snapshot(), ctx.Err()
	}
}

// Window returns up to limit rows starting at offset in the given order. It
// works while the listing is still loading: rows loaded so far are sorted, and
// later calls include rows that arrived since.
func (s *Service) Window(id string, spec SortSpec, offset, limit int) (Window, error) {
	l, err := s.listing(id)
	if err != nil {
		return Window{}, err
	}
	return l.window(spec, offset, limit)
}

func (l *Listing) run(ctx context.Context) {
	defer l.svc.wg.Done()
	defer close(l.done)
	region, err := l.svc.Region(ctx, l.profile, l.bucket)
	if err != nil {
		l.finish(ctx, err)
		return
	}
	client, err := l.svc.client(l.profile, region)
	if err != nil {
		l.finish(ctx, err)
		return
	}
	l.mu.Lock()
	l.region = region
	l.mu.Unlock()

	var token *string
	redirected := false
	lastNotify := time.Now()
	for {
		in := &s3.ListObjectsV2Input{
			Bucket:            aws.String(l.bucket),
			Delimiter:         aws.String(Delimiter),
			EncodingType:      types.EncodingTypeUrl,
			MaxKeys:           aws.Int32(l.svc.opts.PageSize),
			ContinuationToken: token,
		}
		if l.prefix != "" {
			in.Prefix = aws.String(l.prefix)
		}
		page, err := client.ListObjectsV2(ctx, in)
		if err != nil {
			// A cached region can be stale if the bucket was recreated
			// elsewhere. S3's redirect names the right one; follow it once.
			if hint := s3x.RegionHint(err); hint != "" && hint != region && !redirected {
				redirected = true
				region = hint
				l.svc.setRegion(l.bucket, region)
				if client, err = l.svc.client(l.profile, region); err == nil {
					l.mu.Lock()
					l.region = region
					l.mu.Unlock()
					continue
				}
			}
			l.finish(ctx, err)
			return
		}
		full, err := l.add(page)
		if err != nil {
			l.finish(ctx, err)
			return
		}
		if full || !aws.ToBool(page.IsTruncated) || aws.ToString(page.NextContinuationToken) == "" {
			l.finish(ctx, nil)
			return
		}
		token = page.NextContinuationToken
		if now := time.Now(); now.Sub(lastNotify) >= l.svc.opts.NotifyInterval {
			lastNotify = now
			l.notify()
		}
	}
}

// add appends one page and reports whether MaxRows was reached.
func (l *Listing) add(page *s3.ListObjectsV2Output) (bool, error) {
	decode := func(s *string) (string, error) { return aws.ToString(s), nil }
	if page.EncodingType == types.EncodingTypeUrl {
		decode = func(s *string) (string, error) { return url.QueryUnescape(aws.ToString(s)) }
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	limit := l.svc.opts.MaxRows
	for _, p := range page.CommonPrefixes {
		if len(l.rows) >= limit {
			l.truncated = true
			break
		}
		key, err := decode(p.Prefix)
		if err != nil {
			return false, fmt.Errorf("S3 returned a folder name that cannot be decoded: %w", err)
		}
		l.rows = append(l.rows, row{key: key, folder: true})
		l.folders++
	}
	for _, o := range page.Contents {
		if l.truncated || len(l.rows) >= limit {
			l.truncated = true
			break
		}
		key, err := decode(o.Key)
		if err != nil {
			return false, fmt.Errorf("S3 returned a key that cannot be decoded: %w", err)
		}
		if key == l.prefix {
			continue // the folder's own placeholder object
		}
		r := row{key: key, size: aws.ToInt64(o.Size), class: l.classLocked(string(o.StorageClass))}
		if o.LastModified != nil {
			r.mod = o.LastModified.UnixNano()
		}
		l.rows = append(l.rows, r)
	}
	l.pages++
	l.version++
	return l.truncated, nil
}

func (l *Listing) classLocked(class string) uint8 {
	if i, ok := l.classIdx[class]; ok {
		return i
	}
	if len(l.classes) == 255 {
		return 255 // more distinct classes than S3 has; keep the last slot
	}
	i := uint8(len(l.classes))
	l.classes = append(l.classes, class)
	l.classIdx[class] = i
	return i
}

func (l *Listing) finish(ctx context.Context, err error) {
	l.mu.Lock()
	switch {
	case err == nil:
		l.state = StateDone
	case ctx.Err() != nil:
		l.state = StateCancelled
	default:
		l.state = StateError
		l.err = Message(err)
	}
	l.elapsed = time.Since(l.started)
	l.version++
	l.mu.Unlock()
	if l.state != StateCancelled {
		l.notify()
	}
}

func (l *Listing) notify() {
	if l.svc.opts.Notify != nil {
		l.svc.opts.Notify(l.snapshot())
	}
}

func (l *Listing) snapshot() Snapshot {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.snapshotLocked()
}

func (l *Listing) snapshotLocked() Snapshot {
	elapsed := l.elapsed
	if l.state == StateLoading {
		elapsed = time.Since(l.started)
	}
	return Snapshot{
		ID: l.id, Profile: l.profile, Bucket: l.bucket, Prefix: l.prefix, Region: l.region,
		State: l.state, Count: len(l.rows), Folders: l.folders, Objects: len(l.rows) - l.folders,
		Pages: l.pages, Truncated: l.truncated, Error: l.err, Version: l.version,
		ElapsedMs: float64(elapsed.Microseconds()) / 1000,
	}
}

func (l *Listing) window(spec SortSpec, offset, limit int) (Window, error) {
	switch spec.Field {
	case SortName, SortSize, SortModified, SortClass:
	case "":
		spec.Field = SortName
	default:
		return Window{}, fmt.Errorf("unknown sort field %q", spec.Field)
	}
	if offset < 0 {
		offset = 0
	}
	limit = min(max(limit, 0), MaxWindow)
	l.mu.Lock()
	defer l.mu.Unlock()
	perm := l.viewLocked(spec)
	w := Window{Listing: l.snapshotLocked(), Sort: spec, Offset: offset, Rows: []Row{}}
	end := min(offset+limit, len(perm))
	for _, i := range perm[min(offset, end):end] {
		w.Rows = append(w.Rows, l.rowLocked(&l.rows[i]))
	}
	return w, nil
}

func (l *Listing) rowLocked(r *row) Row {
	name := strings.TrimPrefix(r.key, l.prefix)
	out := Row{Key: r.key, Folder: r.folder, Size: r.size}
	if r.folder {
		if trimmed := strings.TrimSuffix(name, Delimiter); trimmed != "" {
			name = trimmed
		}
	} else {
		t := time.Unix(0, r.mod).UTC()
		out.Modified = &t
		out.StorageClass = l.classes[r.class]
	}
	out.Name = name
	return out
}

// Summary totals a selection made of index ranges in one sort order.
type Summary struct {
	Count   int   `json:"count"`
	Folders int   `json:"folders"`
	Objects int   `json:"objects"`
	Bytes   int64 `json:"bytes"`
}

// Summarize totals the rows in the inclusive index ranges [from, to] of the
// given sort order. Ranges are clamped to the listing, so a selection made
// while the listing was smaller stays valid.
func (s *Service) Summarize(id string, spec SortSpec, ranges [][2]int) (Summary, error) {
	l, err := s.listing(id)
	if err != nil {
		return Summary{}, err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	perm := l.viewLocked(spec)
	var out Summary
	for _, r := range ranges {
		from, to := max(r[0], 0), min(r[1], len(perm)-1)
		for _, i := range perm[min(from, to+1) : max(to+1, from)] {
			row := &l.rows[i]
			out.Count++
			if row.folder {
				out.Folders++
			} else {
				out.Objects++
				out.Bytes += row.size
			}
		}
	}
	return out, nil
}

// ErrSelectionChanged means a selection was made on an earlier version of a
// listing, so its index ranges may no longer name the same rows.
var ErrSelectionChanged = errors.New("the folder changed after the selection was made; select the items again")

// Keys returns the keys of the rows in the inclusive index ranges of the given
// sort order, in that order. Folder keys end in "/". The selection must have
// been made on this version of the listing: index ranges of an older version
// can name other rows, so a different version is ErrSelectionChanged.
func (s *Service) Keys(id string, spec SortSpec, version uint64, ranges [][2]int) ([]string, error) {
	switch spec.Field {
	case SortName, SortSize, SortModified, SortClass:
	case "":
		spec.Field = SortName
	default:
		return nil, fmt.Errorf("unknown sort field %q", spec.Field)
	}
	l, err := s.listing(id)
	if err != nil {
		return nil, err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.version != version {
		return nil, ErrSelectionChanged
	}
	perm := l.viewLocked(spec)
	var keys []string
	last := -1
	for _, r := range ranges {
		from, to := max(r[0], 0, last+1), min(r[1], len(perm)-1)
		for _, i := range perm[min(from, to+1) : max(to+1, from)] {
			keys = append(keys, l.rows[i].key)
		}
		last = max(last, to)
	}
	return keys, nil
}
