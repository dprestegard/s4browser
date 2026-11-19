package s3x

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/aws/aws-sdk-go-v2/aws"
	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"
)

// Browsing calls (listing buckets and objects, finding a bucket's region) are
// small metadata requests. Unlike part transfers they keep the SDK's standard
// retries, because no engine retry loop sits above them.

// DefaultRegion signs requests that do not yet know a bucket's region. S3
// answers ListBuckets, HeadBucket and GetBucketLocation for any bucket there.
const DefaultRegion = "us-east-1"

// bucketRegionHeader is set by S3 on HeadBucket answers, including 301 and 403.
const bucketRegionHeader = "x-amz-bucket-region"

// Region detection errors.
var (
	// ErrNoSuchBucket means S3 reported that the bucket does not exist.
	ErrNoSuchBucket = errors.New("the bucket does not exist")
	// ErrRegionUnknown means neither HeadBucket nor GetBucketLocation revealed
	// the region, usually because the profile may not call either. The caller
	// can ask the user for the region.
	ErrRegionUnknown = errors.New("the bucket's region could not be detected")
)

// NewBrowseClient returns an S3 client for browsing in region. The HTTP client
// is the tuned one unless opts replace it. Tests use opts to point the client at
// a fake endpoint.
func NewBrowseClient(region string, creds aws.CredentialsProvider, opts ...func(*s3.Options)) *s3.Client {
	base := func(o *s3.Options) {
		o.Region = region
		o.Credentials = creds
		o.HTTPClient = NewHTTPClient()
	}
	return s3.New(s3.Options{}, append([]func(*s3.Options){base}, opts...)...)
}

// DetectRegion finds a bucket's region. client may be signed for any region.
//
// HeadBucket comes first: S3 reports the region in a header even when it
// answers 301 (wrong region) or 403 (no permission on the bucket). If that gives
// nothing, GetBucketLocation is tried. The result is ErrNoSuchBucket, an error
// matching ErrRegionUnknown, or another failure such as missing credentials.
func DetectRegion(ctx context.Context, client *s3.Client, bucket string) (string, error) {
	out, headErr := client.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: aws.String(bucket)})
	if headErr == nil {
		if out.BucketRegion != nil && *out.BucketRegion != "" {
			return *out.BucketRegion, nil
		}
	} else {
		if region := regionFromError(headErr); region != "" {
			return region, nil
		}
		if httpStatus(headErr) == http.StatusNotFound {
			return "", ErrNoSuchBucket
		}
		if !isServiceError(headErr) {
			// No answer from S3 at all: credentials, network or cancellation.
			return "", headErr
		}
	}
	loc, locErr := client.GetBucketLocation(ctx, &s3.GetBucketLocationInput{Bucket: aws.String(bucket)})
	if locErr == nil {
		return normalizeLocation(string(loc.LocationConstraint)), nil
	}
	if apiCode(locErr) == "NoSuchBucket" {
		return "", ErrNoSuchBucket
	}
	if !isServiceError(locErr) {
		return "", locErr
	}
	if headErr == nil {
		headErr = errors.New("HeadBucket returned no region")
	}
	return "", fmt.Errorf("%w: %w; %w", ErrRegionUnknown, headErr, locErr)
}

// normalizeLocation turns a LocationConstraint into a region name.
func normalizeLocation(constraint string) string {
	switch constraint {
	case "":
		return "us-east-1"
	case "EU":
		return "eu-west-1"
	}
	return constraint
}

func regionFromError(err error) string {
	var re *awshttp.ResponseError
	if errors.As(err, &re) && re.Response != nil {
		return re.Response.Header.Get(bucketRegionHeader)
	}
	return ""
}

func httpStatus(err error) int {
	var re *awshttp.ResponseError
	if errors.As(err, &re) {
		return re.HTTPStatusCode()
	}
	return 0
}

func apiCode(err error) string {
	var ae smithy.APIError
	if errors.As(err, &ae) {
		return ae.ErrorCode()
	}
	return ""
}

// isServiceError reports whether S3 answered with an HTTP response.
func isServiceError(err error) bool {
	var re *awshttp.ResponseError
	return errors.As(err, &re)
}

// ErrorCode returns the S3 error code of err, or "" if S3 did not answer with
// one. A HEAD error has no body, so its code is the HTTP status text the SDK
// substitutes, such as "Forbidden".
func ErrorCode(err error) string { return apiCode(err) }

// HTTPStatus returns the HTTP status of an S3 error response, or 0.
func HTTPStatus(err error) int { return httpStatus(err) }

// RegionHint returns the region S3 named in an error response's
// x-amz-bucket-region header, as on a 301 redirect, or "".
func RegionHint(err error) string { return regionFromError(err) }
