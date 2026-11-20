package queue

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"

	"s4browser/internal/engine"
	"s4browser/internal/fsx"
)

// Folder upload: how a local tree is read.
//
//   - Files are queued as they are found, in batches of dirBatch directory
//     entries, so memory does not grow with the size of the tree. It holds one
//     batch, the names of the sub-folders of the folders on the current path, and
//     the lists capped in SubmitItem.
//   - A folder with nothing in it creates no object (S3 has no folders): it is
//     counted in EmptyFolders. A folder holding only refused items is not empty.
//   - Symbolic links, junctions and other reparse-point folders inside the tree
//     are not followed and not uploaded: each is refused with code "link". That
//     makes a loop impossible. A link the user dropped directly is followed once.
//   - Hidden and system files are uploaded like any other. The user chose the
//     folder, and the attributes say nothing about whether they matter.
//   - Files are not opened while expanding. A file that vanishes or is locked
//     after expansion fails its own job with the engine's reason and can be
//     retried. A file that vanishes during expansion, or whose entry cannot be
//     read, and a folder that cannot be listed, are refused with codes "not-found"
//     and "unreadable", and the rest of the tree continues.
//   - Devices, pipes and sockets are refused with code "not-regular".

// dirBatch is how many directory entries are read at a time.
const dirBatch = 1024

// dirLister reads a folder in batches. It returns what ReadDir returned before
// an error, so a folder that fails halfway still yields its first entries.
type dirLister interface {
	ReadDir(dir string, batch func([]fs.DirEntry) error) error
}

// osLister reads real folders.
type osLister struct{}

func (osLister) ReadDir(dir string, batch func([]fs.DirEntry) error) error {
	f, err := os.Open(dir)
	if err != nil {
		return unwrapPath(err)
	}
	defer f.Close()
	for {
		entries, err := f.ReadDir(dirBatch)
		if len(entries) > 0 {
			if berr := batch(entries); berr != nil {
				return berr
			}
		}
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return unwrapPath(err)
		}
	}
}

type walker struct {
	ctx    context.Context
	item   *SubmitItem
	fsys   dirLister
	root   string // Absolute path of the folder being uploaded.
	base   string // Key prefix of its files: "<prefix><folder name>/".
	submit func(local, key string) (engine.JobID, error)
	fatal  error // Set when the walk must stop: cancelled or engine closed.
}

// walk uploads the folder at rel ("" is the root, otherwise slash-separated).
func (w *walker) walk(rel string) error {
	dir := w.root
	if rel != "" {
		dir = filepath.Join(w.root, filepath.FromSlash(rel))
	}
	count := 0
	var subdirs []string
	err := w.fsys.ReadDir(dir, func(entries []fs.DirEntry) error {
		for _, e := range entries {
			if w.fatal = w.ctx.Err(); w.fatal != nil {
				return w.fatal
			}
			count++
			if sub := w.entry(rel, e); sub != "" {
				subdirs = append(subdirs, sub)
			}
			if w.fatal != nil {
				return w.fatal
			}
		}
		return nil
	})
	if w.fatal != nil {
		return w.fatal
	}
	switch {
	case err != nil && rel == "" && count == 0:
		w.item.fail(CodeUnreadable, "cannot read the folder: %v", err)
		return nil
	case err != nil:
		w.item.refuse(dir, CodeUnreadable, fmt.Sprintf("this folder cannot be fully read: %v", err))
	case count == 0:
		w.item.EmptyFolders++
	}
	for _, name := range subdirs {
		if err := w.walk(joinRel(rel, name)); err != nil {
			return err
		}
	}
	return nil
}

func joinRel(rel, name string) string {
	if rel == "" {
		return name
	}
	return rel + "/" + name
}

// entry handles one directory entry. It returns the name of a sub-folder to walk
// afterwards, or "".
func (w *walker) entry(rel string, e fs.DirEntry) string {
	name := e.Name()
	local := filepath.Join(w.root, filepath.FromSlash(joinRel(rel, name)))
	typ := e.Type()
	switch {
	case typ&(fs.ModeSymlink|fs.ModeIrregular) != 0:
		w.item.Examined++
		w.item.refuse(local, fsx.CodeLink, "this is a link or junction. Links are not followed, so it was not uploaded")
		return ""
	case typ.IsDir():
		return name
	case !typ.IsRegular():
		w.item.Examined++
		w.item.refuse(local, CodeNotRegular, "this is a device or other special file, not a regular file")
		return ""
	}
	w.item.Examined++
	if _, err := e.Info(); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			w.item.refuse(local, CodeNotFound, "the file disappeared while the folder was being read")
		} else {
			w.item.refuse(local, CodeUnreadable, fmt.Sprintf("cannot read the file: %v", unwrapPath(err)))
		}
		return ""
	}
	if !utf8.ValidString(name) {
		w.item.refuse(local, fsx.CodeInvalidText, "the file name is not valid text, so it has no valid object name")
		return ""
	}
	key := w.base + joinRel(rel, name)
	if len(key) > maxKeyBytes {
		w.item.refuse(local, CodeKeyTooLong, fmt.Sprintf("the object name would be longer than S3's limit of %d bytes", maxKeyBytes))
		return ""
	}
	id, err := w.submit(local, key)
	if err != nil {
		if errors.Is(err, engine.ErrEngineClosed) {
			w.fatal = err
			return ""
		}
		w.item.refuse(local, CodeStopped, err.Error())
		return ""
	}
	w.item.queued(id)
	return ""
}

