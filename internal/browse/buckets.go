// Package browse lists buckets and objects for the UI. Go owns every listing:
// pages arrive from S3 in the background, rows are kept here, and the UI asks
// for sorted windows of rows, so 100,000 rows never reach the webview at once.
package browse

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"s4browser/internal/engine"
	"s4browser/internal/s3x"
)

// Options configures a Service.
type Options struct {
	// Dir holds buckets.json, the buckets added by name. Created when needed.
	Dir string
	// Credentials returns a profile's refreshing provider, normally
	// creds.Manager.Provider.
	Credentials func(profileID string) (aws.CredentialsProvider, error)
	// ClientOptions are applied to every S3 client after the defaults. Tests use
	// them to reach a fake endpoint.
	ClientOptions []func(*s3.Options)
	// Notify receives listing progress, at most every NotifyInterval per listing
	// and once when a listing stops. It must not block.
	Notify func(Snapshot)
	// NotifyInterval defaults to 200 ms, about 5 updates a second.
	NotifyInterval time.Duration
	// MaxRows stops a listing once it holds this many rows. Default 1,000,000.
	MaxRows int
	// MaxListings is how many listings may be open; opening one more closes the
	// oldest. Default 16.
	MaxListings int
	// PageSize is the MaxKeys of each ListObjectsV2 call. Default 1000.
	PageSize int32
}

// Defaults for Options.
const (
	DefaultNotifyInterval = 200 * time.Millisecond
	DefaultMaxRows        = 1_000_000
	DefaultMaxListings    = 16
	DefaultPageSize       = 1000
)

// Bucket is one entry of the bucket list.
type Bucket struct {
	Name string `json:"name"`
	// Region is empty until it is known.
	Region string `json:"region,omitempty"`
	// Listed is true if ListBuckets returned it, Added if the user added it by name.
	Listed  bool       `json:"listed"`
	Added   bool       `json:"added"`
	Created *time.Time `json:"created,omitempty"`
}

// BucketList is the result of Service.Buckets.
type BucketList struct {
	Buckets []Bucket `json:"buckets"`
	// ListError says why ListBuckets failed. Added buckets are still returned.
	ListError string `json:"listError,omitempty"`
	// ListDenied is true when the profile may not list buckets, so the user
	// should add buckets by name.
	ListDenied bool `json:"listDenied,omitempty"`
}

// Errors for bad input.
var (
	ErrInvalidBucketName = errors.New("not a valid bucket name")
	ErrListingNotFound   = errors.New("the listing is closed")
	ErrClosed            = errors.New("the browse service is closed")
)

type clientKey struct{ profile, region string }

// Service lists buckets and objects for any profile. It is safe for concurrent use.
type Service struct {
	opts  Options
	saved *savedBuckets

	mu       sync.Mutex
	clients  map[clientKey]*s3.Client
	regions  map[string]string // bucket name -> region; regions are global
	listings map[string]*Listing
	order    []string // listing IDs, oldest first
	closed   bool
	wg       sync.WaitGroup
}

// New opens the service and its saved bucket list.
func New(o Options) (*Service, error) {
	if o.Credentials == nil {
		return nil, errors.New("browse: Options.Credentials is required")
	}
	if o.NotifyInterval <= 0 {
		o.NotifyInterval = DefaultNotifyInterval
	}
	if o.MaxRows <= 0 {
		o.MaxRows = DefaultMaxRows
	}
	if o.MaxListings <= 0 {
		o.MaxListings = DefaultMaxListings
	}
	if o.PageSize <= 0 || o.PageSize > 1000 {
		o.PageSize = DefaultPageSize
	}
	saved, err := openSaved(o.Dir)
	if err != nil {
		return nil, err
	}
	s := &Service{
		opts: o, saved: saved,
		clients:  map[clientKey]*s3.Client{},
		regions:  map[string]string{},
		listings: map[string]*Listing{},
	}
	for _, list := range saved.state.Profiles {
		for _, b := range list {
			if b.Region != "" {
				s.regions[b.Name] = b.Region
			}
		}
	}
	return s, nil
}

