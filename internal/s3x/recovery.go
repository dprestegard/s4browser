package s3x

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"s4browser/internal/engine"
	"s4browser/internal/journal"
)

// This file holds what credential recovery and launch cleanup need from S3: the
// production engine.CredentialProber, journal.RemoteCleaner, and a client
// factory that serves every profile and region from one connection pool.

// NewClients returns a ClientFunc that builds one client per profile and region
// and keeps it. All clients share one tuned HTTP client. credentials returns a
// profile's provider. It should be a long-lived *aws.CredentialsCache such as
// creds.Manager.Provider, which the SDK uses as it is instead of adding a second
// cache. opts are applied to every client after the defaults.
func NewClients(credentials func(profile string) (aws.CredentialsProvider, error), opts ...func(*s3.Options)) ClientFunc {
	f := &clientFactory{credentials: credentials, opts: opts, http: NewHTTPClient(), clients: map[[2]string]*s3.Client{}}
	return f.client
}

type clientFactory struct {
	credentials func(string) (aws.CredentialsProvider, error)
	opts        []func(*s3.Options)
	http        aws.HTTPClient

	mu      sync.Mutex
	clients map[[2]string]*s3.Client
}

func (f *clientFactory) client(_ context.Context, ref engine.ObjectRef) (*s3.Client, error) {
	region := ref.Region
	if region == "" {
		region = DefaultRegion
	}
	id := [2]string{ref.Profile, region}
	f.mu.Lock()
	defer f.mu.Unlock()
	if c := f.clients[id]; c != nil {
		return c, nil
	}
	provider, err := f.credentials(ref.Profile)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", engine.ErrCredentialsUnavailable, err)
	}
	base := func(o *s3.Options) {
		o.Region = region
		o.Credentials = provider
		o.HTTPClient = f.http
	}
	c := s3.New(s3.Options{}, append([]func(*s3.Options){base}, f.opts...)...)
	f.clients[id] = c
	return c, nil
}

// Prober is the production engine.CredentialProber.
type Prober struct {
	refresh func(ctx context.Context, profile string) error
	clients ClientFunc
}

var _ engine.CredentialProber = (*Prober)(nil)

// NewProber returns a prober that first calls refresh, which must discard the
// profile's cached credentials and load them again (creds.Manager.Refresh), and
// then proves them with ProbeAccess on a client from clients. Those clients must
// use the credentials that refresh reloads.
func NewProber(refresh func(ctx context.Context, profile string) error, clients ClientFunc) *Prober {
	return &Prober{refresh: refresh, clients: clients}
}

// Probe implements engine.CredentialProber.
func (p *Prober) Probe(ctx context.Context, ref engine.ObjectRef) error {
	if err := p.refresh(ctx, ref.Profile); err != nil {
		if errors.Is(err, engine.ErrCredentialsUnavailable) {
			return err
		}
		return fmt.Errorf("%w: %w", engine.ErrCredentialsUnavailable, err)
	}
	client, err := p.clients(ctx, ref)
	if err != nil {
		return err
	}
	return ProbeAccess(ctx, client, ref)
}

// ProbeAccess proves that S3 accepts client's credentials with one signed
// request: a one-byte GET of ref. Loading credentials proves nothing, because an
// expired session still loads, so only S3's answer counts.
//
// S3 authenticates a request before it applies policy, so an answer whose error
// code is outside the credential class shows that the credentials work:
// success, NoSuchKey, AccessDenied, InvalidRange (an empty object) and the like.
// A credential code, no answer, a server error or a code-less 400 or 403 is not
// proof. The error is returned and the engine probes again later.
func ProbeAccess(ctx context.Context, client *s3.Client, ref engine.ObjectRef) error {
	const op = "CredentialProbe"
	out, err := client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(ref.Bucket),
		Key:    aws.String(ref.Key),
		Range:  aws.String("bytes=0-0"),
	}, SingleAttemptOptions)
	if err == nil {
		// A connection that breaks while the answer is read proves nothing.
		_, err = io.Copy(io.Discard, io.LimitReader(out.Body, 1<<10))
		_ = out.Body.Close()
		if err != nil {
			return &engine.OpError{Op: op, Class: engine.ClassifyAttempt(ctx, err), Err: err}
		}
		return nil
	}
	mapped := toOpError(op, err)
	var opErr *engine.OpError
	if !errors.As(mapped, &opErr) {
		return mapped // A credential provider failure.
	}
	if opErr.Status != 0 && opErr.Class == engine.ClassPermanent && !engine.IsCodelessAuthFailure(opErr.Status, opErr.Code) {
		return nil
	}
	return mapped
}

