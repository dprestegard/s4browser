package app

import (
	"bytes"
	"crypto/sha256"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"s4browser/internal/fakes3"
	"s4browser/internal/queue"
)

// Stage 2.3: folder transfers and Windows path safety, through the bindings,
// against the HTTPS fake S3, on real temporary folders. Each test's rig ends with
// a goroutine leak check.

// tree writes files under root: rel path (slash separated) to size. The seed of
// each file is returned, so S3 content can be checked.
func (r *rig) tree(root string, files map[string]int64, emptyDirs ...string) map[string]uint64 {
	r.t.Helper()
	seeds := map[string]uint64{}
	names := make([]string, 0, len(files))
	for rel := range files {
		names = append(names, rel)
	}
	sort.Strings(names)
	for i, rel := range names {
		seed := tSeed + uint64(i) + 1
		seeds[rel] = seed
		path := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			r.t.Fatal(err)
		}
		f, err := os.Create(path)
		if err != nil {
			r.t.Fatal(err)
		}
		buf := make([]byte, 1<<20)
		for off := int64(0); off < files[rel]; off += int64(len(buf)) {
			chunk := buf[:min(int64(len(buf)), files[rel]-off)]
			fakes3.Pattern{Seed: seed}.Fill(chunk, off)
			if _, err := f.Write(chunk); err != nil {
				r.t.Fatal(err)
			}
		}
		if err := f.Close(); err != nil {
			r.t.Fatal(err)
		}
	}
	for _, d := range emptyDirs {
		if err := os.MkdirAll(filepath.Join(root, filepath.FromSlash(d)), 0o755); err != nil {
			r.t.Fatal(err)
		}
	}
	return seeds
}

func (r *rig) uploadFolders(prefix string, paths ...string) queue.SubmitResult {
	r.t.Helper()
	res, err := r.app.QueueUploads(queue.UploadRequest{ProfileID: tProfile, Bucket: fakes3.DefaultBucket, Region: fakes3.Region, Prefix: prefix, Paths: paths})
	if err != nil {
		r.t.Fatal(err)
	}
	return res
}

func (r *rig) downloadKeys(dest string, keys ...string) queue.SubmitResult {
	r.t.Helper()
	res, err := r.app.QueueDownloads(queue.DownloadRequest{ProfileID: tProfile, Bucket: fakes3.DefaultBucket, Region: fakes3.Region, Keys: keys, DestDir: dest})
	if err != nil {
		r.t.Fatal(err)
	}
	return res
}

// waitJobs waits for every job of the items to complete.
func (r *rig) waitJobs(res queue.SubmitResult) {
	r.t.Helper()
	n := 0
	for _, it := range res.Items {
		ids := it.JobIDs
		if it.JobID != "" {
			ids = []string{it.JobID}
		}
		for _, id := range ids {
			r.waitState(id, "completed")
			n++
		}
	}
	if n != res.Queued() {
		r.t.Fatalf("waited for %d jobs, %d were queued", n, res.Queued())
	}
}

// files lists every file below dir (slash separated, relative), and its folders.
func listTree(t *testing.T, dir string) (files, dirs []string) {
	t.Helper()
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		switch {
		case rel == ".":
		case d.IsDir():
			dirs = append(dirs, filepath.ToSlash(rel))
		default:
			files = append(files, filepath.ToSlash(rel))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(files)
	sort.Strings(dirs)
	return files, dirs
}

func hashFile(t *testing.T, path string) [32]byte {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		t.Fatal(err)
	}
	var sum [32]byte
	copy(sum[:], h.Sum(nil))
	return sum
}

