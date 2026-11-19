package browse

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"s4browser/internal/engine"
	"s4browser/internal/fakes3"
	"s4browser/internal/s3x"
)

const testProfile = "app:test"

type env struct {
	t     *testing.T
	fake  *fakes3.Server
	svc   *Service
	dir   string
	notes chan Snapshot
}

type envOpts struct {
	fake    fakes3.Options
	service func(*Options)
}

// newEnv starts a fake S3 with buckets in three regions and a Service that
// reaches it over HTTPS with the real SDK.
func newEnv(t *testing.T, eo envOpts) *env {
	t.Helper()
	baseline := runtime.NumGoroutine()
	if eo.fake.BucketRegions == nil {
		eo.fake.BucketRegions = map[string]string{
			"east-bucket": "us-east-1",
			"west-bucket": "us-west-2",
			"eu-bucket":   "eu-west-1",
		}
	}
	fake, err := fakes3.New(eo.fake)
	if err != nil {
		t.Fatal(err)
	}
	e := &env{t: t, fake: fake, dir: t.TempDir(), notes: make(chan Snapshot, 10000)}
	o := Options{
		Dir:           e.dir,
		Credentials:   fakeCredentials,
		ClientOptions: []func(*s3.Options){fakeEndpoint(fake)},
		Notify: func(s Snapshot) {
			select {
			case e.notes <- s:
			default:
			}
		},
		NotifyInterval: time.Millisecond,
	}
	if eo.service != nil {
		eo.service(&o)
	}
	e.svc, err = New(o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		e.svc.Close()
		fake.Close()
		assertNoLeak(t, baseline)
	})
	return e
}

func fakeCredentials(string) (aws.CredentialsProvider, error) {
	return aws.NewCredentialsCache(s3x.MarkCredentialErrors(
		credentials.NewStaticCredentialsProvider(fakes3.AccessKeyID, fakes3.SecretAccessKey, ""))), nil
}

func fakeEndpoint(fake *fakes3.Server) func(*s3.Options) {
	return func(o *s3.Options) {
		o.BaseEndpoint = aws.String(fake.Endpoint())
		o.UsePathStyle = true
		o.HTTPClient = fake.HTTPClient()
	}
}

