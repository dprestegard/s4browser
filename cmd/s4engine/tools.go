package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"time"

	"s4browser/internal/fakes3"
)

// hashFile returns the SHA-256 and size of a file.
func hashFile(ctx context.Context, path string) (sum string, size int64, err error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	buf := make([]byte, 4<<20)
	for {
		if err := ctx.Err(); err != nil {
			return "", 0, err
		}
		n, rerr := f.Read(buf)
		h.Write(buf[:n])
		size += int64(n)
		if rerr == io.EOF {
			return hex.EncodeToString(h.Sum(nil)), size, nil
		}
		if rerr != nil {
			return "", 0, rerr
		}
	}
}

type verifyResult struct {
	Command   string     `json:"command"`
	Match     bool       `json:"match"`
	A         verifyFile `json:"a"`
	B         verifyFile `json:"b"`
	DurationS float64    `json:"durationSeconds"`
}

type verifyFile struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Bytes  int64  `json:"bytes"`
}

// cmdVerify compares the SHA-256 of two local files. Exit 0 means identical.
func cmdVerify(ctx context.Context, e env, args []string) (int, error) {
	fs := newFlagSet("verify", e)
	resultPath := fs.String("result", "", "optional JSON result file")
	positional, err := parseFlags(fs, args)
	if err != nil {
		return exitUsage, err
	}
	if len(positional) != 2 {
		return exitUsage, usagef("give exactly two files to compare")
	}
	start := time.Now()
	var files [2]verifyFile
	var errs [2]error
	var wg sync.WaitGroup
	for i, p := range positional {
		wg.Add(1)
		go func() {
			defer wg.Done()
			files[i].Path = p
			files[i].SHA256, files[i].Bytes, errs[i] = hashFile(ctx, p)
		}()
	}
	wg.Wait()
	if err := errors.Join(errs[:]...); err != nil {
		if ctx.Err() != nil {
			return exitInterrupted, nil
		}
		return exitFailed, err
	}
	match := files[0].SHA256 == files[1].SHA256 && files[0].Bytes == files[1].Bytes
	res := verifyResult{Command: "verify", Match: match, A: files[0], B: files[1], DurationS: time.Since(start).Seconds()}
	for _, f := range files {
		fmt.Fprintf(e.stdout, "%s  %s  (%d bytes)\n", f.SHA256, f.Path, f.Bytes)
	}
	if *resultPath != "" {
		if err := writeJSON(*resultPath, res); err != nil {
			return exitFailed, err
		}
	}
	if !match {
		fmt.Fprintln(e.stdout, "MISMATCH")
		return exitFailed, nil
	}
	fmt.Fprintln(e.stdout, "MATCH")
	return exitOK, nil
}

// cmdGenerate writes a deterministic file: the 8-byte word at offset 8*i is
// SplitMix64(seed+i), the pattern internal/fakes3 uses, so the same size and
// seed always give the same bytes.
func cmdGenerate(ctx context.Context, e env, args []string) (int, error) {
	fs := newFlagSet("generate", e)
	out := fs.String("out", "", "file to create (must not exist)")
	sizeFlag := fs.String("size", "", "size, e.g. 10GB or 1GiB")
	seed := fs.Uint64("seed", 1, "content seed")
	positional, err := parseFlags(fs, args)
	if err != nil {
		return exitUsage, err
	}
	if len(positional) != 0 || *out == "" || *sizeFlag == "" {
		return exitUsage, usagef("usage: generate --out FILE --size SIZE [--seed N]")
	}
	size, err := parseSize(*sizeFlag)
	if err != nil {
		return exitUsage, usagef("--size: %v", err)
	}
	f, err := os.OpenFile(*out, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return exitUsage, usagef("%s already exists; generate never overwrites", *out)
		}
		return exitFailed, err
	}
	pattern := fakes3.Pattern{Seed: *seed}
	h := sha256.New()
	buf := make([]byte, 4<<20)
	start := time.Now()
	var written int64
	werr := func() error {
		for written < size {
			if err := ctx.Err(); err != nil {
				return err
			}
			chunk := buf[:min(int64(len(buf)), size-written)]
			pattern.Fill(chunk, written)
			h.Write(chunk)
			if _, err := f.Write(chunk); err != nil {
				return err
			}
			written += int64(len(chunk))
		}
		return nil
	}()
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		_ = os.Remove(*out)
		if ctx.Err() != nil {
			return exitInterrupted, nil
		}
		return exitFailed, werr
	}
	fmt.Fprintf(e.stdout, "%s  %s  (%d bytes, seed %d, %.1fs)\n", hex.EncodeToString(h.Sum(nil)), *out, size, *seed, time.Since(start).Seconds())
	return exitOK, nil
}

