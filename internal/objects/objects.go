// Package objects runs the object commands that are not transfers: delete,
// create folder, presigned URLs, the metadata view and tag editing. Every call
// takes an S3 client already signed for the bucket's region.
package objects

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// ---- create folder ----

// Errors for bad folder names.
var (
	ErrFolderName   = errors.New("not a valid folder name")
	ErrFolderExists = errors.New("a folder or object with that name already exists here")
)

// FolderKey checks a new folder's name and returns its key under prefix:
// prefix + name + "/".
func FolderKey(prefix, name string) (string, error) {
	switch {
	case name == "":
		return "", fmt.Errorf("%w: enter a name", ErrFolderName)
	case strings.TrimSpace(name) != name:
		return "", fmt.Errorf("%w: it starts or ends with a space", ErrFolderName)
	case strings.ContainsAny(name, `/\`):
		return "", fmt.Errorf("%w: it contains a slash; create one folder at a time", ErrFolderName)
	case name == "." || name == "..":
		return "", fmt.Errorf("%w: %q is reserved", ErrFolderName, name)
	case !utf8.ValidString(name) || strings.IndexFunc(name, unicode.IsControl) >= 0:
		return "", fmt.Errorf("%w: it contains a control character", ErrFolderName)
	}
	key := prefix + name + "/"
	if len(key) > 1024 {
		return "", fmt.Errorf("%w: the key would be longer than S3's 1,024 bytes", ErrFolderName)
	}
	return key, nil
}

// CreateFolder makes the zero-byte placeholder object S3 consoles use for an
// empty folder. It refuses if anything already exists under the folder's key.
func CreateFolder(ctx context.Context, c *s3.Client, bucket, prefix, name string) (string, error) {
	key, err := FolderKey(prefix, name)
	if err != nil {
		return "", err
	}
	out, err := c.ListObjectsV2(ctx, &s3.ListObjectsV2Input{Bucket: aws.String(bucket), Prefix: aws.String(key), MaxKeys: aws.Int32(1)})
	if err != nil {
		return "", err
	}
	if len(out.Contents) > 0 || len(out.CommonPrefixes) > 0 {
		return "", ErrFolderExists
	}
	_, err = c.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String(bucket), Key: aws.String(key), Body: strings.NewReader(""), ContentLength: aws.Int64(0)})
	if err != nil {
		return "", err
	}
	return key, nil
}

// ---- presigned URLs ----

// MaxPresign is S3's limit for a SigV4 presigned URL.
const MaxPresign = 7 * 24 * time.Hour

// minPresign is the shortest URL worth handing out.
const minPresign = time.Minute

// Why a presigned URL expires sooner than requested.
const (
	LimitMaximum     = "maximum"
	LimitCredentials = "credentials"
)

// ErrCredentialsExpiring means the profile's credentials expire within a minute,
// so a URL signed with them would be useless.
var ErrCredentialsExpiring = errors.New("the profile's credentials expire within a minute; refresh them and try again")

// Presigned is a download link for one object.
type Presigned struct {
	URL       string    `json:"url"`
	Expires   time.Time `json:"expires"`
	Requested int64     `json:"requestedSeconds"`
	Effective int64     `json:"effectiveSeconds"`
	// LimitedBy is "" when the URL lasts as long as requested.
	LimitedBy string `json:"limitedBy,omitempty"`
	// CredentialsExpire is when the signing credentials expire, if they do.
	CredentialsExpire *time.Time `json:"credentialsExpire,omitempty"`
}

// Presign signs a GET URL for bucket/key. The URL lasts for the shortest of the
// requested time, seven days and the remaining life of the profile's
// credentials, because S3 refuses a URL whose credentials have expired. The
// credentials are fetched once and the URL is signed with exactly those.
func Presign(ctx context.Context, c *s3.Client, bucket, key string, requested time.Duration, now time.Time) (Presigned, error) {
	if requested < minPresign {
		return Presigned{}, fmt.Errorf("a link must last at least a minute")
	}
	provider := c.Options().Credentials
	if provider == nil {
		return Presigned{}, errors.New("the profile has no credentials")
	}
	creds, err := provider.Retrieve(ctx)
	if err != nil {
		return Presigned{}, err
	}
	out := Presigned{Requested: int64(requested / time.Second)}
	life := requested
	if life > MaxPresign {
		life, out.LimitedBy = MaxPresign, LimitMaximum
	}
	if creds.CanExpire {
		exp := creds.Expires
		out.CredentialsExpire = &exp
		left := exp.Sub(now).Truncate(time.Second)
		if left < minPresign {
			return Presigned{}, ErrCredentialsExpiring
		}
		if left < life {
			life, out.LimitedBy = left, LimitCredentials
		}
	}
	signer := s3.NewPresignClient(c, func(o *s3.PresignOptions) {
		o.Expires = life
		o.ClientOptions = append(o.ClientOptions, func(so *s3.Options) {
			so.Credentials = credentials.StaticCredentialsProvider{Value: creds}
		})
	})
	req, err := signer.PresignGetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(bucket), Key: aws.String(key)})
	if err != nil {
		return Presigned{}, err
	}
	// The signer stamps its own time: read it back so the expiry shown is the URL's.
	signed := now
	if u, perr := url.Parse(req.URL); perr == nil {
		if t, terr := time.Parse("20060102T150405Z", u.Query().Get("X-Amz-Date")); terr == nil {
			signed = t
		}
	}
	out.URL = req.URL
	out.Effective = int64(life / time.Second)
	out.Expires = signed.Add(life).UTC()
	return out, nil
}

// ---- metadata ----

// Metadata is what HeadObject reports about an object.
type Metadata struct {
	Key                string            `json:"key"`
	Size               int64             `json:"size"`
	ETag               string            `json:"etag"`
	Parts              int               `json:"parts,omitempty"`
	LastModified       *time.Time        `json:"lastModified,omitempty"`
	StorageClass       string            `json:"storageClass"`
	ContentType        string            `json:"contentType,omitempty"`
	CacheControl       string            `json:"cacheControl,omitempty"`
	ContentDisposition string            `json:"contentDisposition,omitempty"`
	ContentEncoding    string            `json:"contentEncoding,omitempty"`
	ContentLanguage    string            `json:"contentLanguage,omitempty"`
	Expires            string            `json:"expires,omitempty"`
	VersionID          string            `json:"versionId,omitempty"`
	Encryption         string            `json:"encryption,omitempty"`
	KMSKeyID           string            `json:"kmsKeyId,omitempty"`
	Checksum           string            `json:"checksum,omitempty"` // "CRC32 abc== (composite)"
	TagCount           int               `json:"tagCount"`
	Restore            string            `json:"restore,omitempty"`
	ObjectLock         string            `json:"objectLock,omitempty"`
	Metadata           map[string]string `json:"metadata"`
}

// Head reads an object's metadata.
func Head(ctx context.Context, c *s3.Client, bucket, key string) (Metadata, error) {
	out, err := c.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String(bucket), Key: aws.String(key), ChecksumMode: types.ChecksumModeEnabled})
	if err != nil {
		return Metadata{}, err
	}
	m := Metadata{
		Key: key, Size: aws.ToInt64(out.ContentLength), ETag: aws.ToString(out.ETag), LastModified: out.LastModified,
		StorageClass: string(out.StorageClass), ContentType: aws.ToString(out.ContentType), CacheControl: aws.ToString(out.CacheControl),
		ContentDisposition: aws.ToString(out.ContentDisposition), ContentEncoding: aws.ToString(out.ContentEncoding),
		ContentLanguage: aws.ToString(out.ContentLanguage), Expires: aws.ToString(out.ExpiresString), VersionID: aws.ToString(out.VersionId),
		Encryption: string(out.ServerSideEncryption), KMSKeyID: aws.ToString(out.SSEKMSKeyId), TagCount: int(aws.ToInt32(out.TagCount)),
		Restore: aws.ToString(out.Restore), Metadata: maps.Clone(out.Metadata),
	}
	if m.StorageClass == "" {
		m.StorageClass = "STANDARD" // S3 leaves the header out for STANDARD
	}
	if m.Metadata == nil {
		m.Metadata = map[string]string{}
	}
	if _, n, ok := strings.Cut(strings.Trim(m.ETag, `"`), "-"); ok {
		m.Parts, _ = strconv.Atoi(n)
	}
	for _, ck := range []struct {
		name string
		v    *string
	}{{"CRC64NVME", out.ChecksumCRC64NVME}, {"CRC32", out.ChecksumCRC32}, {"CRC32C", out.ChecksumCRC32C}, {"SHA256", out.ChecksumSHA256}, {"SHA1", out.ChecksumSHA1}} {
		if v := aws.ToString(ck.v); v != "" {
			m.Checksum = ck.name + " " + v
			if t := strings.ToLower(string(out.ChecksumType)); t != "" {
				m.Checksum += " (" + strings.ReplaceAll(t, "_", " ") + ")"
			}
			break
		}
	}
	if out.ObjectLockMode != "" {
		m.ObjectLock = string(out.ObjectLockMode)
		if out.ObjectLockRetainUntilDate != nil {
			m.ObjectLock += " until " + out.ObjectLockRetainUntilDate.UTC().Format(time.RFC3339)
		}
	}
	if out.ObjectLockLegalHoldStatus == types.ObjectLockLegalHoldStatusOn {
		m.ObjectLock = strings.TrimSpace(m.ObjectLock + " legal hold")
	}
	return m, nil
}

// ---- tags ----

// Tag is one object tag.
type Tag struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

// MaxTags is S3's limit per object.
const MaxTags = 10

// ErrTagsChanged means the object's tags changed after they were read, so
// saving would overwrite someone else's edit.
var ErrTagsChanged = errors.New("the object's tags changed since they were loaded; reload them and edit again")

// tagChar reports whether S3 allows r in a tag key or value: letters, digits,
// spaces and + - = . _ : / @.
func tagChar(r rune) bool {
	return unicode.IsLetter(r) || unicode.IsDigit(r) || r == ' ' || strings.ContainsRune("+-=._:/@", r)
}

// ValidateTags applies S3's rules for an object's tag set. The error names the
// first problem.
func ValidateTags(tags []Tag) error {
	if len(tags) > MaxTags {
		return fmt.Errorf("an object can have at most %d tags", MaxTags)
	}
	seen := map[string]bool{}
	for _, t := range tags {
		switch n := utf8.RuneCountInString(t.Key); {
		case n == 0:
			return errors.New("a tag needs a key")
		case n > 128:
			return fmt.Errorf("tag key %q is longer than 128 characters", t.Key[:32]+"…")
		}
		if utf8.RuneCountInString(t.Value) > 256 {
			return fmt.Errorf("the value of %q is longer than 256 characters", t.Key)
		}
		if strings.HasPrefix(strings.ToLower(t.Key), "aws:") {
			return fmt.Errorf("tag key %q: keys starting with \"aws:\" are reserved", t.Key)
		}
		if i := strings.IndexFunc(t.Key, func(r rune) bool { return !tagChar(r) }); i >= 0 {
			return fmt.Errorf("tag key %q contains %q; use letters, digits, spaces and + - = . _ : / @", t.Key, []rune(t.Key[i:])[0])
		}
		if i := strings.IndexFunc(t.Value, func(r rune) bool { return !tagChar(r) }); i >= 0 {
			return fmt.Errorf("the value of %q contains %q; use letters, digits, spaces and + - = . _ : / @", t.Key, []rune(t.Value[i:])[0])
		}
		if seen[t.Key] {
			return fmt.Errorf("tag key %q is used twice", t.Key)
		}
		seen[t.Key] = true
	}
	return nil
}

// Tags returns an object's tags, sorted by key.
func Tags(ctx context.Context, c *s3.Client, bucket, key string) ([]Tag, error) {
	out, err := c.GetObjectTagging(ctx, &s3.GetObjectTaggingInput{Bucket: aws.String(bucket), Key: aws.String(key)})
	if err != nil {
		return nil, err
	}
	tags := make([]Tag, 0, len(out.TagSet))
	for _, t := range out.TagSet {
		tags = append(tags, Tag{Key: aws.ToString(t.Key), Value: aws.ToString(t.Value)})
	}
	sortTags(tags)
	return tags, nil
}

func sortTags(t []Tag) {
	slices.SortFunc(t, func(a, b Tag) int { return strings.Compare(a.Key, b.Key) })
}

func sameTags(a, b []Tag) bool {
	a, b = slices.Clone(a), slices.Clone(b)
	sortTags(a)
	sortTags(b)
	return slices.Equal(a, b)
}

// SetTags replaces an object's tags with after. before is the set the user
// edited: if the object's tags are no longer that set, nothing is written and
// the result is ErrTagsChanged. S3 has no conditional tag write, so a change
// in the moment between the check and the write can still be lost. An empty set
// removes the tags. Tagging does not copy or rewrite the object, so it works
// on objects of any size.
func SetTags(ctx context.Context, c *s3.Client, bucket, key string, before, after []Tag) error {
	if err := ValidateTags(after); err != nil {
		return err
	}
	current, err := Tags(ctx, c, bucket, key)
	if err != nil {
		return err
	}
	if !sameTags(current, before) {
		return ErrTagsChanged
	}
	if len(after) == 0 {
		_, err = c.DeleteObjectTagging(ctx, &s3.DeleteObjectTaggingInput{Bucket: aws.String(bucket), Key: aws.String(key)})
		return err
	}
	set := make([]types.Tag, len(after))
	for i, t := range after {
		set[i] = types.Tag{Key: aws.String(t.Key), Value: aws.String(t.Value)}
	}
	_, err = c.PutObjectTagging(ctx, &s3.PutObjectTaggingInput{Bucket: aws.String(bucket), Key: aws.String(key), Tagging: &types.Tagging{TagSet: set}})
	return err
}