// Folder download: how a prefix is read.
//
//   - The prefix is listed without a delimiter, page by page, and each object is
//     queued as it arrives. No list of the objects is kept.
//   - Each object goes to "<DestDir>\<folder name>\<relative key>", where the folder
//     name is the last name of the prefix. Every name is checked by fsx.Placer,
//     which refuses an unsafe, colliding or existing destination and never
//     renames. The folders an accepted object needs are created at that moment, so
//     a refused object leaves nothing on disk. A folder created for a job that
//     is cancelled or fails later stays, empty.
//   - A zero-byte object whose key ends in "/" is a folder placeholder. It is
//     skipped and counted in Placeholders. Empty folders are not created locally.
//     An object that ends in "/" but has data cannot be a file and is refused with
//     code "folder-key".
//   - Collisions are decided in listing order: the first object keeps the name,
//     later ones that equal it ignoring letter case (or need it as a folder, or as
//     a file) are refused.
//   - Memory grows with the number of objects, about 40 bytes each, for the
//     collision check (see fsx.Placer). Nothing else does.

// objectLister calls fn for every object below prefix, in key order, and stops
// at the first error fn returns.
type objectLister func(ctx context.Context, prefix string, fn func(key string, size int64) error) error

func expandPrefix(ctx context.Context, item *SubmitItem, placer *fsx.Placer, prefix, folder string, list objectLister, submit func(key, dest string) (engine.JobID, error)) error {
	err := list(ctx, prefix, func(key string, size int64) error {
		item.Examined++
		rel := strings.TrimPrefix(key, prefix)
		if rel == "" || strings.HasSuffix(rel, "/") {
			if size == 0 {
				item.Placeholders++
				return nil
			}
			item.refuse(key, CodeFolderKey, fmt.Sprintf("the object name ends with a slash but the object holds %d bytes, so it cannot be a file", size))
			return nil
		}
		parts, err := fsx.SplitKey(rel)
		var dest string
		if err == nil {
			dest, err = placer.Place(append([]string{folder}, parts...))
		}
		if err == nil {
			err = placer.MakeParents(dest)
		}
		if err != nil {
			code := fsx.CodeOf(err)
			if code == "" {
				code = fsx.CodeBlocked
			}
			item.refuse(key, code, err.Error())
			return nil
		}
		id, err := submit(key, dest)
		if err != nil {
			if errors.Is(err, engine.ErrEngineClosed) {
				return err
			}
			item.refuse(key, CodeStopped, err.Error())
			return nil
		}
		item.queued(id)
		return nil
	})
	if err == nil && item.Examined == 0 {
		item.fail(CodeNotFound, "no objects were found under this folder")
	}
	return err
}

// listObjects lists every object below prefix, one request per 1000 keys, with
// the SDK's standard retries. Keys are requested URL-encoded, because a key may
// hold characters XML cannot carry.
func (s *Service) listObjects(ctx context.Context, ref engine.ObjectRef, prefix string, fn func(key string, size int64) error) error {
	client, err := s.clients(ctx, ref)
	if err != nil {
		return fmt.Errorf("cannot list %s: %w", ref.Bucket, err)
	}
	var token *string
	for {
		page, err := client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
			Bucket:            aws.String(ref.Bucket),
			Prefix:            aws.String(prefix),
			EncodingType:      types.EncodingTypeUrl,
			ContinuationToken: token,
		})
		if err != nil {
			if ctx.Err() != nil {
				return context.Cause(ctx)
			}
			return fmt.Errorf("cannot list %s/%s: %w", ref.Bucket, prefix, err)
		}
		for _, o := range page.Contents {
			key := aws.ToString(o.Key)
			if page.EncodingType == types.EncodingTypeUrl {
				if key, err = url.QueryUnescape(key); err != nil {
					return fmt.Errorf("S3 returned an object name that cannot be decoded: %w", err)
				}
			}
			if err := fn(key, aws.ToInt64(o.Size)); err != nil {
				return err
			}
		}
		if !aws.ToBool(page.IsTruncated) || aws.ToString(page.NextContinuationToken) == "" {
			return nil
		}
		token = page.NextContinuationToken
	}
}
