package objects_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"s4browser/internal/engine"
	"s4browser/internal/fakes3"
	"s4browser/internal/objects"
)

const bucket = fakes3.DefaultBucket

func newFake(t *testing.T, o fakes3.Options) (*fakes3.Server, *s3.Client) {
	t.Helper()
	s, err := fakes3.New(o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return s, s.S3Client()
}

func put(t *testing.T, s *fakes3.Server, keys ...string) {
	t.Helper()
	for _, k := range keys {
		size := int64(10)
		if strings.HasSuffix(k, "/") {
			size = 0 // a folder placeholder
		}
		if err := s.PutListed(bucket, k, size, time.Now()); err != nil {
			t.Fatal(err)
		}
	}
}

func TestDeletePlanAndRun(t *testing.T) {
	s, c := newFake(t, fakes3.Options{})
	ctx := context.Background()
	put(t, s, "a/one.bin", "a/sub/two.bin", "a/sub/", "a/", "ab/keep.bin", "top.bin", "other/keep.bin")
	for i := range 2500 {
		put(t, s, fmt.Sprintf("big/%05d", i))
	}

	var found []int
	plan, err := objects.PlanDelete(ctx, c, bucket, []objects.Item{
		{Key: "a/", Folder: true}, {Key: "top.bin", Size: 10}, {Key: "a/one.bin", Size: 10}, {Key: "big/", Folder: true},
	}, 0, func(n int) { found = append(found, n) })
	if err != nil {
		t.Fatal(err)
	}
	// "a/" must not reach "ab/keep.bin"; a/one.bin is listed once.
	if plan.Objects != 4+1+2500 || plan.Folders != 2 || plan.Placeholders != 2 {
		t.Fatalf("plan = %d objects, %d folders, %d placeholders", plan.Objects, plan.Folders, plan.Placeholders)
	}
	if plan.Bytes != int64(plan.Objects-plan.Placeholders)*10 {
		t.Fatalf("bytes = %d", plan.Bytes)
	}
	if len(found) < 3 {
		t.Fatalf("progress calls = %v, want one per page", found)
	}

	var done []int
	res := plan.Run(ctx, c, func(n int) { done = append(done, n) })
	if res.Deleted != plan.Objects || res.FailedTotal != 0 || res.Stopped != "" || res.Markers != 0 {
		t.Fatalf("result = %+v", res)
	}
	if fmt.Sprint(done) != fmt.Sprint([]int{1000, 2000, 2505}) {
		t.Fatalf("progress = %v", done)
	}
	if got := s.Finished(fakes3.OpDeleteObjects, bucket, "", 0); got != 3 {
		t.Fatalf("DeleteObjects calls = %d, want 3", got)
	}
	if n := s.ObjectCount(bucket); n != 2 {
		t.Fatalf("%d objects left, want ab/keep.bin and other/keep.bin", n)
	}
	for _, k := range []string{"ab/keep.bin", "other/keep.bin"} {
		if _, ok := s.Object(bucket, k); !ok {
			t.Fatalf("%s was deleted", k)
		}
	}
}

func TestDeleteReportsRefusedKeysAndMarkers(t *testing.T) {
	guard, err := engine.NewTestPrefixGuard("run1")
	if err != nil {
		t.Fatal(err)
	}
	s, c := newFake(t, fakes3.Options{Guard: guard})
	ctx := context.Background()
	in := "s4browser-test/run1/"
	put(t, s, in+"x.bin", in+"y.bin", in+"z.bin", "outside.bin")
	if err := s.SetVersioning(bucket, "Enabled"); err != nil {
		t.Fatal(err)
	}
	s.AddRule(fakes3.Rule{Op: fakes3.OpDelete, Key: in + "y.bin", Fault: fakes3.Fault{Kind: fakes3.FaultError, Status: 403, Code: "AccessDenied", Message: "no"}})

	plan, err := objects.PlanDelete(ctx, c, bucket, []objects.Item{{Key: in, Folder: true}, {Key: "outside.bin"}}, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	res := plan.Run(ctx, c, nil)
	if res.Deleted != 2 || res.Markers != 2 || res.FailedTotal != 2 || len(res.Failed) != 2 {
		t.Fatalf("result = %+v", res)
	}
	for _, f := range res.Failed {
		if f.Code != "AccessDenied" || (f.Key != in+"y.bin" && f.Key != "outside.bin") {
			t.Fatalf("failure = %+v", f)
		}
	}
	if _, ok := s.Object(bucket, "outside.bin"); !ok {
		t.Fatal("the guard did not keep outside.bin")
	}
	if v, err := objects.Versioning(ctx, c, bucket); err != nil || v != objects.VersioningEnabled {
		t.Fatalf("versioning = %q, %v", v, err)
	}
}

func TestDeleteStopsOnFailedRequestAndCancel(t *testing.T) {
	s, c := newFake(t, fakes3.Options{})
	ctx := context.Background()
	for i := range 1500 {
		put(t, s, fmt.Sprintf("f/%05d", i))
	}
	plan, err := objects.PlanDelete(ctx, c, bucket, []objects.Item{{Key: "f/", Folder: true}}, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	s.AddRule(fakes3.Rule{Op: fakes3.OpDeleteObjects, Attempt: 0, FromAttempt: 2, Fault: fakes3.AccessDenied()})
	res := plan.Run(ctx, c, nil)
	if res.Deleted != 1000 || res.NotTried != 500 || !strings.Contains(res.Stopped, "AccessDenied") {
		t.Fatalf("result = %+v", res)
	}

	cctx, cancel := context.WithCancel(ctx)
	cancel()
	res = plan.Run(cctx, c, nil)
	if res.Deleted != 0 || res.NotTried != 1500 || res.Stopped != "Cancelled." {
		t.Fatalf("cancelled result = %+v", res)
	}

	if _, err := objects.PlanDelete(ctx, c, bucket, []objects.Item{{Key: "f/", Folder: true}}, 100, nil); !errors.Is(err, objects.ErrTooMany) {
		t.Fatalf("over the cap: %v", err)
	}
}

func TestVersioningStates(t *testing.T) {
	s, c := newFake(t, fakes3.Options{})
	ctx := context.Background()
	for _, tc := range []struct{ set, want string }{{"", objects.VersioningOff}, {"Suspended", objects.VersioningSuspended}, {"Enabled", objects.VersioningEnabled}} {
		if err := s.SetVersioning(bucket, tc.set); err != nil {
			t.Fatal(err)
		}
		if v, err := objects.Versioning(ctx, c, bucket); err != nil || v != tc.want {
			t.Fatalf("%q: got %q, %v", tc.set, v, err)
		}
	}
	s.AddRule(fakes3.Rule{Op: fakes3.OpVersioning, Fault: fakes3.AccessDenied()})
	if v, err := objects.Versioning(ctx, c, bucket); err == nil || v != objects.VersioningUnknown {
		t.Fatalf("denied: got %q, %v", v, err)
	}
}

func TestCreateFolder(t *testing.T) {
	s, c := newFake(t, fakes3.Options{})
	ctx := context.Background()
	put(t, s, "media/existing/file.bin", "media/taken")

	key, err := objects.CreateFolder(ctx, c, bucket, "media/", "Reel 1 — ünïcode")
	if err != nil || key != "media/Reel 1 — ünïcode/" {
		t.Fatalf("got %q, %v", key, err)
	}
	if st, ok := s.Object(bucket, key); !ok || st.Size != 0 {
		t.Fatalf("placeholder = %+v, %v", st, ok)
	}
	for _, name := range []string{"existing", "Reel 1 — ünïcode"} {
		if _, err := objects.CreateFolder(ctx, c, bucket, "media/", name); !errors.Is(err, objects.ErrFolderExists) {
			t.Fatalf("%q: %v", name, err)
		}
	}
	for _, name := range []string{"", " x", "x ", "a/b", `a\b`, ".", "..", "tab\there", strings.Repeat("n", 1100)} {
		if _, err := objects.CreateFolder(ctx, c, bucket, "media/", name); !errors.Is(err, objects.ErrFolderName) {
			t.Fatalf("%q: %v", name, err)
		}
	}
	// "taken" is an object, not a folder: a folder of the same name is allowed,
	// as S3 keys "taken" and "taken/" are different.
	if _, err := objects.CreateFolder(ctx, c, bucket, "media/", "taken"); err != nil {
		t.Fatal(err)
	}
}

func TestPresign(t *testing.T) {
	start := time.Now()
	clock := fakes3.NewFakeClock(start)
	s, c := newFake(t, fakes3.Options{Clock: clock})
	ctx := context.Background()
	if err := s.PutSynthetic(bucket, "clips/a b+c.bin", 7, 4096); err != nil {
		t.Fatal(err)
	}

	p, err := objects.Presign(ctx, c, bucket, "clips/a b+c.bin", time.Hour, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if p.Effective != 3600 || p.LimitedBy != "" || p.CredentialsExpire != nil {
		t.Fatalf("presigned = %+v", p)
	}
	u, _ := url.Parse(p.URL)
	if u.Query().Get("X-Amz-Expires") != "3600" {
		t.Fatalf("url expires = %q", u.Query().Get("X-Amz-Expires"))
	}
	if d := p.Expires.Sub(start); d < time.Hour-time.Minute || d > time.Hour+time.Minute {
		t.Fatalf("expires %v after start", d)
	}
	get := func() int {
		resp, err := s.HTTPClient().Get(p.URL)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		n, _ := io.Copy(io.Discard, resp.Body)
		if resp.StatusCode == http.StatusOK && n != 4096 {
			t.Fatalf("read %d bytes", n)
		}
		return resp.StatusCode
	}
	if code := get(); code != http.StatusOK {
		t.Fatalf("before expiry: %d", code)
	}
	clock.Advance(time.Hour + time.Minute)
	if code := get(); code != http.StatusForbidden {
		t.Fatalf("after expiry: %d", code)
	}

	// Longer than S3 allows: capped at seven days.
	p, err = objects.Presign(ctx, c, bucket, "clips/a b+c.bin", 30*24*time.Hour, time.Now())
	if err != nil || p.Effective != int64(objects.MaxPresign/time.Second) || p.LimitedBy != objects.LimitMaximum {
		t.Fatalf("capped: %+v, %v", p, err)
	}

	// Temporary credentials: the link ends when they do.
	expiring := func(left time.Duration) *s3.Client {
		return s.S3Client(func(o *s3.Options) {
			o.Credentials = aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
				return aws.Credentials{AccessKeyID: fakes3.AccessKeyID, SecretAccessKey: fakes3.SecretAccessKey, SessionToken: "tok", CanExpire: true, Expires: time.Now().Add(left)}, nil
			})
		})
	}
	p, err = objects.Presign(ctx, expiring(20*time.Minute), bucket, "clips/a b+c.bin", time.Hour, time.Now())
	if err != nil || p.LimitedBy != objects.LimitCredentials || p.Effective < 19*60 || p.Effective > 20*60 || p.CredentialsExpire == nil {
		t.Fatalf("credential-limited: %+v, %v", p, err)
	}
	if !strings.Contains(p.URL, "X-Amz-Security-Token=tok") {
		t.Fatal("the URL does not carry the session token it was signed with")
	}
	if _, err := objects.Presign(ctx, expiring(30*time.Second), bucket, "clips/a b+c.bin", time.Hour, time.Now()); !errors.Is(err, objects.ErrCredentialsExpiring) {
		t.Fatalf("nearly expired: %v", err)
	}
	if _, err := objects.Presign(ctx, c, bucket, "x", time.Second, time.Now()); err == nil {
		t.Fatal("a one-second link was signed")
	}
}

func TestHeadMetadata(t *testing.T) {
	_, c := newFake(t, fakes3.Options{})
	ctx := context.Background()
	_, err := c.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(bucket), Key: aws.String("doc.txt"), Body: strings.NewReader("hello"),
		ContentType: aws.String("text/plain"), CacheControl: aws.String("max-age=60"),
		Metadata: map[string]string{"camera": "A", "Shot-Id": "42"}, Tagging: aws.String("a=1&b=2"),
	})
	if err != nil {
		t.Fatal(err)
	}
	m, err := objects.Head(ctx, c, bucket, "doc.txt")
	if err != nil {
		t.Fatal(err)
	}
	if m.Size != 5 || m.ContentType != "text/plain" || m.CacheControl != "max-age=60" || m.StorageClass != "STANDARD" || m.TagCount != 2 {
		t.Fatalf("metadata = %+v", m)
	}
	if m.Metadata["camera"] != "A" || m.Metadata["shot-id"] != "42" {
		t.Fatalf("user metadata = %v", m.Metadata)
	}
	if !strings.HasPrefix(m.Checksum, "CRC32 ") || m.Parts != 0 {
		t.Fatalf("checksum %q, parts %d", m.Checksum, m.Parts)
	}
	if _, err := objects.Head(ctx, c, bucket, "missing"); err == nil {
		t.Fatal("missing object had metadata")
	}
}