// Cleaner is the production journal.RemoteCleaner. Each call makes one attempt,
// because journal.Recovery owns the retries.
type Cleaner struct {
	clients ClientFunc
}

var _ journal.RemoteCleaner = (*Cleaner)(nil)

// NewCleaner returns a Cleaner whose clients come from clients.
func NewCleaner(clients ClientFunc) *Cleaner { return &Cleaner{clients: clients} }

func entryRef(e journal.Entry) engine.ObjectRef {
	return engine.ObjectRef{Profile: e.Profile, Region: e.Region, Bucket: e.Bucket, Key: e.Key}
}

// cleanupError marks credential failures with journal.ErrCredentials, so that
// the entry waits for the profile's credentials instead of being retried now.
func cleanupError(op string, err error) error {
	mapped := toOpError(op, err)
	var opErr *engine.OpError
	if errors.Is(mapped, engine.ErrCredentialsUnavailable) || (errors.As(mapped, &opErr) && opErr.Class == engine.ClassCredential) {
		return fmt.Errorf("%w: %w", journal.ErrCredentials, mapped)
	}
	return mapped
}

// AbortUpload implements journal.RemoteCleaner. NoSuchUpload returns
// journal.ErrUploadGone: the upload was completed or aborted already.
func (c *Cleaner) AbortUpload(ctx context.Context, e journal.Entry, uploadID string) error {
	const op = "AbortMultipartUpload"
	client, err := c.clients(ctx, entryRef(e))
	if err != nil {
		return cleanupError(op, err)
	}
	_, err = client.AbortMultipartUpload(ctx, &s3.AbortMultipartUploadInput{
		Bucket:   aws.String(e.Bucket),
		Key:      aws.String(e.Key),
		UploadId: aws.String(uploadID),
	}, SingleAttemptOptions)
	if err == nil {
		return nil
	}
	mapped := cleanupError(op, err)
	var opErr *engine.OpError
	if errors.As(mapped, &opErr) && (opErr.Code == "NoSuchUpload" || (opErr.Code == "" && opErr.Status == http.StatusNotFound)) {
		return fmt.Errorf("%w: %w", journal.ErrUploadGone, mapped)
	}
	return mapped
}

// ListUploads implements journal.RemoteCleaner. It reads every page.
func (c *Cleaner) ListUploads(ctx context.Context, e journal.Entry) ([]journal.RemoteUpload, error) {
	const op = "ListMultipartUploads"
	client, err := c.clients(ctx, entryRef(e))
	if err != nil {
		return nil, cleanupError(op, err)
	}
	in := &s3.ListMultipartUploadsInput{Bucket: aws.String(e.Bucket), Prefix: aws.String(e.Key)}
	var out []journal.RemoteUpload
	for {
		page, err := client.ListMultipartUploads(ctx, in, SingleAttemptOptions)
		if err != nil {
			return nil, cleanupError(op, err)
		}
		for _, u := range page.Uploads {
			out = append(out, journal.RemoteUpload{Key: aws.ToString(u.Key), UploadID: aws.ToString(u.UploadId), Initiated: aws.ToTime(u.Initiated)})
		}
		if !aws.ToBool(page.IsTruncated) {
			return out, nil
		}
		if aws.ToString(page.NextKeyMarker) == "" && aws.ToString(page.NextUploadIdMarker) == "" {
			return nil, errors.New("s3x: ListMultipartUploads is truncated but gives no marker")
		}
		in.KeyMarker, in.UploadIdMarker = page.NextKeyMarker, page.NextUploadIdMarker
	}
}
