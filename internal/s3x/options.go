// Package s3x adapts the AWS SDK to the engine's ObjectStore contract. The tests
// in this package pin the SDK behaviour that docs/engine-design.md relies on, so
// an SDK upgrade that changes it fails here first.
package s3x

import (
	"context"
	"errors"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"s4browser/internal/engine"
)

// SingleAttemptOptions must be passed to every S3 call made for ObjectStore. It
// disables SDK retries, because the engine owns retry policy, and turns off
// response checksum handling, because ranged GETs carry no usable checksum.
func SingleAttemptOptions(o *s3.Options) {
	o.Retryer = aws.NopRetryer{}
	o.ResponseChecksumValidation = aws.ResponseChecksumValidationWhenRequired
	o.DisableLogOutputChecksumValidationSkipped = true
}

// MarkCredentialErrors wraps provider so its failures match
// engine.ErrCredentialsUnavailable. Wrap the base provider and put the
// aws.CredentialsCache outside this wrapper.
func MarkCredentialErrors(provider aws.CredentialsProvider) aws.CredentialsProvider {
	return markedProvider{provider}
}

type markedProvider struct {
	aws.CredentialsProvider
}

func (p markedProvider) Retrieve(ctx context.Context) (aws.Credentials, error) {
	creds, err := p.CredentialsProvider.Retrieve(ctx)
	if err == nil {
		return creds, nil
	}
	if ctxErr := ctx.Err(); ctxErr != nil && errors.Is(err, ctxErr) {
		return aws.Credentials{}, err
	}
	return aws.Credentials{}, fmt.Errorf("%w: %w", engine.ErrCredentialsUnavailable, err)
}
