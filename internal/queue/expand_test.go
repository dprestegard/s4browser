package queue

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"s4browser/internal/engine"
	"s4browser/internal/fsx"
)

// These tests drive the expanders with synthetic folders and listings, and a
// stub in place of the engine, to reach cases a real disk cannot produce on
// demand: a file that vanishes, a folder that cannot be read, a million files.
// The real-disk and fake-S3 tests are in transfers_folders_test.go.

func noLeak(t *testing.T) {
	t.Helper()
	base := runtime.NumGoroutine()
	t.Cleanup(func() {
		deadline := time.Now().Add(5 * time.Second)
		for runtime.NumGoroutine() > base {
			if time.Now().After(deadline) {
				t.Errorf("goroutines: %d, baseline %d", runtime.NumGoroutine(), base)
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	})
}

// ent is a directory entry.
type ent struct {
	name    string
	mode    fs.FileMode
	infoErr error
}

func (e ent) Name() string      { return e.name }
func (e ent) IsDir() bool       { return e.mode.IsDir() }
func (e ent) Type() fs.FileMode { return e.mode.Type() }
func (e ent) Info() (fs.FileInfo, error) {
	if e.infoErr != nil {
		return nil, e.infoErr
	}
	return info{e}, nil
}

type info struct{ e ent }

func (i info) Name() string       { return i.e.name }
func (i info) Size() int64        { return 1 }
func (i info) Mode() fs.FileMode  { return i.e.mode }
func (i info) ModTime() time.Time { return time.Time{} }
func (i info) IsDir() bool        { return i.e.mode.IsDir() }
func (i info) Sys() any           { return nil }

// treeLister serves folders from a map keyed by absolute path.
type treeLister struct {
	dirs map[string][]fs.DirEntry
	errs map[string]error
	// readHook runs when a folder is read.
	readHook func(dir string)
}

func (l treeLister) ReadDir(dir string, batch func([]fs.DirEntry) error) error {
	if l.readHook != nil {
		l.readHook(dir)
	}
	entries, ok := l.dirs[dir]
	if !ok && l.errs[dir] == nil {
		return fs.ErrNotExist
	}
	if len(entries) > 0 {
		if err := batch(entries); err != nil {
			return err
		}
	}
	return l.errs[dir]
}

const fakeRoot = `Z:\media\tree`

func at(rel string) string { return filepath.Join(fakeRoot, filepath.FromSlash(rel)) }

type stub struct {
	calls []string
	err   error
	after int // Fail with err after this many calls. 0 means never.
}

func (s *stub) submit(local, key string) (engine.JobID, error) {
	s.calls = append(s.calls, local+" -> "+key)
	if s.err != nil && len(s.calls) > s.after {
		return "", s.err
	}
	return engine.JobID(fmt.Sprintf("job%06d", len(s.calls))), nil
}

func (s *stub) keys() map[string]bool {
	m := map[string]bool{}
	for _, c := range s.calls {
		m[c[strings.Index(c, " -> ")+4:]] = true
	}
	return m
}

func walkTree(t *testing.T, ctx context.Context, l dirLister, s *stub) *SubmitItem {
	t.Helper()
	item := &SubmitItem{Source: fakeRoot, Kind: KindFolder}
	w := &walker{ctx: ctx, item: item, fsys: l, root: fakeRoot, base: "up/tree/", submit: s.submit}
	if err := w.walk(""); err != nil && item.Error == "" {
		item.fail(CodeStopped, "%v", err)
	}
	return item
}

func codes(item *SubmitItem) map[string]string {
	m := map[string]string{}
	for _, r := range item.Refused {
		m[r.Path] = r.Code
	}
	return m
}

func TestWalkerDecisions(t *testing.T) {
	noLeak(t)
	l := treeLister{dirs: map[string][]fs.DirEntry{
		fakeRoot: {
			ent{name: "a.bin"}, ent{name: "sub", mode: fs.ModeDir}, ent{name: "empty", mode: fs.ModeDir},
			ent{name: "loop", mode: fs.ModeDir | fs.ModeSymlink}, ent{name: "junction", mode: fs.ModeIrregular},
			ent{name: "pipe", mode: fs.ModeNamedPipe}, ent{name: "gone.bin", infoErr: fs.ErrNotExist},
			ent{name: "locked.bin", infoErr: fs.ErrPermission}, ent{name: "bad\xffname.bin"},
			ent{name: "hidden-system.bin"}, ent{name: "unreadable", mode: fs.ModeDir}, ent{name: "partial", mode: fs.ModeDir},
			ent{name: "onlybad", mode: fs.ModeDir},
		},
		at("sub"):        {ent{name: "b.bin"}, ent{name: "deeper", mode: fs.ModeDir}},
		at("sub/deeper"): {ent{name: "c.bin"}},
		at("empty"):      nil,
		at("onlybad"):    {ent{name: "x", mode: fs.ModeSymlink}},
		at("partial"):    {ent{name: "p.bin"}},
	}, errs: map[string]error{
		at("unreadable"): fs.ErrPermission,
		at("partial"):    errors.New("the disk reported an error"),
	}}
	l.dirs[at("unreadable")] = nil
	s := &stub{}
	item := walkTree(t, context.Background(), l, s)
	if item.Error != "" {
		t.Fatalf("item error: %s", item.Error)
	}
	want := map[string]bool{
		"up/tree/a.bin": true, "up/tree/sub/b.bin": true, "up/tree/sub/deeper/c.bin": true,
		"up/tree/hidden-system.bin": true, "up/tree/partial/p.bin": true,
	}
	got := s.keys()
	for k := range want {
		if !got[k] {
			t.Errorf("not queued: %s", k)
		}
	}
	if len(got) != len(want) || item.Queued != len(want) {
		t.Errorf("queued %d (%v), want %d", item.Queued, got, len(want))
	}
	wantCodes := map[string]string{
		at("loop"): fsx.CodeLink, at("junction"): fsx.CodeLink, at("pipe"): CodeNotRegular,
		at("gone.bin"): CodeNotFound, at("locked.bin"): CodeUnreadable, at("bad\xffname.bin"): fsx.CodeInvalidText,
		at("unreadable"): CodeUnreadable, at("partial"): CodeUnreadable, at("onlybad/x"): fsx.CodeLink,
	}
	if gotCodes := codes(item); fmt.Sprint(gotCodes) != fmt.Sprint(wantCodes) {
		t.Errorf("refusals:\n got %v\nwant %v", gotCodes, wantCodes)
	}
	for _, r := range item.Refused {
		if r.Reason == "" {
			t.Errorf("no reason for %s", r.Path)
		}
	}
	// "empty" and "unreadable" have no entries. Only "empty" is an empty folder:
	// "unreadable" failed, and "onlybad" holds a refused entry.
	if item.EmptyFolders != 1 {
		t.Errorf("EmptyFolders = %d, want 1", item.EmptyFolders)
	}
	if item.RefusedTotal != len(wantCodes) {
		t.Errorf("RefusedTotal = %d", item.RefusedTotal)
	}
}

func TestWalkerRootProblems(t *testing.T) {
	noLeak(t)
	// A root that cannot be read is an item error, not a refusal of nothing.
	s := &stub{}
	item := walkTree(t, context.Background(), treeLister{dirs: map[string][]fs.DirEntry{fakeRoot: nil}, errs: map[string]error{fakeRoot: fs.ErrPermission}}, s)
	if item.Code != CodeUnreadable || item.Queued != 0 || item.RefusedTotal != 0 {
		t.Errorf("unreadable root: %+v", item)
	}
	// An empty root queues nothing and says so.
	item = walkTree(t, context.Background(), treeLister{dirs: map[string][]fs.DirEntry{fakeRoot: nil}}, s)
	if item.Error != "" || item.Queued != 0 || item.EmptyFolders != 1 {
		t.Errorf("empty root: %+v", item)
	}
}

func TestWalkerStopsWhenCancelled(t *testing.T) {
	noLeak(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	l := treeLister{dirs: map[string][]fs.DirEntry{
		fakeRoot:  {ent{name: "a"}, ent{name: "b"}, ent{name: "c"}, ent{name: "sub", mode: fs.ModeDir}},
		at("sub"): {ent{name: "d"}},
	}}
	s := &stub{}
	inner := s.submit
	n := 0
	submit := func(local, key string) (engine.JobID, error) {
		if n++; n == 2 {
			cancel()
		}
		return inner(local, key)
	}
	item := &SubmitItem{Kind: KindFolder}
	w := &walker{ctx: ctx, item: item, fsys: l, root: fakeRoot, base: "p/", submit: submit}
	err := w.walk("")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	if item.Queued != 2 || len(s.calls) != 2 {
		t.Fatalf("queued %d after cancel, want 2", item.Queued)
	}
}

func TestWalkerEngineClosedStops(t *testing.T) {
	noLeak(t)
	l := treeLister{dirs: map[string][]fs.DirEntry{fakeRoot: {ent{name: "a"}, ent{name: "b"}, ent{name: "c"}}}}
	s := &stub{err: engine.ErrEngineClosed, after: 1}
	item := &SubmitItem{Kind: KindFolder}
	w := &walker{ctx: context.Background(), item: item, fsys: l, root: fakeRoot, base: "p/", submit: s.submit}
	if err := w.walk(""); !errors.Is(err, engine.ErrEngineClosed) {
		t.Fatalf("err = %v", err)
	}
	if item.Queued != 1 || len(s.calls) != 2 {
		t.Fatalf("queued %d, calls %d", item.Queued, len(s.calls))
	}
	// Any other submit failure refuses that file and carries on.
	s = &stub{err: errors.New("no room"), after: 1}
	item = &SubmitItem{Kind: KindFolder}
	w = &walker{ctx: context.Background(), item: item, fsys: l, root: fakeRoot, base: "p/", submit: s.submit}
	if err := w.walk(""); err != nil || item.Queued != 1 || item.RefusedTotal != 2 || len(s.calls) != 3 {
		t.Fatalf("err %v, %+v", err, item)
	}
}

func TestWalkerKeyLength(t *testing.T) {
	noLeak(t)
	long := strings.Repeat("n", 600)
	l := treeLister{dirs: map[string][]fs.DirEntry{fakeRoot: {ent{name: "ok"}, ent{name: long, mode: fs.ModeDir}}, at(long): {ent{name: long}, ent{name: "short"}}}}
	s := &stub{}
	item := walkTree(t, context.Background(), l, s)
	// "up/tree/" + 600 + "/" + 600 is over 1024 bytes. "short" is under.
	if got := s.keys(); !got["up/tree/ok"] || !got["up/tree/"+long+"/short"] || len(got) != 2 {
		t.Fatalf("queued: %v", got)
	}
	if item.RefusedTotal != 1 || item.Refused[0].Code != CodeKeyTooLong {
		t.Fatalf("refused: %+v", item.Refused)
	}
}

// TestListsAreCapped: counts are exact and the lists never outgrow their caps.
func TestListsAreCapped(t *testing.T) {
	noLeak(t)
	var entries []fs.DirEntry
	for i := range 5000 {
		mode := fs.FileMode(0)
		if i%2 == 0 {
			mode = fs.ModeSymlink
		}
		entries = append(entries, ent{name: fmt.Sprintf("f%05d", i), mode: mode})
	}
	s := &stub{}
	item := walkTree(t, context.Background(), treeLister{dirs: map[string][]fs.DirEntry{fakeRoot: entries}}, s)
	if item.Queued != 2500 || item.RefusedTotal != 2500 || item.Examined != 5000 {
		t.Fatalf("counts: %d %d %d", item.Queued, item.RefusedTotal, item.Examined)
	}
	if len(item.Refused) != MaxListedRefusals || len(item.JobIDs) != MaxListedJobs {
		t.Fatalf("lists: %d refusals, %d jobs", len(item.Refused), len(item.JobIDs))
	}
}

// ---- downloads ----

// listOf serves a fixed list of keys with sizes, in the order given.
func listOf(keys ...string) objectLister {
	return func(ctx context.Context, prefix string, fn func(string, int64) error) error {
		for _, k := range keys {
			if !strings.HasPrefix(k, prefix) {
				continue
			}
			size := int64(10)
			if strings.HasSuffix(k, "/") {
				size = 0
			}
			if strings.HasPrefix(k, "data/") {
				size = 5
			}
			if err := fn(k, size); err != nil {
				return err
			}
		}
		return nil
	}
}

func destPlacer(t *testing.T) (*fsx.Placer, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "dest")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	p, err := fsx.NewPlacer(dir)
	if err != nil {
		t.Fatal(err)
	}
	return p, dir
}

func TestExpandPrefixDecisions(t *testing.T) {
	noLeak(t)
	p, dest := destPlacer(t)
	keys := []string{
		"pre/", "pre/ok.bin", "pre/sub/", "pre/sub/deep.bin",
		"pre/../escape.bin", "pre//abs.bin", "pre/C:/x.bin", "pre/con.txt", "pre/bad:name",
		"pre/Same.bin", "pre/same.BIN", "pre/dir.bin", "pre/dir.bin/inner.bin",
		"pre/trail.", `pre/back\slash`, "pre/\x01ctl",
	}
	list := func(ctx context.Context, prefix string, fn func(string, int64) error) error {
		for _, k := range keys {
			size := int64(10)
			if strings.HasSuffix(k, "/") {
				size = 0
			}
			if err := fn(k, size); err != nil {
				return err
			}
		}
		// A "folder" object that holds data cannot be written as a file.
		return fn("pre/odd/", 7)
	}
	s := &stub{}
	item := &SubmitItem{Kind: KindPrefix}
	if err := expandPrefix(context.Background(), item, p, "pre/", "pre", list, s.submit); err != nil {
		t.Fatal(err)
	}
	wantQueued := map[string]string{
		"pre/ok.bin": filepath.Join(dest, "pre", "ok.bin"), "pre/sub/deep.bin": filepath.Join(dest, "pre", "sub", "deep.bin"),
		"pre/Same.bin": filepath.Join(dest, "pre", "Same.bin"), "pre/dir.bin": filepath.Join(dest, "pre", "dir.bin"),
	}
	if len(s.calls) != len(wantQueued) {
		t.Fatalf("queued %v", s.calls)
	}
	for k, v := range wantQueued {
		if !strings.Contains(strings.Join(s.calls, "\n"), k+" -> "+v) {
			t.Errorf("missing %s -> %s in %v", k, v, s.calls)
		}
	}
	wantCodes := map[string]string{
		"pre/../escape.bin": fsx.CodeTraversal, "pre//abs.bin": fsx.CodeAbsolute, "pre/C:/x.bin": fsx.CodeDrive,
		"pre/con.txt": fsx.CodeReserved, "pre/bad:name": fsx.CodeForbiddenChar, "pre/same.BIN": fsx.CodeCollision,
		"pre/dir.bin/inner.bin": fsx.CodeCollision, "pre/trail.": fsx.CodeTrailing, `pre/back\slash`: fsx.CodeBackslash,
		"pre/\x01ctl": fsx.CodeControlChar, "pre/odd/": CodeFolderKey,
	}
	if got := codes(item); fmt.Sprint(got) != fmt.Sprint(wantCodes) {
		t.Errorf("refusals:\n got %v\nwant %v", got, wantCodes)
	}
	if item.Placeholders != 2 { // "pre/" and "pre/sub/"
		t.Errorf("Placeholders = %d", item.Placeholders)
	}
	if item.Examined != len(keys)+1 {
		t.Errorf("Examined = %d", item.Examined)
	}
	// Only the folders of accepted objects exist.
	var onDisk []string
	_ = filepath.WalkDir(dest, func(path string, d fs.DirEntry, err error) error {
		if rel, _ := filepath.Rel(dest, path); rel != "." {
			onDisk = append(onDisk, filepath.ToSlash(rel))
		}
		return err
	})
	if fmt.Sprint(onDisk) != "[pre pre/sub]" {
		t.Errorf("on disk: %v", onDisk)
	}
}

func TestExpandPrefixEmptyAndFailing(t *testing.T) {
	noLeak(t)
	p, _ := destPlacer(t)
	s := &stub{}
	item := &SubmitItem{Kind: KindPrefix}
	if err := expandPrefix(context.Background(), item, p, "nothing/", "nothing", listOf(), s.submit); err != nil || item.Code != CodeNotFound {
		t.Fatalf("empty prefix: %v %+v", err, item)
	}
	// Only placeholders: not an error.
	item = &SubmitItem{Kind: KindPrefix}
	if err := expandPrefix(context.Background(), item, p, "ph/", "ph", listOf("ph/", "ph/a/"), s.submit); err != nil || item.Error != "" || item.Placeholders != 2 {
		t.Fatalf("placeholders: %v %+v", err, item)
	}
	// The lister failing passes through, and what was queued stays.
	boom := errors.New("listing failed")
	list := func(ctx context.Context, prefix string, fn func(string, int64) error) error {
		_ = fn("x/a.bin", 1)
		return boom
	}
	item = &SubmitItem{Kind: KindPrefix}
	if err := expandPrefix(context.Background(), item, p, "x/", "x", list, s.submit); !errors.Is(err, boom) || item.Queued != 1 {
		t.Fatalf("%v %+v", err, item)
	}
	// Engine closed stops the listing.
	cs := &stub{err: engine.ErrEngineClosed}
	item = &SubmitItem{Kind: KindPrefix}
	if err := expandPrefix(context.Background(), item, p, "y/", "y", listOf("y/a", "y/b"), cs.submit); !errors.Is(err, engine.ErrEngineClosed) || len(cs.calls) != 1 {
		t.Fatalf("%v %d", err, len(cs.calls))
	}
}

func TestDownloadKeyEntryPoints(t *testing.T) {
	noLeak(t)
	p, dest := destPlacer(t)
	s := &stub{}
	submit := func(key, d string) (engine.JobID, error) { return s.submit(d, key) }
	check := func(key, wantCode string) *SubmitItem {
		t.Helper()
		item := &SubmitItem{Source: key}
		if err := downloadKey(context.Background(), item, p, key, listOf("data/x/ok.bin"), submit); err != nil {
			t.Fatal(err)
		}
		if item.Code != wantCode {
			t.Errorf("%q: code %q (%s), want %q", key, item.Code, item.Error, wantCode)
		}
		return item
	}
	// A single object saves under its last name, and the parent names are
	// checked too.
	check("a/b/file.bin", "")
	check("x/../file2.bin", fsx.CodeTraversal)
	check("a:/file3.bin", fsx.CodeDrive)
	check("good/con.txt", fsx.CodeReserved)
	check("bad:dir/ok.bin", fsx.CodeForbiddenChar)
	check("other/file.bin", fsx.CodeCollision) // Same destination as a/b/file.bin.
	check("", fsx.CodeEmptySegment)
	check("/", fsx.CodeEmptySegment)
	check("//", fsx.CodeAbsolute)
	check("a//", fsx.CodeEmptySegment)
	check("../", fsx.CodeTraversal)
	// A folder, by its last name.
	if it := check("data/x/", ""); it.Kind != KindPrefix || it.Queued != 1 {
		t.Errorf("prefix: %+v", it)
	}
	if got := s.calls; len(got) != 2 || !strings.HasSuffix(got[0], "file.bin") || !strings.Contains(got[1], filepath.Join(dest, "x", "ok.bin")) {
		t.Errorf("calls: %v", got)
	}
}

// TestFolderDownloadKeepsSubmitRefusalCode: a file of a folder whose
// destination another download has is refused with CodeExists (review F6).
func TestFolderDownloadKeepsSubmitRefusalCode(t *testing.T) {
	noLeak(t)
	p, _ := destPlacer(t)
	busy := &stub{err: &fsx.PathError{Code: fsx.CodeExists, Msg: "taken"}}
	item := &SubmitItem{Kind: KindPrefix}
	if err := expandPrefix(context.Background(), item, p, "y/", "y", listOf("y/a"), busy.submit); err != nil {
		t.Fatal(err)
	}
	if len(item.Refused) != 1 || item.Refused[0].Code != fsx.CodeExists {
		t.Fatalf("%+v", item)
	}
}