// Close cancels every listing and waits for them to stop.
func (s *Service) Close() {
	s.mu.Lock()
	s.closed = true
	for _, l := range s.listings {
		l.cancel()
	}
	s.listings = map[string]*Listing{}
	s.order = nil
	s.mu.Unlock()
	s.wg.Wait()
}

func (s *Service) client(profile, region string) (*s3.Client, error) {
	if region == "" {
		region = s3x.DefaultRegion
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	k := clientKey{profile, region}
	if c := s.clients[k]; c != nil {
		return c, nil
	}
	provider, err := s.opts.Credentials(profile)
	if err != nil {
		return nil, err
	}
	c := s3x.NewBrowseClient(region, provider, s.opts.ClientOptions...)
	s.clients[k] = c
	return c, nil
}

func (s *Service) cachedRegion(bucket string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.regions[bucket]
}

func (s *Service) setRegion(bucket, region string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.regions[bucket] = region
}

// Buckets returns the buckets ListBuckets reports, merged with the ones added
// by name, sorted by name. A ListBuckets failure is reported in the result, not
// as an error, because added buckets can still be browsed.
func (s *Service) Buckets(ctx context.Context, profile string) (BucketList, error) {
	if _, err := parseProfile(profile); err != nil {
		return BucketList{}, err
	}
	out := BucketList{Buckets: []Bucket{}}
	byName := map[string]int{}
	listed, err := s.listBuckets(ctx, profile)
	if err != nil {
		if ctx.Err() != nil {
			return BucketList{}, ctx.Err()
		}
		out.ListError = Message(err)
		out.ListDenied = isAccessDenied(err)
	}
	for _, b := range listed {
		byName[b.Name] = len(out.Buckets)
		out.Buckets = append(out.Buckets, b)
	}
	for _, a := range s.saved.list(profile) {
		if i, ok := byName[a.Name]; ok {
			out.Buckets[i].Added = true
			if out.Buckets[i].Region == "" {
				out.Buckets[i].Region = a.Region
			}
			continue
		}
		out.Buckets = append(out.Buckets, Bucket{Name: a.Name, Region: a.Region, Added: true})
	}
	slices.SortFunc(out.Buckets, func(a, b Bucket) int { return strings.Compare(a.Name, b.Name) })
	return out, nil
}

func (s *Service) listBuckets(ctx context.Context, profile string) ([]Bucket, error) {
	c, err := s.client(profile, s3x.DefaultRegion)
	if err != nil {
		return nil, err
	}
	var out []Bucket
	// Any parameter makes S3 report each bucket's region, which saves a
	// HeadBucket per bucket.
	in := &s3.ListBucketsInput{MaxBuckets: aws.Int32(1000)}
	p := s3.NewListBucketsPaginator(c, in)
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return out, err
		}
		for _, b := range page.Buckets {
			entry := Bucket{Name: aws.ToString(b.Name), Region: aws.ToString(b.BucketRegion), Listed: true, Created: b.CreationDate}
			if entry.Region != "" {
				s.setRegion(entry.Name, entry.Region)
			} else {
				entry.Region = s.cachedRegion(entry.Name)
			}
			out = append(out, entry)
		}
	}
	return out, nil
}

// AddResult is the result of AddBucket.
type AddResult struct {
	Bucket Bucket `json:"bucket"`
	// Note explains a choice the user should know about, such as a detected
	// region that differs from the one entered.
	Note string `json:"note,omitempty"`
}

