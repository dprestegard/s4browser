package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"

	"s4browser/internal/engine"
	"s4browser/internal/fakes3"
	"s4browser/internal/s3x"
)

// commonFlags are shared by the commands that talk to S3.
type commonFlags struct {
	fake    bool
	profile string
	region  string
	bucket  string
	runID   string
	seed    uint64
	result  string
}

func (c *commonFlags) register(fs *flag.FlagSet) {
	fs.BoolVar(&c.fake, "fake", false, "use a local TLS fake S3 started inside this process")
	fs.StringVar(&c.profile, "profile", "", "AWS profile name (static keys or credential_process)")
	fs.StringVar(&c.region, "region", "", "AWS region of the bucket")
	fs.StringVar(&c.bucket, "bucket", "", "bucket name")
	fs.StringVar(&c.runID, "run-id", "", "test run ID; all keys live under s4browser-test/<run-id>/ (default: generated; required for cleanup against S3)")
	fs.Uint64Var(&c.seed, "seed", 0, "with --fake: content seed of the data (upload: verify the stored object against it; download: seed of the generated object)")
	fs.StringVar(&c.result, "result", "", "JSON result file (default s4engine-<command>-<run-id>.json in the current directory)")
}

// validate checks the target flags and builds the guard.
func (c *commonFlags) validate(command string, needRunID bool) (*engine.TestPrefixGuard, error) {
	switch {
	case c.fake && (c.profile != "" || c.region != "" || c.bucket != ""):
		return nil, usagef("--fake cannot be combined with --profile, --region or --bucket")
	case !c.fake && (c.profile == "" || c.region == "" || c.bucket == ""):
		return nil, usagef("give --fake, or all of --profile, --region and --bucket")
	}
	if c.runID == "" {
		if needRunID && !c.fake {
			return nil, usagef("--run-id is required: it names the only prefix this command may touch")
		}
		c.runID = newRunID()
	}
	guard, err := engine.NewTestPrefixGuard(c.runID)
	if err != nil {
		return nil, usagef("%v", err)
	}
	if c.fake {
		c.bucket, c.region = fakes3.DefaultBucket, fakes3.Region
	}
	if c.result == "" {
		c.result = fmt.Sprintf("s4engine-%s-%s.json", command, c.runID)
	}
	return guard, nil
}

func newRunID() string {
	var b [3]byte
	_, _ = rand.Read(b[:])
	return time.Now().UTC().Format("20060102-150405") + "-" + hex.EncodeToString(b[:])
}

// adminAPI is the listing and deletion the engine contract does not cover.
type adminAPI interface {
	ListUploads(ctx context.Context, bucket, prefix string) ([]uploadRef, error)
	AbortUpload(ctx context.Context, bucket, key, uploadID string) error
	ListObjects(ctx context.Context, bucket, prefix string) ([]string, error)
	DeleteObject(ctx context.Context, bucket, key string) error
}

type uploadRef struct{ Key, ID string }

// target is everything a command needs to reach S3, with the guard applied.
type target struct {
	kind    string // "fake" or "s3"
	profile string
	region  string
	bucket  string
	guard   *engine.TestPrefixGuard
	store   engine.ObjectStore // guarded
	admin   adminAPI           // guarded
	prober  engine.CredentialProber
	fake    *fakes3.Server
	close   func()
}

func (t *target) ref(key string) engine.ObjectRef {
	return engine.ObjectRef{Profile: t.profile, Region: t.region, Bucket: t.bucket, Key: key}
}

// newTarget connects to the fake or to S3. The guard wraps the store and the
// admin calls here, so no caller can reach the network without it.
func newTarget(ctx context.Context, c *commonFlags, guard *engine.TestPrefixGuard, e env) (*target, error) {
	if c.fake {
		server, closeServer := e.fake, func() {}
		if server == nil {
			var err error
			server, err = fakes3.New(fakes3.Options{Guard: guard})
			if err != nil {
				return nil, err
			}
			closeServer = server.Close
		}
		client := server.S3Client()
		return &target{
			kind: "fake", profile: "fake", region: c.region, bucket: c.bucket, guard: guard, fake: server, close: closeServer,
			store:  fakes3.GuardStore(s3x.NewStore(s3x.StaticClient(client)), guard),
			admin:  guardedAdmin{sdkAdmin{client}, guard},
			prober: okProber{},
		}, nil
	}
	creds, err := loadCredentials(ctx, c.profile, c.region)
	if err != nil {
		return nil, err
	}
	client := s3.NewFromConfig(creds.cfg, func(o *s3.Options) {
		o.Region = c.region
		o.HTTPClient = s3x.NewHTTPClient()
		o.Credentials = creds.provider
	})
	return &target{
		kind: "s3", profile: c.profile, region: c.region, bucket: c.bucket, guard: guard, close: func() {},
		store:  fakes3.GuardStore(s3x.NewStore(s3x.StaticClient(client)), guard),
		admin:  guardedAdmin{sdkAdmin{client}, guard},
		prober: s3x.NewProber(creds.Reload, s3x.StaticClient(client)),
	}, nil
}

type okProber struct{}

func (okProber) Probe(context.Context, engine.ObjectRef) error { return nil }