func assertNoLeak(t *testing.T, baseline int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for runtime.NumGoroutine() > baseline {
		if time.Now().After(deadline) {
			buf := make([]byte, 1<<20)
			t.Fatalf("goroutines: %d, baseline %d\n%s", runtime.NumGoroutine(), baseline, buf[:runtime.Stack(buf, true)])
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func (e *env) requests(op fakes3.Op) []fakes3.RequestRecord {
	var out []fakes3.RequestRecord
	for _, r := range e.fake.Requests() {
		if r.Op == op {
			out = append(out, r)
		}
	}
	return out
}

func (e *env) open(bucket, prefix string) Snapshot {
	e.t.Helper()
	snap, err := e.svc.OpenListing(testProfile, bucket, prefix)
	if err != nil {
		e.t.Fatal(err)
	}
	return snap
}

func (e *env) wait(id string) Snapshot {
	e.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	snap, err := e.svc.Wait(ctx, id)
	if err != nil {
		e.t.Fatal(err)
	}
	return snap
}

func (e *env) all(id string, spec SortSpec) []Row {
	e.t.Helper()
	var rows []Row
	for offset := 0; ; offset += MaxWindow {
		w, err := e.svc.Window(id, spec, offset, MaxWindow)
		if err != nil {
			e.t.Fatal(err)
		}
		rows = append(rows, w.Rows...)
		if len(w.Rows) < MaxWindow {
			return rows
		}
	}
}

func TestBucketsComeWithTheirRegions(t *testing.T) {
	e := newEnv(t, envOpts{})
	list, err := e.svc.Buckets(context.Background(), testProfile)
	if err != nil {
		t.Fatal(err)
	}
	if list.ListError != "" {
		t.Fatalf("ListError = %q", list.ListError)
	}
	got := map[string]string{}
	for _, b := range list.Buckets {
		if !b.Listed || b.Added || b.Created == nil {
			t.Errorf("%+v: want listed, not added, with a creation date", b)
		}
		got[b.Name] = b.Region
	}
	want := map[string]string{"east-bucket": "us-east-1", "west-bucket": "us-west-2", "eu-bucket": "eu-west-1"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("buckets = %v, want %v", got, want)
	}
	if n := len(e.requests(fakes3.OpHeadBucket)); n != 0 {
		t.Errorf("%d HeadBucket calls; ListBuckets should have supplied every region", n)
	}
	// Listing a bucket in another region goes straight to that region.
	snap := e.wait(e.open("eu-bucket", "").ID)
	if snap.State != StateDone || snap.Region != "eu-west-1" {
		t.Fatalf("snapshot = %+v", snap)
	}
	for _, r := range e.requests(fakes3.OpListObjects) {
		if r.Status != 200 {
			t.Errorf("ListObjectsV2 answered %d %s", r.Status, r.Code)
		}
	}
}

func TestListBucketsPages(t *testing.T) {
	regions := map[string]string{}
	for i := range 2345 {
		regions[fmt.Sprintf("bucket-%05d", i)] = "us-west-2"
	}
	e := newEnv(t, envOpts{fake: fakes3.Options{BucketRegions: regions}})
	list, err := e.svc.Buckets(context.Background(), testProfile)
	if err != nil {
		t.Fatal(err)
	}
	if len(list.Buckets) != 2345 {
		t.Fatalf("%d buckets, want 2345", len(list.Buckets))
	}
	for i, b := range list.Buckets {
		if want := fmt.Sprintf("bucket-%05d", i); b.Name != want || b.Region != "us-west-2" {
			t.Fatalf("bucket %d = %+v, want %s", i, b, want)
		}
	}
	if n := len(e.requests(fakes3.OpListBuckets)); n != 3 {
		t.Errorf("%d ListBuckets pages, want 3", n)
	}
}

func TestRegionDetection(t *testing.T) {
	cases := []struct {
		name   string
		hide   bool
		rules  []fakes3.Rule
		bucket string
		want   string
		err    error
	}{
		{name: "head 200 in us-east-1", bucket: "east-bucket", want: "us-east-1"},
		{name: "head 301 names the region", bucket: "west-bucket", want: "us-west-2"},
		{name: "head 403 still names the region", bucket: "eu-bucket", want: "eu-west-1",
			rules: []fakes3.Rule{{Op: fakes3.OpHeadBucket, Fault: fakes3.AccessDenied()}}},
		{name: "no header, falls back to GetBucketLocation", hide: true, bucket: "west-bucket", want: "us-west-2"},
		{name: "no header, empty location is us-east-1", hide: true, bucket: "east-bucket", want: "us-east-1"},
		{name: "no header, location denied", hide: true, bucket: "eu-bucket", err: s3x.ErrRegionUnknown,
			rules: []fakes3.Rule{{Op: fakes3.OpLocation, Fault: fakes3.AccessDenied()}}},
		{name: "missing bucket", bucket: "no-such-bucket", err: s3x.ErrNoSuchBucket},
		{name: "missing bucket, no header", hide: true, bucket: "no-such-bucket", err: s3x.ErrNoSuchBucket},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t, envOpts{fake: fakes3.Options{HideBucketRegion: tc.hide}})
			for _, r := range tc.rules {
				e.fake.AddRule(r)
			}
			c, err := e.svc.client(testProfile, s3x.DefaultRegion)
			if err != nil {
				t.Fatal(err)
			}
			got, err := s3x.DetectRegion(context.Background(), c, tc.bucket)
			if tc.err != nil {
				if !errors.Is(err, tc.err) {
					t.Fatalf("err = %v, want %v", err, tc.err)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("DetectRegion = %q, %v; want %q", got, err, tc.want)
			}
			// When S3 sends the header, it is enough: no second request.
			if n := len(e.requests(fakes3.OpLocation)); !tc.hide && n != 0 {
				t.Errorf("%d GetBucketLocation calls, want 0", n)
			}
		})
	}
}

func TestAddBucketByNameWhenListingIsDenied(t *testing.T) {
	e := newEnv(t, envOpts{})
	e.fake.AddRule(fakes3.Rule{Op: fakes3.OpListBuckets, Fault: fakes3.AccessDenied()})
	ctx := context.Background()

	list, err := e.svc.Buckets(ctx, testProfile)
	if err != nil {
		t.Fatal(err)
	}
	if !list.ListDenied || list.ListError == "" || len(list.Buckets) != 0 {
		t.Fatalf("list = %+v, want denied and empty", list)
	}

	// An entered region that is wrong is replaced by the detected one.
	res, err := e.svc.AddBucket(ctx, testProfile, " s3://west-bucket/ ", "eu-west-1")
	if err != nil {
		t.Fatal(err)
	}
	if res.Bucket.Name != "west-bucket" || res.Bucket.Region != "us-west-2" || !strings.Contains(res.Note, "us-west-2") {
		t.Fatalf("AddBucket = %+v", res)
	}
	if _, err := e.svc.AddBucket(ctx, testProfile, "no-such-bucket", ""); err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("missing bucket: err = %v", err)
	}
	if _, err := e.svc.AddBucket(ctx, testProfile, "bad/name", ""); !errors.Is(err, ErrInvalidBucketName) {
		t.Fatalf("bad name: err = %v", err)
	}

	list, err = e.svc.Buckets(ctx, testProfile)
	if err != nil {
		t.Fatal(err)
	}
	if !list.ListDenied || len(list.Buckets) != 1 || list.Buckets[0] != (Bucket{Name: "west-bucket", Region: "us-west-2", Added: true}) {
		t.Fatalf("list = %+v", list)
	}
	// Another profile does not see it.
	other, err := e.svc.Buckets(ctx, "aws:other")
	if err != nil {
		t.Fatal(err)
	}
	if len(other.Buckets) != 0 {
		t.Fatalf("other profile sees %+v", other.Buckets)
	}

	// The list survives a restart, and the saved region is used without a lookup.
	e.svc.Close()
	svc2, err := New(Options{Dir: e.dir, Credentials: fakeCredentials, ClientOptions: []func(*s3.Options){fakeEndpoint(e.fake)}})
	if err != nil {
		t.Fatal(err)
	}
	defer svc2.Close()
	heads := len(e.requests(fakes3.OpHeadBucket))
	snap, err := svc2.OpenListing(testProfile, "west-bucket", "")
	if err != nil {
		t.Fatal(err)
	}
	if snap, err = svc2.Wait(ctx, snap.ID); err != nil || snap.State != StateDone || snap.Region != "us-west-2" {
		t.Fatalf("listing = %+v, %v", snap, err)
	}
	if n := len(e.requests(fakes3.OpHeadBucket)); n != heads {
		t.Errorf("%d new HeadBucket calls after restart, want 0", n-heads)
	}
	if err := svc2.RemoveBucket(testProfile, "west-bucket"); err != nil {
		t.Fatal(err)
	}
	if err := svc2.RemoveBucket(testProfile, "west-bucket"); err == nil {
		t.Fatal("second remove succeeded")
	}
	data, err := os.ReadFile(filepath.Join(e.dir, savedFileName))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "west-bucket") {
		t.Fatalf("removed bucket still saved: %s", data)
	}
}