// AddBucket remembers a bucket for a profile that may not be able to list all
// buckets. The region is detected. region is used only when detection is not
// possible, and is otherwise compared with the detected one. A bucket that S3
// reports as missing is refused. Adding a bucket again updates its region.
func (s *Service) AddBucket(ctx context.Context, profile, name, region string) (AddResult, error) {
	if _, err := parseProfile(profile); err != nil {
		return AddResult{}, err
	}
	name, err := NormalizeBucketName(name)
	if err != nil {
		return AddResult{}, err
	}
	region = strings.TrimSpace(region)
	if region != "" && !regionPattern.MatchString(region) {
		return AddResult{}, fmt.Errorf("%q is not a valid region name", region)
	}
	c, err := s.client(profile, s3x.DefaultRegion)
	if err != nil {
		return AddResult{}, err
	}
	res := AddResult{}
	detected, err := s3x.DetectRegion(ctx, c, name)
	switch {
	case err == nil:
		if region != "" && region != detected {
			res.Note = fmt.Sprintf("The bucket is in %s, not %s. %s is used.", detected, region, detected)
		}
		region = detected
	case errors.Is(err, s3x.ErrNoSuchBucket):
		return AddResult{}, fmt.Errorf("bucket %q does not exist", name)
	case errors.Is(err, s3x.ErrRegionUnknown) && region != "":
		res.Note = fmt.Sprintf("The region could not be detected, so %s is used as entered.", region)
	case errors.Is(err, s3x.ErrRegionUnknown):
		return AddResult{}, fmt.Errorf("the region of bucket %q could not be detected; enter its region", name)
	default:
		return AddResult{}, errors.New(Message(err))
	}
	if err := s.saved.add(profile, savedBucket{Name: name, Region: region}); err != nil {
		return AddResult{}, err
	}
	s.setRegion(name, region)
	res.Bucket = Bucket{Name: name, Region: region, Added: true}
	return res, nil
}

// RemoveBucket forgets a bucket added by name. It does not touch S3.
func (s *Service) RemoveBucket(profile, name string) error {
	if _, err := parseProfile(profile); err != nil {
		return err
	}
	return s.saved.remove(profile, name)
}

// Region returns a bucket's region, detecting and caching it when unknown.
func (s *Service) Region(ctx context.Context, profile, bucket string) (string, error) {
	if r := s.cachedRegion(bucket); r != "" {
		return r, nil
	}
	c, err := s.client(profile, s3x.DefaultRegion)
	if err != nil {
		return "", err
	}
	r, err := s3x.DetectRegion(ctx, c, bucket)
	if err != nil {
		return "", err
	}
	s.setRegion(bucket, r)
	return r, nil
}

var (
	// Lenient on purpose: buckets created in us-east-1 before 2018 may hold
	// capitals and underscores. S3 itself rejects anything else.
	bucketPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{1,254}$`)
	regionPattern = regexp.MustCompile(`^[a-z]{2}(-[a-z0-9]+)+-\d+$`)
)

// NormalizeBucketName trims an entered bucket name, accepting "s3://name" and a
// trailing slash, and checks it.
func NormalizeBucketName(name string) (string, error) {
	name = strings.TrimSpace(name)
	name = strings.TrimPrefix(name, "s3://")
	name = strings.TrimSuffix(name, "/")
	if !bucketPattern.MatchString(name) || strings.Contains(name, "..") {
		return "", fmt.Errorf("%w: %q", ErrInvalidBucketName, name)
	}
	return name, nil
}

func parseProfile(id string) (string, error) {
	if id == "" {
		return "", errors.New("no profile is selected")
	}
	return id, nil
}

func isAccessDenied(err error) bool {
	code := s3x.ErrorCode(err)
	return code == "AccessDenied" || code == "AllAccessDisabled" || s3x.HTTPStatus(err) == http.StatusForbidden
}

// Message turns an error from browsing into a sentence for the UI. It never
// includes credential values, because the SDK's errors hold none.
func Message(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, context.Canceled):
		return "Cancelled."
	case errors.Is(err, context.DeadlineExceeded):
		return "S3 did not answer in time."
	case errors.Is(err, engine.ErrCredentialsUnavailable):
		return "The profile's credentials could not be loaded: " + err.Error()
	case errors.Is(err, s3x.ErrNoSuchBucket), s3x.ErrorCode(err) == "NoSuchBucket":
		return "The bucket does not exist."
	case s3x.ErrorCode(err) == "ExpiredToken" || s3x.ErrorCode(err) == "TokenRefreshRequired":
		return "The profile's session has expired. Sign in again, then retry."
	case s3x.ErrorCode(err) == "InvalidAccessKeyId" || s3x.ErrorCode(err) == "SignatureDoesNotMatch":
		return "S3 rejected the profile's keys (" + s3x.ErrorCode(err) + ")."
	case isAccessDenied(err):
		return "Access denied. The profile is not allowed to do this here."
	}
	return err.Error()
}