func TestTags(t *testing.T) {
	s, c := newFake(t, fakes3.Options{})
	ctx := context.Background()
	// A 6 TiB multipart-sized object: tagging must not copy it.
	if err := s.PutSynthetic(bucket, "huge.mxf", 1, 6<<40); err != nil {
		t.Fatal(err)
	}
	tags, err := objects.Tags(ctx, c, bucket, "huge.mxf")
	if err != nil || len(tags) != 0 {
		t.Fatalf("initial tags %v, %v", tags, err)
	}
	want := []objects.Tag{{"project", "Ünï Film"}, {"reel", "1"}, {"note", ""}}
	if err := objects.SetTags(ctx, c, bucket, "huge.mxf", nil, want); err != nil {
		t.Fatal(err)
	}
	got, _ := objects.Tags(ctx, c, bucket, "huge.mxf")
	if fmt.Sprint(got) != fmt.Sprint([]objects.Tag{{"note", ""}, {"project", "Ünï Film"}, {"reel", "1"}}) {
		t.Fatalf("tags = %v", got)
	}
	// Someone else changed them: the stale edit is refused.
	if err := objects.SetTags(ctx, c, bucket, "huge.mxf", nil, []objects.Tag{{"x", "y"}}); !errors.Is(err, objects.ErrTagsChanged) {
		t.Fatalf("stale edit: %v", err)
	}
	// Removing every tag.
	if err := objects.SetTags(ctx, c, bucket, "huge.mxf", got, nil); err != nil {
		t.Fatal(err)
	}
	if x, _ := s.Extras(bucket, "huge.mxf"); len(x.Tags) != 0 {
		t.Fatalf("tags left: %v", x.Tags)
	}
	if s.Finished(fakes3.OpPut, "", "", 0)+s.Finished(fakes3.OpCreateUpload, "", "", 0) != 0 {
		t.Fatal("tagging rewrote the object")
	}

	for _, bad := range [][]objects.Tag{
		{{"", "v"}}, {{"aws:x", "v"}}, {{"a", "v"}, {"a", "w"}}, {{"k#", "v"}}, {{"k", "v<"}},
		{{strings.Repeat("k", 129), ""}}, {{"k", strings.Repeat("v", 257)}},
		{{"1", ""}, {"2", ""}, {"3", ""}, {"4", ""}, {"5", ""}, {"6", ""}, {"7", ""}, {"8", ""}, {"9", ""}, {"10", ""}, {"11", ""}},
	} {
		if err := objects.ValidateTags(bad); err == nil {
			t.Fatalf("%v was accepted", bad)
		}
		if err := objects.SetTags(ctx, c, bucket, "huge.mxf", nil, bad); err == nil {
			t.Fatalf("%v was saved", bad)
		}
	}
	if err := objects.ValidateTags([]objects.Tag{{"a b+-=._:/@", "Ünï 9"}}); err != nil {
		t.Fatal(err)
	}
}