func TestAddBucketUsesEnteredRegionOnlyWhenDetectionFails(t *testing.T) {
	e := newEnv(t, envOpts{fake: fakes3.Options{HideBucketRegion: true}})
	e.fake.AddRule(fakes3.Rule{Op: fakes3.OpLocation, Fault: fakes3.AccessDenied()})
	ctx := context.Background()
	if _, err := e.svc.AddBucket(ctx, testProfile, "west-bucket", ""); err == nil || !strings.Contains(err.Error(), "enter its region") {
		t.Fatalf("err = %v, want a request for the region", err)
	}
	if _, err := e.svc.AddBucket(ctx, testProfile, "west-bucket", "not a region"); err == nil {
		t.Fatal("invalid region accepted")
	}
	res, err := e.svc.AddBucket(ctx, testProfile, "west-bucket", "us-west-2")
	if err != nil {
		t.Fatal(err)
	}
	if res.Bucket.Region != "us-west-2" || res.Note == "" {
		t.Fatalf("AddBucket = %+v", res)
	}
}

func TestDamagedSavedFileIsKept(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, savedFileName)
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := New(Options{Dir: dir, Credentials: fakeCredentials}); err == nil {
		t.Fatal("damaged file accepted")
	}
	data, _ := os.ReadFile(path)
	if string(data) != "{not json" {
		t.Fatal("damaged file was changed")
	}
}