type cleanupResult struct {
	Command        string    `json:"command"`
	Target         targetOut `json:"target"`
	DryRun         bool      `json:"dryRun"`
	UploadsFound   int       `json:"multipartUploadsFound"`
	UploadsAborted int       `json:"multipartUploadsAborted"`
	ObjectsFound   int       `json:"objectsFound"`
	ObjectsDeleted int       `json:"objectsDeleted"`
	Errors         []string  `json:"errors,omitempty"`
}

// cmdCleanup aborts multipart uploads and deletes objects under
// s4browser-test/<run-id>/. Listing, aborting and deleting all go through the
// guard, so nothing outside that prefix can be touched.
func cmdCleanup(ctx context.Context, e env, args []string) (int, error) {
	var c commonFlags
	fs := newFlagSet("cleanup", e)
	c.register(fs)
	dry := fs.Bool("dry-run", false, "list what would be aborted and deleted, change nothing")
	positional, err := parseFlags(fs, args)
	if err != nil {
		return exitUsage, err
	}
	if len(positional) != 0 {
		return exitUsage, usagef("cleanup takes no arguments")
	}
	guard, err := c.validate("cleanup", true)
	if err != nil {
		return exitUsage, err
	}
	tgt, err := newTarget(ctx, &c, guard, e)
	if err != nil {
		return exitFailed, err
	}
	defer tgt.close()

	res := cleanupResult{
		Command: "cleanup", DryRun: *dry,
		Target: targetOut{Kind: tgt.kind, Bucket: tgt.bucket, Region: tgt.region, Profile: tgt.profile, RunID: c.runID, Prefix: guard.Prefix()},
	}
	fail := func(format string, args ...any) {
		msg := fmt.Sprintf(format, args...)
		res.Errors = append(res.Errors, msg)
		fmt.Fprintln(e.stderr, msg)
	}

	uploads, err := tgt.admin.ListUploads(ctx, tgt.bucket, guard.Prefix())
	if err != nil {
		return exitFailed, fmt.Errorf("list multipart uploads: %w", err)
	}
	res.UploadsFound = len(uploads)
	for _, u := range uploads {
		fmt.Fprintf(e.stdout, "multipart upload  %s  %s\n", u.Key, u.ID)
		if *dry {
			continue
		}
		if err := tgt.admin.AbortUpload(ctx, tgt.bucket, u.Key, u.ID); err != nil {
			fail("abort %s %s: %v", u.Key, u.ID, err)
			continue
		}
		res.UploadsAborted++
	}
	keys, err := tgt.admin.ListObjects(ctx, tgt.bucket, guard.Prefix())
	if err != nil {
		return exitFailed, fmt.Errorf("list objects: %w", err)
	}
	res.ObjectsFound = len(keys)
	for _, key := range keys {
		fmt.Fprintf(e.stdout, "object            %s\n", key)
		if *dry {
			continue
		}
		if err := tgt.admin.DeleteObject(ctx, tgt.bucket, key); err != nil {
			fail("delete %s: %v", key, err)
			continue
		}
		res.ObjectsDeleted++
	}
	if *dry {
		fmt.Fprintf(e.stdout, "dry run: %d multipart upload(s) and %d object(s) under %s\n", res.UploadsFound, res.ObjectsFound, guard.Prefix())
	} else {
		fmt.Fprintf(e.stdout, "aborted %d of %d multipart upload(s), deleted %d of %d object(s) under %s\n",
			res.UploadsAborted, res.UploadsFound, res.ObjectsDeleted, res.ObjectsFound, guard.Prefix())
	}
	if werr := writeJSON(c.result, res); werr != nil {
		fmt.Fprintf(e.stderr, "write result: %v\n", werr)
	}
	switch {
	case ctx.Err() != nil:
		return exitInterrupted, nil
	case len(res.Errors) > 0:
		return exitFailed, nil
	}
	return exitOK, nil
}
