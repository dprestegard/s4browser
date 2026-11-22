package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"

	"s4browser/internal/queue"
)

// maxConflictNames bounds the names UploadConflicts returns.
const maxConflictNames = 20

// UploadConflicts reports which of the paths an upload would replace, so the
// page can ask before overwriting and stay out of the way otherwise. A file
// conflicts when an object with its key exists. A folder conflicts when any
// object exists under its key prefix: it may replace some of them, and checking
// each file would mean reading the whole tree. Paths that cannot be uploaded
// (missing, relative) are left for the upload itself to refuse.
//
// If the bucket cannot be checked (for example, no permission to read the keys),
// it returns an error and the page asks, as an unchecked upload may overwrite.
func (a *App) UploadConflicts(req queue.UploadRequest) ([]string, error) {
	ctx, cancel := context.WithTimeout(a.bindingContext(), objectTimeout)
	defer cancel()
	c, err := a.bucketClient(ctx, req.ProfileID, req.Bucket)
	if err != nil {
		return nil, err
	}
	return uploadConflicts(ctx, c, req.Bucket, req.Prefix, req.Paths)
}

func uploadConflicts(ctx context.Context, c *s3.Client, bucket, prefix string, paths []string) ([]string, error) {
	names := []string{}
	for _, p := range paths {
		if !filepath.IsAbs(p) {
			continue
		}
		info, err := os.Stat(p)
		if err != nil {
			continue
		}
		name := filepath.Base(p)
		var exists bool
		if info.IsDir() {
			exists, err = prefixHasObjects(ctx, c, bucket, prefix+name+"/")
		} else {
			exists, err = objectExists(ctx, c, bucket, prefix+name)
		}
		if err != nil {
			return nil, friendly(err)
		}
		if exists {
			names = append(names, name)
			if len(names) == maxConflictNames {
				break
			}
		}
	}
	return names, nil
}

func objectExists(ctx context.Context, c *s3.Client, bucket, key string) (bool, error) {
	_, err := c.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String(bucket), Key: aws.String(key)})
	var nf *types.NotFound
	switch {
	case err == nil:
		return true, nil
	case errors.As(err, &nf):
		return false, nil
	}
	return false, err
}

func prefixHasObjects(ctx context.Context, c *s3.Client, bucket, prefix string) (bool, error) {
	out, err := c.ListObjectsV2(ctx, &s3.ListObjectsV2Input{Bucket: aws.String(bucket), Prefix: aws.String(prefix), MaxKeys: aws.Int32(1)})
	if err != nil {
		return false, err
	}
	return len(out.Contents) > 0, nil
}