func TestNormalizeBucketName(t *testing.T) {
	good := map[string]string{
		"my-bucket": "my-bucket", " s3://my.bucket/ ": "my.bucket", "Legacy_Bucket": "Legacy_Bucket", "abc": "abc",
	}
	for in, want := range good {
		if got, err := NormalizeBucketName(in); err != nil || got != want {
			t.Errorf("%q: got %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"", "a", "-abc", "a/b", "a..b", "s3://", "a b", "ä-bucket", strings.Repeat("a", 256)} {
		if _, err := NormalizeBucketName(in); !errors.Is(err, ErrInvalidBucketName) {
			t.Errorf("%q accepted", in)
		}
	}
}

func TestListingFoldersObjectsAndEncodedKeys(t *testing.T) {
	e := newEnv(t, envOpts{})
	mod := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	keys := []string{
		"media/",             // placeholder of the listed folder itself
		"media/a b+c%d.mov",  // needs URL decoding
		"media/ünï/clip.mxf", // non-ASCII folder
		"media/ctl\x01x.wav", // invalid in XML 1.0 unless encoded
		"media/shot10.exr",   //
		"media/Shot2.exr",    //
		"media/sub/deep/x.y", // folder "sub/"
		"media/sub/other",    //
		"media//empty-seg",   // folder "media//"
		"other/not-listed.txt" /* outside the prefix */}
	for i, k := range keys {
		if err := e.fake.PutListed("west-bucket", k, int64(i*100), mod.Add(time.Duration(i)*time.Hour)); err != nil {
			t.Fatal(err)
		}
	}
	snap := e.wait(e.open("west-bucket", "media/").ID)
	if snap.State != StateDone || snap.Folders != 3 || snap.Objects != 4 {
		t.Fatalf("snapshot = %+v, want 3 folders and 4 objects", snap)
	}
	rows := e.all(snap.ID, SortSpec{Field: SortName})
	var got []string
	for _, r := range rows {
		got = append(got, fmt.Sprintf("%v:%s", r.Folder, r.Name))
	}
	want := []string{"true:/", "true:sub", "true:ünï", "false:a b+c%d.mov", "false:ctl\x01x.wav", "false:Shot2.exr", "false:shot10.exr"}
	if fmt.Sprintf("%q", got) != fmt.Sprintf("%q", want) {
		t.Fatalf("rows =\n%q\nwant\n%q", got, want)
	}
	for _, r := range rows {
		if r.Folder {
			if r.Modified != nil || r.Size != 0 || !strings.HasSuffix(r.Key, "/") {
				t.Errorf("folder row %+v", r)
			}
			continue
		}
		if r.Modified == nil || r.StorageClass != "STANDARD" || !strings.HasPrefix(r.Key, "media/") {
			t.Errorf("object row %+v", r)
		}
	}
	if rows[3].Key != "media/a b+c%d.mov" || rows[3].Size != 100 || !rows[3].Modified.Equal(mod.Add(time.Hour)) {
		t.Errorf("decoded row = %+v", rows[3])
	}
}

func TestListingFollowsARegionChange(t *testing.T) {
	e := newEnv(t, envOpts{})
	ctx := context.Background()
	if _, err := e.svc.Buckets(ctx, testProfile); err != nil { // caches us-west-2
		t.Fatal(err)
	}
	// The bucket is deleted and recreated elsewhere under the same name.
	e.fake.AddBucketInRegion("west-bucket", "eu-central-1")
	if err := e.fake.PutListed("west-bucket", "x", 1, time.Now()); err != nil {
		t.Fatal(err)
	}
	snap := e.wait(e.open("west-bucket", "").ID)
	if snap.State != StateDone || snap.Region != "eu-central-1" || snap.Count != 1 {
		t.Fatalf("snapshot = %+v", snap)
	}
	if r, _ := e.svc.Region(ctx, testProfile, "west-bucket"); r != "eu-central-1" {
		t.Fatalf("cached region = %q", r)
	}
}

func TestListingErrors(t *testing.T) {
	t.Run("access denied", func(t *testing.T) {
		e := newEnv(t, envOpts{})
		e.fake.AddRule(fakes3.Rule{Op: fakes3.OpListObjects, Fault: fakes3.AccessDenied()})
		snap := e.wait(e.open("east-bucket", "private/").ID)
		if snap.State != StateError || !strings.Contains(snap.Error, "Access denied") {
			t.Fatalf("snapshot = %+v", snap)
		}
	})
	t.Run("expired session", func(t *testing.T) {
		e := newEnv(t, envOpts{})
		e.fake.AddRule(fakes3.Rule{Op: fakes3.OpListObjects, Fault: fakes3.ExpiredToken()})
		snap := e.wait(e.open("east-bucket", "").ID)
		if snap.State != StateError || !strings.Contains(snap.Error, "expired") {
			t.Fatalf("snapshot = %+v", snap)
		}
	})
	t.Run("credentials fail to load", func(t *testing.T) {
		e := newEnv(t, envOpts{service: func(o *Options) {
			o.Credentials = func(string) (aws.CredentialsProvider, error) {
				return aws.NewCredentialsCache(s3x.MarkCredentialErrors(aws.CredentialsProviderFunc(
					func(context.Context) (aws.Credentials, error) {
						return aws.Credentials{}, errors.New("helper exited 1")
					}))), nil
			}
		}})
		snap := e.wait(e.open("east-bucket", "").ID)
		if snap.State != StateError || !strings.Contains(snap.Error, "credentials could not be loaded") {
			t.Fatalf("snapshot = %+v", snap)
		}
		if len(e.fake.Requests()) != 0 {
			t.Fatal("a request was sent without credentials")
		}
	})
	t.Run("missing bucket", func(t *testing.T) {
		e := newEnv(t, envOpts{})
		snap := e.wait(e.open("no-such-bucket", "").ID)
		if snap.State != StateError || !strings.Contains(snap.Error, "does not exist") {
			t.Fatalf("snapshot = %+v", snap)
		}
	})
	t.Run("transient 503 is retried by the SDK", func(t *testing.T) {
		e := newEnv(t, envOpts{})
		e.fake.AddRule(fakes3.Rule{Op: fakes3.OpListObjects, Attempt: 1, Fault: fakes3.ServiceUnavailable()})
		if err := e.fake.PutListed("east-bucket", "k", 1, time.Now()); err != nil {
			t.Fatal(err)
		}
		snap := e.wait(e.open("east-bucket", "").ID)
		if snap.State != StateDone || snap.Count != 1 {
			t.Fatalf("snapshot = %+v", snap)
		}
	})
	_ = engine.ErrCredentialsUnavailable
}

func putMany(t *testing.T, fake *fakes3.Server, bucket, prefix string, n int) {
	t.Helper()
	base := time.Date(2025, 6, 1, 0, 0, 0, 0, time.UTC)
	for i := range n {
		// Sizes and dates are scrambled so each order differs from key order.
		size := int64((i * 7919) % 100003)
		mod := base.Add(time.Duration((i*104729)%1000003) * time.Second)
		if err := fake.PutListed(bucket, fmt.Sprintf("%sfile-%06d.bin", prefix, i), size, mod); err != nil {
			t.Fatal(err)
		}
	}
}

func TestProgressiveLoadingAndCancel(t *testing.T) {
	e := newEnv(t, envOpts{service: func(o *Options) { o.PageSize = 100 }})
	putMany(t, e.fake, "east-bucket", "p/", 1000)
	// The third page stalls until the request is cancelled.
	e.fake.AddRule(fakes3.Rule{Op: fakes3.OpListObjects, Attempt: 3, Fault: fakes3.StallAt(0, 0)})
	snap := e.open("east-bucket", "p/")
	deadline := time.Now().Add(30 * time.Second)
	for {
		s, err := e.svc.Snapshot(snap.ID)
		if err != nil {
			t.Fatal(err)
		}
		if s.Count == 200 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("snapshot = %+v, want 200 rows loaded", s)
		}
		time.Sleep(5 * time.Millisecond)
	}
	// A window while loading is sorted over the rows loaded so far.
	w, err := e.svc.Window(snap.ID, SortSpec{Field: SortSize, Desc: true}, 0, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if w.Listing.State != StateLoading || len(w.Rows) != 200 {
		t.Fatalf("window: state %s, %d rows", w.Listing.State, len(w.Rows))
	}
	for i := 1; i < len(w.Rows); i++ {
		if w.Rows[i-1].Size < w.Rows[i].Size {
			t.Fatalf("rows %d and %d out of order", i-1, i)
		}
	}
	var loading bool
	for len(e.notes) > 0 {
		if n := <-e.notes; n.ID == snap.ID && n.State == StateLoading && n.Count > 0 {
			loading = true
		}
	}
	if !loading {
		t.Error("no progress notification while loading")
	}
	e.svc.CloseListing(snap.ID)
	if _, err := e.svc.Window(snap.ID, SortSpec{}, 0, 10); !errors.Is(err, ErrListingNotFound) {
		t.Fatalf("window after close: %v", err)
	}
	// Leak check in Cleanup proves the listing goroutine and its request ended.
}

func TestListingLimitsAndEviction(t *testing.T) {
	e := newEnv(t, envOpts{service: func(o *Options) {
		o.MaxRows = 2500
		o.MaxListings = 2
	}})
	putMany(t, e.fake, "east-bucket", "", 4000)
	snap := e.wait(e.open("east-bucket", "").ID)
	if snap.State != StateDone || !snap.Truncated || snap.Count != 2500 {
		t.Fatalf("snapshot = %+v, want 2500 rows, truncated", snap)
	}
	if snap.Pages != 3 {
		t.Errorf("%d pages, want 3: loading should stop at the limit", snap.Pages)
	}
	second := e.open("west-bucket", "")
	third := e.open("eu-bucket", "")
	e.wait(second.ID)
	e.wait(third.ID)
	if _, err := e.svc.Snapshot(snap.ID); !errors.Is(err, ErrListingNotFound) {
		t.Fatalf("oldest listing still open: %v", err)
	}
}

func TestWindowBounds(t *testing.T) {
	e := newEnv(t, envOpts{})
	putMany(t, e.fake, "east-bucket", "", 30)
	id := e.wait(e.open("east-bucket", "").ID).ID
	for _, tc := range []struct{ offset, limit, want int }{
		{0, 10, 10}, {25, 10, 5}, {30, 10, 0}, {100, 10, 0}, {-5, 3, 3}, {0, 5000, 30}, {0, -1, 0},
	} {
		w, err := e.svc.Window(id, SortSpec{}, tc.offset, tc.limit)
		if err != nil || len(w.Rows) != tc.want {
			t.Errorf("Window(%d, %d) = %d rows, %v; want %d", tc.offset, tc.limit, len(w.Rows), err, tc.want)
		}
	}
	if _, err := e.svc.Window(id, SortSpec{Field: "colour"}, 0, 1); err == nil {
		t.Error("unknown sort field accepted")
	}
}

func TestConcurrentWindowsDuringLoading(t *testing.T) {
	e := newEnv(t, envOpts{service: func(o *Options) { o.PageSize = 50 }})
	putMany(t, e.fake, "east-bucket", "", 3000)
	id := e.open("east-bucket", "").ID
	specs := []SortSpec{{SortName, false}, {SortName, true}, {SortSize, false}, {SortModified, true}, {SortClass, false}}
	var wg sync.WaitGroup
	for _, spec := range specs {
		wg.Go(func() {
			for {
				w, err := e.svc.Window(id, spec, 0, 100)
				if err != nil {
					t.Error(err)
					return
				}
				if w.Listing.State != StateLoading {
					return
				}
			}
		})
	}
	wg.Wait()
	snap := e.wait(id)
	for _, spec := range specs {
		checkOrder(t, e.all(id, spec), spec, snap.Count)
	}
}

// checkOrder verifies a full window walk: every row once, in spec's order.
func checkOrder(t *testing.T, rows []Row, spec SortSpec, count int) {
	t.Helper()
	if len(rows) != count {
		t.Fatalf("%v: %d rows, want %d", spec, len(rows), count)
	}
	seen := make(map[string]bool, len(rows))
	for i, r := range rows {
		if seen[r.Key] {
			t.Fatalf("%v: %s twice", spec, r.Key)
		}
		seen[r.Key] = true
		if i > 0 && compareRows(rows[i-1], r, spec) >= 0 {
			t.Fatalf("%v: row %d (%s) not after row %d (%s)", spec, i, r.Key, i-1, rows[i-1].Key)
		}
	}
}

// compareRows is an independent statement of the sort rules, written against
// the public Row type.
func compareRows(a, b Row, spec SortSpec) int {
	if a.Folder != b.Folder {
		if a.Folder {
			return -1
		}
		return 1
	}
	byName := CompareNames(a.Name, b.Name)
	if a.Folder && spec.Field != SortName {
		return byName
	}
	c := 0
	switch spec.Field {
	case SortName:
		c = byName
	case SortSize:
		c = cmpInt(a.Size, b.Size)
	case SortModified:
		c = cmpInt(a.Modified.UnixNano(), b.Modified.UnixNano())
	case SortClass:
		c = strings.Compare(a.StorageClass, b.StorageClass)
	}
	if spec.Desc {
		c = -c
	}
	if c == 0 {
		c = byName
	}
	return c
}

func cmpInt(a, b int64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

func TestRowLimitCountsFolders(t *testing.T) {
	e := newEnv(t, envOpts{service: func(o *Options) { o.MaxRows = 50 }})
	for i := range 100 {
		if err := e.fake.PutListed("east-bucket", fmt.Sprintf("d%03d/x", i), 1, time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	snap := e.wait(e.open("east-bucket", "").ID)
	if snap.State != StateDone || !snap.Truncated || snap.Count != 50 || snap.Folders != 50 {
		t.Fatalf("snapshot = %+v, want 50 folders, truncated", snap)
	}
}

func TestSummarizeRanges(t *testing.T) {
	e := newEnv(t, envOpts{})
	snap := e.wait(e.open("west-bucket", "").ID)
	spec := SortSpec{Field: SortName}
	rows := e.all(snap.ID, spec)
	if len(rows) < 3 {
		t.Skip("fixture too small")
	}
	var want Summary
	for _, r := range rows[1:3] {
		want.Count++
		if r.Folder {
			want.Folders++
		} else {
			want.Objects++
			want.Bytes += r.Size
		}
	}
	got, err := e.svc.Summarize(snap.ID, spec, [][2]int{{1, 2}})
	if err != nil || got != want {
		t.Fatalf("got %+v, %v want %+v", got, err, want)
	}
	all, _ := e.svc.Summarize(snap.ID, spec, [][2]int{{-5, 1 << 40}})
	if all.Count != len(rows) {
		t.Fatalf("clamped range counted %d of %d", all.Count, len(rows))
	}
	none, _ := e.svc.Summarize(snap.ID, spec, [][2]int{{5, 2}})
	if none.Count != 0 {
		t.Fatalf("inverted range counted %d", none.Count)
	}
}