// TestFolderRoundTrip: a nested folder goes up and comes back byte for byte.
func TestFolderRoundTrip(t *testing.T) {
	r := newRig(t, rigOptions{})
	src := filepath.Join(r.files, "shoot")
	spec := map[string]int64{
		"a.bin":                       3 * tMiB,
		"zero.dat":                    0,
		"day1/clip1.bin":              11 * tMiB, // Three parts.
		"day1/raw/b.bin":              tMiB,
		"day1/raw/notes.txt":          2048,
		"日本語/ファイル.bin":                4096,
		"with space/x y.bin":          123457,
		"deep/a/b/c/d/e/f/g/leaf.bin": 777,
	}
	seeds := r.tree(src, spec, "day2", "day1/raw/none", "day1/raw/none/deeper")
	prefix := tPrefix + "up/"

	res := r.uploadFolders(prefix, src)
	it := res.Items[0]
	if it.Kind != queue.KindFolder || it.Queued != len(spec) || it.RefusedTotal != 0 || it.Error != "" || it.Examined != len(spec) {
		t.Fatalf("upload item: %+v", it)
	}
	// "day2", "day1/raw/none/deeper": the folders with nothing in them. "none" is
	// not empty (it holds "deeper"), and S3 gets no object for either.
	if it.EmptyFolders != 2 {
		t.Fatalf("EmptyFolders = %d, want 2", it.EmptyFolders)
	}
	if len(it.JobIDs) != len(spec) {
		t.Fatalf("job ids: %d", len(it.JobIDs))
	}
	r.waitJobs(res)
	for rel, size := range spec {
		r.assertVerified(prefix+"shoot/"+rel, size, seeds[rel])
	}
	if n := r.server.ObjectCount(fakes3.DefaultBucket); n != len(spec) {
		t.Fatalf("%d objects on S3, want %d (no folder objects)", n, len(spec))
	}
	r.assertNoUploads()

	dest := t.TempDir()
	dres := r.downloadKeys(dest, prefix+"shoot/")
	dit := dres.Items[0]
	if dit.Kind != queue.KindPrefix || dit.Queued != len(spec) || dit.RefusedTotal != 0 || dit.Error != "" || dit.Placeholders != 0 {
		t.Fatalf("download item: %+v", dit)
	}
	r.waitJobs(dres)

	gotFiles, gotDirs := listTree(t, filepath.Join(dest, "shoot"))
	wantFiles, _ := listTree(t, src)
	if !slices.Equal(gotFiles, wantFiles) {
		t.Fatalf("files:\n got %v\nwant %v", gotFiles, wantFiles)
	}
	for _, rel := range wantFiles {
		if hashFile(t, filepath.Join(src, filepath.FromSlash(rel))) != hashFile(t, filepath.Join(dest, "shoot", filepath.FromSlash(rel))) {
			t.Errorf("%s differs", rel)
		}
		assertFile(t, filepath.Join(dest, "shoot", filepath.FromSlash(rel)), seeds[rel], spec[rel])
	}
	// Folders without files are not recreated, and nothing else is in the way.
	for _, d := range gotDirs {
		if d == "day2" || strings.HasPrefix(d, "day1/raw/none") {
			t.Errorf("empty folder %s was created", d)
		}
	}
	if top, _ := os.ReadDir(dest); len(top) != 1 {
		t.Fatalf("the destination holds %d entries, want only shoot", len(top))
	}
	if left := journalEntries(t, r.dir); len(left) != 0 {
		t.Fatalf("journal entries after completion: %+v", left)
	}
	if n, err := r.app.ClearFinished(false); err != nil || n != 2*len(spec) {
		t.Fatalf("ClearFinished = %d, %v", n, err)
	}
}

// corpus is the unsafe keys, relative to a folder, with the code each is
// refused with. The fsx tests hold the same cases against the path rules alone.
var corpus = []struct{ key, code string }{
	{"../evil.txt", "traversal"}, {"a/../../evil.txt", "traversal"}, {"a/./b.txt", "traversal"}, {"a/..", "traversal"},
	{"/abs.txt", "absolute"}, {"C:/Windows/evil.dll", "drive-path"}, {"c:evil.txt", "drive-path"},
	{"//server/share/x.txt", "unc"}, {`\\server\share\x.txt`, "unc"},
	{`a\b.txt`, "backslash"}, {`..\evil.txt`, "backslash"},
	{"a//b.txt", "empty-segment"},
	{"a<b.txt", "forbidden-char"}, {"a>b.txt", "forbidden-char"}, {"x/a:b.txt", "forbidden-char"}, {`a"b.txt`, "forbidden-char"},
	{"a|b.txt", "forbidden-char"}, {"a?b.txt", "forbidden-char"}, {"a*b.txt", "forbidden-char"}, {"file.txt:hidden", "forbidden-char"},
	{"a\x01b.txt", "control-char"}, {"a\nb.txt", "control-char"}, {"a\x7fb.txt", "control-char"},
	{"dot.", "trailing-dot-space"}, {"space ", "trailing-dot-space"}, {"dir./file.txt", "trailing-dot-space"},
	{"CON", "reserved-name"}, {"con.txt", "reserved-name"}, {"Prn.tar.gz", "reserved-name"}, {"AUX", "reserved-name"},
	{"nul.txt", "reserved-name"}, {"COM1", "reserved-name"}, {"com9.log", "reserved-name"}, {"LPT1.txt", "reserved-name"},
	{"lpt9", "reserved-name"}, {"dir/NUL/file.txt", "reserved-name"},
	{strings.Repeat("n", 256), "name-too-long"},
	{"bad\xffutf8.txt", "invalid-text"},
}

