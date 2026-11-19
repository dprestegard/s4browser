package s3x_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"s4browser/internal/engine"
	"s4browser/internal/fakes3"
	"s4browser/internal/journal"
	"s4browser/internal/s3x"
)

func cleanupEntry(k string) journal.Entry {
	return journal.Entry{ID: "0123456789abcdef-1", Kind: journal.KindMultipartUpload, Phase: journal.PhaseCreating,
		CreatedAt: time.Now().UTC(), Profile: "work", Region: fakes3.Region, Bucket: fakes3.DefaultBucket, Key: k}
}

func TestCleanerListsEveryPageAndAborts(t *testing.T) {
	server := newServer(t)
	cleaner := s3x.NewCleaner(s3x.StaticClient(server.S3Client()))
	ctx := context.Background()
	const n = 1005 // More than one page of 1,000.
	want := map[string]bool{}
	for i := range n {
		k := key
		if i%2 == 1 {
			k = fmt.Sprintf("%s.%04d", key, i) // Shares the prefix, so S3 lists it too.
		}
		id, err := server.Store().CreateMultipartUpload(ctx, engine.ObjectRef{Bucket: fakes3.DefaultBucket, Key: k})
		if err != nil {
			t.Fatal(err)
		}
		want[id] = true
	}
	if err := server.PutSynthetic(fakes3.DefaultBucket, "s4browser-test/run1/other", 1, 1); err != nil {
		t.Fatal(err)
	}
	entry := cleanupEntry(key)
	got, err := cleaner.ListUploads(ctx, entry)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != n {
		t.Fatalf("listed %d uploads, want %d", len(got), n)
	}
	for _, u := range got {
		if !want[u.UploadID] || u.Initiated.IsZero() {
			t.Fatalf("unexpected upload %+v", u)
		}
	}
	if server.Finished(fakes3.OpListUploads, "", "", 0) < 2 {
		t.Fatal("the second page was not requested")
	}

	if err := cleaner.AbortUpload(ctx, entry, got[0].UploadID); err != nil {
		t.Fatal(err)
	}
	if err := cleaner.AbortUpload(ctx, entry, got[0].UploadID); !errors.Is(err, journal.ErrUploadGone) {
		t.Fatalf("a second abort: %v", err)
	}
}

func TestCleanerMarksCredentialFailures(t *testing.T) {
	server := newServer(t)
	cleaner := s3x.NewCleaner(s3x.StaticClient(server.S3Client()))
	ctx := context.Background()
	entry := cleanupEntry(key)
	id, err := server.Store().CreateMultipartUpload(ctx, ref)
	if err != nil {
		t.Fatal(err)
	}

	server.AddRule(fakes3.Rule{Fault: fakes3.ExpiredToken(), Times: 2})
	if err := cleaner.AbortUpload(ctx, entry, id); !errors.Is(err, journal.ErrCredentials) {
		t.Fatalf("abort: %v", err)
	}
	if _, err := cleaner.ListUploads(ctx, entry); !errors.Is(err, journal.ErrCredentials) {
		t.Fatalf("list: %v", err)
	}

	// A provider that cannot load credentials sends nothing.
	broken := s3x.NewCleaner(s3x.NewClients(func(string) (aws.CredentialsProvider, error) {
		return nil, errors.New("profile not found")
	}))
	before := len(server.Requests())
	if err := broken.AbortUpload(ctx, entry, id); !errors.Is(err, journal.ErrCredentials) {
		t.Fatalf("abort without credentials: %v", err)
	}
	if len(server.Requests()) != before {
		t.Fatal("a request was sent without credentials")
	}

	server.AddRule(fakes3.Rule{Fault: fakes3.AccessDenied()})
	err = cleaner.AbortUpload(ctx, entry, id)
	if err == nil || errors.Is(err, journal.ErrCredentials) || errors.Is(err, journal.ErrUploadGone) {
		t.Fatalf("access denied: %v", err)
	}
	var opErr *engine.OpError
	if !errors.As(err, &opErr) || opErr.Status != http.StatusForbidden {
		t.Fatalf("access denied lost its status: %v", err)
	}
}

// TestClientsAreSharedPerProfileAndRegion: one client, and one credentials
// lookup, per profile and region. The provider is used as given, so the SDK
// adds no second cache in front of creds.Manager's.
func TestClientsAreSharedPerProfileAndRegion(t *testing.T) {
	server := newServer(t)
	var mu sync.Mutex
	lookups := map[string]int{}
	retrieves := 0
	provider := aws.NewCredentialsCache(aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
		mu.Lock()
		defer mu.Unlock()
		retrieves++
		return credentials.NewStaticCredentialsProvider(fakes3.AccessKeyID, fakes3.SecretAccessKey, "").Retrieve(context.Background())
	}))
	clients := s3x.NewClients(func(profile string) (aws.CredentialsProvider, error) {
		mu.Lock()
		defer mu.Unlock()
		lookups[profile]++
		return provider, nil
	}, func(o *s3.Options) {
		o.BaseEndpoint = aws.String(server.Endpoint())
		o.UsePathStyle = true
		o.HTTPClient = server.HTTPClient()
	})
	ctx := context.Background()
	a1, _ := clients(ctx, engine.ObjectRef{Profile: "a", Region: "us-east-1"})
	a2, _ := clients(ctx, engine.ObjectRef{Profile: "a", Region: "us-east-1"})
	a3, _ := clients(ctx, engine.ObjectRef{Profile: "a", Region: "eu-west-1"})
	b1, _ := clients(ctx, engine.ObjectRef{Profile: "b", Region: "us-east-1"})
	if a1 != a2 || a1 == a3 || a1 == b1 {
		t.Fatal("clients are not kept per profile and region")
	}
	if lookups["a"] != 2 || lookups["b"] != 1 {
		t.Fatalf("lookups %v", lookups)
	}
	if a1.Options().Credentials != provider {
		t.Fatal("the SDK replaced the provider")
	}

	store := s3x.NewStore(clients)
	for range 3 {
		if _, err := store.HeadObject(ctx, engine.ObjectRef{Profile: "a", Region: fakes3.Region, Bucket: fakes3.DefaultBucket, Key: "missing"}); err == nil {
			t.Fatal("HEAD of a missing object succeeded")
		}
	}
	provider.Invalidate()
	if _, err := store.HeadObject(ctx, engine.ObjectRef{Profile: "a", Region: fakes3.Region, Bucket: fakes3.DefaultBucket, Key: "missing"}); err == nil {
		t.Fatal("HEAD of a missing object succeeded")
	}
	if retrieves != 2 {
		t.Fatalf("credentials were retrieved %d times, want once per invalidation", retrieves)
	}
}