// awsCredentials holds the credential chain of one profile.
type awsCredentials struct {
	cfg aws.Config
	// provider is aws.NewCredentialsCache(s3x.MarkCredentialErrors(base)), as
	// docs/progress.md requires, so provider failures match
	// engine.ErrCredentialsUnavailable.
	provider *aws.CredentialsCache
	// loaded is the SDK's own cache around the chain. Probe empties both caches,
	// otherwise a refresh would return the old value.
	loaded *aws.CredentialsCache
}

// loadCredentials uses the SDK config loader, which honours AWS_CONFIG_FILE and
// AWS_SHARED_CREDENTIALS_FILE and supports static keys and credential_process.
func loadCredentials(ctx context.Context, profile, region string) (*awsCredentials, error) {
	cfg, err := config.LoadDefaultConfig(ctx, config.WithSharedConfigProfile(profile), config.WithRegion(region))
	if err != nil {
		return nil, fmt.Errorf("load AWS profile %q: %w", profile, err)
	}
	if cfg.Credentials == nil {
		return nil, fmt.Errorf("AWS profile %q has no credentials", profile)
	}
	ac := &awsCredentials{cfg: cfg}
	ac.loaded, _ = cfg.Credentials.(*aws.CredentialsCache)
	ac.provider = aws.NewCredentialsCache(s3x.MarkCredentialErrors(cfg.Credentials))
	return ac, nil
}

// Reload drops both cached credentials and fetches new ones. The engine's
// prober (s3x.NewProber) calls it before proving the result with a request.
func (a *awsCredentials) Reload(ctx context.Context, _ string) error {
	if a.loaded != nil {
		a.loaded.Invalidate()
	}
	a.provider.Invalidate()
	_, err := a.provider.Retrieve(ctx)
	return err
}

// sdkAdmin implements adminAPI with the SDK.
type sdkAdmin struct{ c *s3.Client }

func (a sdkAdmin) ListUploads(ctx context.Context, bucket, prefix string) ([]uploadRef, error) {
	var out []uploadRef
	in := &s3.ListMultipartUploadsInput{Bucket: aws.String(bucket), Prefix: aws.String(prefix)}
	for {
		r, err := a.c.ListMultipartUploads(ctx, in)
		if err != nil {
			return nil, err
		}
		for _, u := range r.Uploads {
			out = append(out, uploadRef{Key: aws.ToString(u.Key), ID: aws.ToString(u.UploadId)})
		}
		if !aws.ToBool(r.IsTruncated) {
			return out, nil
		}
		in.KeyMarker, in.UploadIdMarker = r.NextKeyMarker, r.NextUploadIdMarker
	}
}

func (a sdkAdmin) AbortUpload(ctx context.Context, bucket, key, uploadID string) error {
	_, err := a.c.AbortMultipartUpload(ctx, &s3.AbortMultipartUploadInput{
		Bucket: aws.String(bucket), Key: aws.String(key), UploadId: aws.String(uploadID),
	})
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) && apiErr.ErrorCode() == "NoSuchUpload" {
		return nil
	}
	return err
}

func (a sdkAdmin) ListObjects(ctx context.Context, bucket, prefix string) ([]string, error) {
	var keys []string
	pager := s3.NewListObjectsV2Paginator(a.c, &s3.ListObjectsV2Input{Bucket: aws.String(bucket), Prefix: aws.String(prefix)})
	for pager.HasMorePages() {
		page, err := pager.NextPage(ctx)
		if err != nil {
			return nil, err
		}
		for _, o := range page.Contents {
			keys = append(keys, aws.ToString(o.Key))
		}
	}
	return keys, nil
}

func (a sdkAdmin) DeleteObject(ctx context.Context, bucket, key string) error {
	_, err := a.c.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(bucket), Key: aws.String(key)})
	return err
}

// guardedAdmin lists only below the guard's prefix and checks every abort and
// delete against it.
type guardedAdmin struct {
	inner adminAPI
	guard engine.WriteGuard
}

// checkListPrefix refuses a listing that is not strictly inside the guard's
// prefix. A listing is only a read, but cleanup never looks beyond its prefix.
func (g guardedAdmin) checkListPrefix(prefix string) error {
	if err := g.guard.CheckWrite(prefix + "x"); err != nil || !strings.HasSuffix(prefix, "/") {
		return fmt.Errorf("%w: refusing to list %q", engine.ErrWriteRefused, prefix)
	}
	return nil
}

func (g guardedAdmin) ListUploads(ctx context.Context, bucket, prefix string) ([]uploadRef, error) {
	if err := g.checkListPrefix(prefix); err != nil {
		return nil, err
	}
	return g.inner.ListUploads(ctx, bucket, prefix)
}

func (g guardedAdmin) AbortUpload(ctx context.Context, bucket, key, uploadID string) error {
	if err := g.guard.CheckWrite(key); err != nil {
		return err
	}
	return g.inner.AbortUpload(ctx, bucket, key, uploadID)
}

func (g guardedAdmin) ListObjects(ctx context.Context, bucket, prefix string) ([]string, error) {
	if err := g.checkListPrefix(prefix); err != nil {
		return nil, err
	}
	return g.inner.ListObjects(ctx, bucket, prefix)
}

func (g guardedAdmin) DeleteObject(ctx context.Context, bucket, key string) error {
	if err := g.guard.CheckWrite(key); err != nil {
		return err
	}
	return g.inner.DeleteObject(ctx, bucket, key)
}