func assertOnlyThese(t *testing.T, parent string, want ...string) {
	t.Helper()
	var got []string
	err := filepath.WalkDir(parent, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if rel, _ := filepath.Rel(parent, p); rel != "." {
			got = append(got, filepath.ToSlash(rel))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(got)
	sort.Strings(want)
	if !slices.Equal(got, want) {
		t.Fatalf("on disk below %s:\n got %v\nwant %v", parent, got, want)
	}
}

// TestUnsafeKeysAreReportedAndNothingIsWritten: every unsafe key, listed in a
// prefix and requested alone, is refused with its reason. The parent of the
// destination is walked afterwards and holds only what was meant to be written.
func TestUnsafeKeysAreReportedAndNothingIsWritten(t *testing.T) {
	r := newRig(t, rigOptions{})
	p := tPrefix + "corpus/"
	for _, c := range corpus {
		if err := r.server.PutListed(fakes3.DefaultBucket, p+c.key, 5, time.Now()); err != nil {
			t.Fatalf("%q: %v", c.key, err)
		}
	}
	for i, k := range []string{"safe/ok1.bin", "ok2.bin"} {
		if err := r.server.PutSynthetic(fakes3.DefaultBucket, p+k, tSeed+uint64(i), 4096); err != nil {
			t.Fatal(err)
		}
	}
	parent := t.TempDir()
	dest := filepath.Join(parent, "dest")
	if err := os.Mkdir(dest, 0o755); err != nil {
		t.Fatal(err)
	}

	// Inside a folder.
	res := r.downloadKeys(dest, p)
	it := res.Items[0]
	if it.Error != "" || it.Queued != 2 || it.RefusedTotal != len(corpus) || it.Examined != len(corpus)+2 {
		t.Fatalf("item: queued %d, refused %d, examined %d, error %q", it.Queued, it.RefusedTotal, it.Examined, it.Error)
	}
	got := map[string]string{}
	for _, f := range it.Refused {
		got[f.Path] = f.Code
		if f.Reason == "" {
			t.Errorf("%q: no reason", f.Path)
		}
	}
	for _, c := range corpus {
		if got[p+c.key] != c.code {
			t.Errorf("%q: code %q, want %q", c.key, got[p+c.key], c.code)
		}
	}
	r.waitJobs(res)
	assertOnlyThese(t, parent, "dest", "dest/corpus", "dest/corpus/ok2.bin", "dest/corpus/safe", "dest/corpus/safe/ok1.bin")
	assertFile(t, filepath.Join(dest, "corpus", "safe", "ok1.bin"), tSeed, 4096)
	assertFile(t, filepath.Join(dest, "corpus", "ok2.bin"), tSeed+1, 4096)

	// As single objects, by their whole key.
	parent2 := t.TempDir()
	dest2 := filepath.Join(parent2, "dest")
	if err := os.Mkdir(dest2, 0o755); err != nil {
		t.Fatal(err)
	}
	var keys []string
	for _, c := range corpus {
		keys = append(keys, c.key)
	}
	keys = append(keys, "")
	sres := r.downloadKeys(dest2, keys...)
	if len(sres.Items) != len(keys) || sres.Queued() != 0 {
		t.Fatalf("single objects: queued %d of %d", sres.Queued(), len(sres.Items))
	}
	for i, c := range corpus {
		if sres.Items[i].Kind != queue.KindObject || sres.Items[i].Code != c.code || sres.Items[i].Error == "" {
			t.Errorf("single %q: %+v, want code %s", c.key, sres.Items[i], c.code)
		}
	}
	if last := sres.Items[len(keys)-1]; last.Code != "empty-segment" {
		t.Errorf("empty key: %+v", last)
	}
	assertOnlyThese(t, parent2, "dest")
	if jobs, _ := r.app.QueueJobs(); len(jobs) != 2 {
		t.Fatalf("refused keys became jobs: %d jobs in the queue, want the 2 from the folder", len(jobs))
	}
}

// TestCollisionsAndExistingFiles: names that differ only in case, a file and a
// folder of one name, and a local file that is already there. Nothing is
// replaced, and the report says which item lost and why.
func TestCollisionsAndExistingFiles(t *testing.T) {
	r := newRig(t, rigOptions{})
	p := tPrefix + "coll/"
	const size = 3000
	objects := map[string]uint64{
		"A.TXT": 1, "a.txt": 2, "Dir/x.bin": 3, "dir/X.BIN": 4, "keep.bin": 5, "f": 6, "f/g.txt": 7, "ok.bin": 8,
	}
	for k, seed := range objects {
		if err := r.server.PutSynthetic(fakes3.DefaultBucket, p+k, seed, size); err != nil {
			t.Fatal(err)
		}
	}
	parent := t.TempDir()
	dest := filepath.Join(parent, "dest")
	if err := os.MkdirAll(filepath.Join(dest, "coll"), 0o755); err != nil {
		t.Fatal(err)
	}
	keep := filepath.Join(dest, "coll", "keep.bin")
	if err := os.WriteFile(keep, []byte("mine"), 0o644); err != nil {
		t.Fatal(err)
	}

	res := r.downloadKeys(dest, p)
	it := res.Items[0]
	got := map[string]string{}
	for _, f := range it.Refused {
		got[f.Path] = f.Code
	}
	// Listing order is by key, so the first of each pair keeps the name.
	want := map[string]string{p + "a.txt": "collision", p + "dir/X.BIN": "collision", p + "keep.bin": "exists", p + "f/g.txt": "collision"}
	if len(got) != len(want) || it.Queued != 4 {
		t.Fatalf("queued %d, refused %v", it.Queued, got)
	}
	for k, c := range want {
		if got[k] != c {
			t.Errorf("%s: %q, want %q", k, got[k], c)
		}
	}
	r.waitJobs(res)
	assertFile(t, filepath.Join(dest, "coll", "A.TXT"), 1, size)
	assertFile(t, filepath.Join(dest, "coll", "Dir", "x.bin"), 3, size)
	assertFile(t, filepath.Join(dest, "coll", "f"), 6, size)
	assertFile(t, filepath.Join(dest, "coll", "ok.bin"), 8, size)
	if b, _ := os.ReadFile(keep); string(b) != "mine" {
		t.Fatalf("the existing file was replaced: %q", b)
	}
	assertOnlyThese(t, parent, "dest", "dest/coll", "dest/coll/A.TXT", "dest/coll/Dir", "dest/coll/Dir/x.bin", "dest/coll/f", "dest/coll/keep.bin", "dest/coll/ok.bin")

	// Single objects collide by their last name, in either order.
	for i, order := range [][]string{{p + "a.txt", p + "A.TXT"}, {p + "A.TXT", p + "a.txt"}} {
		d := t.TempDir()
		sres := r.downloadKeys(d, order...)
		if sres.Items[0].Queued != 1 || sres.Items[1].Queued != 0 || sres.Items[1].Code != "collision" {
			t.Fatalf("order %d: %+v", i, sres.Items)
		}
		r.waitJobs(sres)
		winner := strings.TrimPrefix(order[0], p)
		assertFile(t, filepath.Join(d, winner), objects[winner], size)
		if files, _ := listTree(t, d); len(files) != 1 {
			t.Fatalf("order %d: %v", i, files)
		}
	}
	// A single object over an existing local file is refused, and kept.
	d := t.TempDir()
	if err := os.WriteFile(filepath.Join(d, "keep.bin"), []byte("mine too"), 0o644); err != nil {
		t.Fatal(err)
	}
	sres := r.downloadKeys(d, p+"keep.bin")
	if sres.Queued() != 0 || sres.Items[0].Code != "exists" {
		t.Fatalf("existing file: %+v", sres.Items[0])
	}
	if b, _ := os.ReadFile(filepath.Join(d, "keep.bin")); string(b) != "mine too" {
		t.Fatalf("replaced: %q", b)
	}
}

// TestPlaceholdersAndNestedPrefix: zero-byte folder objects are skipped, and a
// prefix below the top downloads under its own last name.
func TestPlaceholdersAndNestedPrefix(t *testing.T) {
	r := newRig(t, rigOptions{})
	p := tPrefix + "ph/"
	for _, k := range []string{"top/", "top/inner/", "top/inner/f.bin", "top/g.bin"} {
		var err error
		if strings.HasSuffix(k, "/") {
			err = r.server.PutListed(fakes3.DefaultBucket, p+k, 0, time.Now())
		} else {
			err = r.server.PutSynthetic(fakes3.DefaultBucket, p+k, 9, 2048)
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	dest := t.TempDir()
	res := r.downloadKeys(dest, p+"top/inner/", p+"top/")
	if res.Items[0].Queued != 1 || res.Items[0].Placeholders != 1 || res.Items[1].Queued != 2 || res.Items[1].Placeholders != 2 {
		t.Fatalf("items: %+v", res.Items)
	}
	r.waitJobs(res)
	// "inner" downloaded twice: once on its own, once inside "top". The second is
	// the same name in another folder, so there is no clash.
	files, _ := listTree(t, dest)
	if !slices.Equal(files, []string{"inner/f.bin", "top/g.bin", "top/inner/f.bin"}) {
		t.Fatalf("files: %v", files)
	}
	// A prefix with nothing under it is reported, not silently empty.
	nres := r.downloadKeys(dest, p+"nothing/")
	if nres.Queued() != 0 || nres.Items[0].Code != "not-found" {
		t.Fatalf("empty prefix: %+v", nres.Items[0])
	}
}

// makeLink makes a link to a folder: a symbolic link if the account may, else a
// junction. It reports false if neither is possible.
func makeLink(t *testing.T, link, target string) bool {
	t.Helper()
	if os.Symlink(target, link) == nil {
		return true
	}
	if runtime.GOOS == "windows" {
		if out, err := exec.Command("cmd", "/c", "mklink", "/J", link, target).CombinedOutput(); err == nil {
			return true
		} else {
			t.Logf("mklink: %v %s", err, out)
		}
	}
	return false
}

// TestFolderUploadOnDisk: what a real tree does: hidden and system files go up,
// links are not followed (so a loop ends), empty folders make no object, and
// dropped paths are checked.
func TestFolderUploadOnDisk(t *testing.T) {
	r := newRig(t, rigOptions{})
	src := filepath.Join(r.files, "mixed")
	seeds := r.tree(src, map[string]int64{"plain.bin": 1000, "sub/inner.bin": 2000, "hidden.bin": 3000, "system.bin": 4000}, "emptydir")
	if runtime.GOOS == "windows" {
		for name, flag := range map[string]string{"hidden.bin": "+h", "system.bin": "+s"} {
			if out, err := exec.Command("attrib", flag, filepath.Join(src, name)).CombinedOutput(); err != nil {
				t.Fatalf("attrib: %v %s", err, out)
			}
		}
	}
	linked := makeLink(t, filepath.Join(src, "sub", "loop"), src) // A link back to the top: a loop if followed.
	outside := filepath.Join(r.files, "outside")
	r.tree(outside, map[string]int64{"secret.bin": 10})
	linked = makeLink(t, filepath.Join(src, "out"), outside) && linked

	t.Logf("links made: %v", linked)
	prefix := tPrefix + "disk/"
	res := r.uploadFolders(prefix, src)
	it := res.Items[0]
	if it.Error != "" || it.Queued != 4 || it.EmptyFolders != 1 {
		t.Fatalf("item: %+v", it)
	}
	if linked {
		if it.RefusedTotal != 2 || len(it.Refused) != 2 {
			t.Fatalf("links: %+v", it.Refused)
		}
		for _, f := range it.Refused {
			if f.Code != "link" || f.Reason == "" {
				t.Errorf("link refusal: %+v", f)
			}
		}
	}
	r.waitJobs(res)
	for rel, size := range map[string]int64{"plain.bin": 1000, "sub/inner.bin": 2000, "hidden.bin": 3000, "system.bin": 4000} {
		r.assertVerified(prefix+"mixed/"+rel, size, seeds[rel])
	}
	if n := r.server.ObjectCount(fakes3.DefaultBucket); n != 4 {
		t.Fatalf("%d objects on S3, want 4", n)
	}
	if _, ok := r.server.Object(fakes3.DefaultBucket, prefix+"mixed/out/secret.bin"); ok {
		t.Fatal("a file behind a link was uploaded")
	}

	// A dropped link to a folder is followed once, and keeps its own name.
	if makeLink(t, filepath.Join(r.files, "shortcut"), outside) {
		lres := r.uploadFolders(prefix, filepath.Join(r.files, "shortcut"))
		if lres.Items[0].Queued != 1 || lres.Items[0].Error != "" {
			t.Fatalf("dropped link: %+v", lres.Items[0])
		}
		r.waitJobs(lres)
		if _, ok := r.server.Object(fakes3.DefaultBucket, prefix+"shortcut/secret.bin"); !ok {
			t.Fatal("the dropped link's contents were not uploaded under its name")
		}
	}

	// Paths from the drop flow, a mix of files, folders and bad entries.
	drop := describePaths([]string{src, filepath.Join(r.files, "gone"), "relative"})
	if drop[0].Kind != KindFolder {
		t.Fatalf("drop: %+v", drop)
	}
	paths := []string{drop[0].Path, filepath.VolumeName(r.files) + string(filepath.Separator), filepath.Join(r.files, "gone"), "relative", src}
	bad := r.uploadFolders(prefix+"again/", paths...)
	wantCodes := []string{"", "folder-root", "not-found", "relative", "duplicate"}
	for i, c := range wantCodes {
		if bad.Items[i].Code != c {
			t.Errorf("item %d (%s): code %q, want %q (%s)", i, paths[i], bad.Items[i].Code, c, bad.Items[i].Error)
		}
	}
	if bad.Queued() != 4 {
		t.Fatalf("queued %d", bad.Queued())
	}
	r.waitJobs(bad)
	// A folder and a file that would take the same name: the second is refused.
	dupDir := filepath.Join(r.files, "d1", "same")
	dupDir2 := filepath.Join(r.files, "d2", "same")
	r.tree(dupDir, map[string]int64{"a.bin": 10})
	r.tree(dupDir2, map[string]int64{"b.bin": 10})
	dres := r.uploadFolders(prefix+"dups/", dupDir, dupDir2)
	if dres.Items[0].Queued != 1 || dres.Items[1].Code != "duplicate" || dres.Items[1].Queued != 0 {
		t.Fatalf("duplicate roots: %+v", dres.Items)
	}
	r.waitJobs(dres)
	if _, ok := r.server.Object(fakes3.DefaultBucket, prefix+"dups/same/b.bin"); ok {
		t.Fatal("the refused folder was uploaded")
	}
}

// TestSingleFilesStillWork: stage 2.2 behaviour, unchanged: a file uploads under
// its name, and an object downloads under its last name.
func TestSingleFilesStillWork(t *testing.T) {
	r := newRig(t, rigOptions{})
	src := r.pattern("one.bin", tSeed, 2*tMiB)
	res := r.uploadFolders(tPrefix+"single/", src)
	if it := res.Items[0]; it.Kind != queue.KindFile || it.JobID == "" || it.Queued != 1 || it.Examined != 1 || len(it.JobIDs) != 0 {
		t.Fatalf("file item: %+v", it)
	}
	r.waitJobs(res)
	r.assertVerified(tPrefix+"single/one.bin", 2*tMiB, tSeed) // Uploaded content is downloadable once verified.
	dest := t.TempDir()
	dres := r.downloadKeys(dest, tPrefix+"single/one.bin")
	if it := dres.Items[0]; it.Kind != queue.KindObject || it.JobID == "" || it.Queued != 1 {
		t.Fatalf("object item: %+v", it)
	}
	r.waitJobs(dres)
	assertFile(t, filepath.Join(dest, "one.bin"), tSeed, 2*tMiB)
	if !bytes.Equal([]byte(dres.Items[0].Source), []byte(tPrefix+"single/one.bin")) {
		t.Fatal("source not kept")
	}
}
