package objects

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// Versioning states as the delete confirmation needs them.
const (
	VersioningOff       = "off"
	VersioningEnabled   = "enabled"
	VersioningSuspended = "suspended"
	VersioningUnknown   = "unknown"
)

// Versioning reports whether deletes in bucket leave older versions behind.
// When the profile may not ask, the state is VersioningUnknown and the error
// says why.
func Versioning(ctx context.Context, c *s3.Client, bucket string) (string, error) {
	out, err := c.GetBucketVersioning(ctx, &s3.GetBucketVersioningInput{Bucket: aws.String(bucket)})
	if err != nil {
		return VersioningUnknown, err
	}
	switch out.Status {
	case types.BucketVersioningStatusEnabled:
		return VersioningEnabled, nil
	case types.BucketVersioningStatusSuspended:
		return VersioningSuspended, nil
	}
	return VersioningOff, nil
}

// Item is one selected row to delete. A folder is deleted with everything
// under its key.
type Item struct {
	Key    string
	Folder bool
	Size   int64
}

// DefaultMaxDelete bounds how many objects one delete may expand to, so the key
// list stays in memory comfortably.
const DefaultMaxDelete = 1_000_000

// ErrTooMany means the selection holds more objects than one delete allows.
var ErrTooMany = errors.New("the selection holds too many objects to delete at once")

// Plan is a delete that has been counted and not yet run.
type Plan struct {
	Bucket  string
	Keys    []string
	Objects int
	Bytes   int64
	Folders int
	// Placeholders counts zero-byte folder placeholder objects among Keys.
	Placeholders int
}

// PlanDelete lists every object under the selected folders and adds the
// selected objects. Keys are deduplicated. progress, if set, is called with the
// number of objects found so far after each listing page. max <= 0 means
// DefaultMaxDelete.
func PlanDelete(ctx context.Context, c *s3.Client, bucket string, items []Item, max int, progress func(found int)) (*Plan, error) {
	if max <= 0 {
		max = DefaultMaxDelete
	}
	p := &Plan{Bucket: bucket}
	seen := map[string]bool{}
	add := func(key string, size int64) error {
		if seen[key] {
			return nil
		}
		if len(p.Keys) >= max {
			return fmt.Errorf("%w: more than %d", ErrTooMany, max)
		}
		seen[key] = true
		p.Keys = append(p.Keys, key)
		p.Objects++
		p.Bytes += size
		if size == 0 && strings.HasSuffix(key, "/") {
			p.Placeholders++
		}
		return nil
	}
	for _, it := range items {
		if !it.Folder {
			if err := add(it.Key, it.Size); err != nil {
				return nil, err
			}
			continue
		}
		if !strings.HasSuffix(it.Key, "/") {
			return nil, fmt.Errorf("folder key %q does not end in /", it.Key)
		}
		p.Folders++
		pages := s3.NewListObjectsV2Paginator(c, &s3.ListObjectsV2Input{Bucket: aws.String(bucket), Prefix: aws.String(it.Key)})
		for pages.HasMorePages() {
			page, err := pages.NextPage(ctx)
			if err != nil {
				return nil, err
			}
			for _, o := range page.Contents {
				key := aws.ToString(o.Key)
				if !strings.HasPrefix(key, it.Key) {
					return nil, fmt.Errorf("S3 listed %q outside the folder %q", key, it.Key)
				}
				if err := add(key, aws.ToInt64(o.Size)); err != nil {
					return nil, err
				}
			}
			if progress != nil {
				progress(len(p.Keys))
			}
		}
	}
	return p, nil
}

// MaxListedFailures caps the failures a Result lists; FailedTotal stays exact.
const MaxListedFailures = 200

// Failure is one object S3 did not delete.
type Failure struct {
	Key     string `json:"key"`
	Code    string `json:"code,omitempty"`
	Message string `json:"message"`
}

// Result says what a delete did.
type Result struct {
	Requested int `json:"requested"`
	Deleted   int `json:"deleted"`
	// Markers counts deletes that left a delete marker (a versioned bucket).
	Markers     int       `json:"markers"`
	Failed      []Failure `json:"failed"`
	FailedTotal int       `json:"failedTotal"`
	// NotTried counts keys never sent because the delete stopped.
	NotTried int `json:"notTried"`
	// Stopped says why the delete ended early: cancelled, or a whole request
	// failed.
	Stopped string `json:"stopped,omitempty"`
}

func (r *Result) fail(f Failure) {
	r.FailedTotal++
	if len(r.Failed) < MaxListedFailures {
		r.Failed = append(r.Failed, f)
	}
}

// batchSize is S3's limit for one DeleteObjects call.
const batchSize = 1000

// Run deletes the plan's keys in batches of 1,000. A key S3 refuses is listed
// and the delete goes on; a failed request (after the SDK's retries) or a
// cancelled ctx stops it, and the keys not yet sent are counted as NotTried.
// progress, if set, gets the count of keys handled after each batch.
func (p *Plan) Run(ctx context.Context, c *s3.Client, progress func(done int)) Result {
	res := Result{Requested: len(p.Keys), Failed: []Failure{}}
	for start := 0; start < len(p.Keys); start += batchSize {
		if err := ctx.Err(); err != nil {
			res.NotTried = len(p.Keys) - start
			res.Stopped = "Cancelled."
			return res
		}
		batch := p.Keys[start:min(start+batchSize, len(p.Keys))]
		ids := make([]types.ObjectIdentifier, len(batch))
		for i, k := range batch {
			ids[i] = types.ObjectIdentifier{Key: aws.String(k)}
		}
		out, err := c.DeleteObjects(ctx, &s3.DeleteObjectsInput{Bucket: aws.String(p.Bucket), Delete: &types.Delete{Objects: ids}})
		if err != nil {
			// The batch may or may not have been applied: S3 does not say.
			res.NotTried = len(p.Keys) - start
			res.Stopped = err.Error()
			if ctx.Err() != nil {
				res.Stopped = "Cancelled."
			}
			return res
		}
		for _, d := range out.Deleted {
			res.Deleted++
			if aws.ToBool(d.DeleteMarker) {
				res.Markers++
			}
		}
		for _, e := range out.Errors {
			res.fail(Failure{Key: aws.ToString(e.Key), Code: aws.ToString(e.Code), Message: aws.ToString(e.Message)})
		}
		if progress != nil {
			progress(start + len(batch))
		}
	}
	return res
}
