package fsx

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// noLeak fails the test if it leaves goroutines behind.
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

// walkAll lists every path below dir, to prove that nothing was written.
func walkAll(t *testing.T, dir string) []string {
	t.Helper()
	var all []string
	err := filepath.WalkDir(dir, func(p string, _ os.DirEntry, err error) error {
		if err == nil && p != dir {
			all = append(all, p)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return all
}

// unsafeKeys is the corpus of keys that must be refused, with the reason code.
// Each is a whole key as S3 would hold it.
var unsafeKeys = []struct{ key, code string }{
	{"../evil.txt", CodeTraversal},
	{"a/../../evil.txt", CodeTraversal},
	{"a/./b.txt", CodeTraversal},
	{"..", CodeTraversal},
	{"a/..", CodeTraversal},
	{"/etc/passwd", CodeAbsolute},
	{"/abs.txt", CodeAbsolute},
	{"C:/Windows/evil.dll", CodeDrive},
	{"c:evil.txt", CodeDrive},
	{"//server/share/x.txt", CodeUNC},
	{`\\server\share\x.txt`, CodeUNC},
	{`a\b.txt`, CodeBackslash},
	{`..\evil.txt`, CodeBackslash},
	{"a//b.txt", CodeEmptySegment},
	{"a/", CodeEmptySegment},
	{"", CodeEmptySegment},
	{"a<b.txt", CodeForbiddenChar},
	{"a>b.txt", CodeForbiddenChar},
	{"x/a:b.txt", CodeForbiddenChar},
	{`a"b.txt`, CodeForbiddenChar},
	{"a|b.txt", CodeForbiddenChar},
	{"a?b.txt", CodeForbiddenChar},
	{"a*b.txt", CodeForbiddenChar},
	{"dir:stream/file.txt", CodeForbiddenChar},
	{"file.txt:hidden", CodeForbiddenChar},
	{"a\x01b.txt", CodeControlChar},
	{"a\x1fb.txt", CodeControlChar},
	{"a\nb.txt", CodeControlChar},
	{"a\x7fb.txt", CodeControlChar},
	{"dot.", CodeTrailing},
	{"space ", CodeTrailing},
	{"dir./file.txt", CodeTrailing},
	{"dir /file.txt", CodeTrailing},
	{"CON", CodeReserved},
	{"con.txt", CodeReserved},
	{"Prn.tar.gz", CodeReserved},
	{"AUX", CodeReserved},
	{"nul.txt", CodeReserved},
	{"COM1", CodeReserved},
	{"com9.log", CodeReserved},
	{"LPT1.txt", CodeReserved},
	{"lpt9", CodeReserved},
	{"COM¹.txt", CodeReserved},
	{"CONIN$", CodeReserved},
	{"CONOUT$.txt", CodeReserved},
	{"CON .txt", CodeReserved},
	{"dir/NUL/file.txt", CodeReserved},
	{"~s4-0123456789abcdef.s4tmp", CodeReserved},
	{strings.Repeat("n", 256), CodeNameTooLong},
	{"a/" + strings.Repeat("é", 256), CodeNameTooLong},
	{"bad\xffutf8.txt", CodeInvalidText},
}

// safeKeys must be accepted: names that look odd but are fine.
var safeKeys = []string{
	"plain.txt", "a/b/c.txt", "with space.txt", "dots.in.name.txt", ".hidden", "..hidden",
	"console.txt", "COM10.txt", "com.txt", "LPT0.txt", "nul-ish.txt", "auxiliary", "CONSOLE",
	"é/ü.txt", "日本語/ファイル.mkv", "name(1).txt", "a-b_c+d=e,f;g!h@i#j$k%l^m&n'o`p~q.txt",
	"[brackets]{braces}.txt"}

func TestUnsafeKeysAreRefusedWithReasons(t *testing.T) {
	noLeak(t)
	parent := t.TempDir()
	dest := filepath.Join(parent, "dest")
	if err := os.Mkdir(dest, 0o755); err != nil {
		t.Fatal(err)
	}
	p, err := NewPlacer(dest)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range unsafeKeys {
		parts, err := SplitKey(c.key)
		if err == nil {
			for _, part := range parts {
				if err = CheckName(part); err != nil {
					break
				}
			}
		}
		if err == nil {
			_, err = p.Place(parts)
		}
		if err == nil {
			t.Errorf("%q was accepted", c.key)
			continue
		}
		if got := CodeOf(err); got != c.code {
			t.Errorf("%q: code %q (%v), want %q", c.key, got, err, c.code)
		}
		if !errors.Is(err, ErrUnsafePath) {
			t.Errorf("%q: %v does not match ErrUnsafePath", c.key, err)
		}
		if strings.TrimSpace(err.Error()) == "" {
			t.Errorf("%q: empty reason", c.key)
		}
	}
	// Nothing was written anywhere: the destination and its parent are as made.
	if all := walkAll(t, parent); len(all) != 1 || all[0] != dest {
		t.Fatalf("files appeared: %v", all)
	}
}

func TestSafeKeysAreAccepted(t *testing.T) {
	noLeak(t)
	dest := t.TempDir()
	p, err := NewPlacer(dest)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range safeKeys {
		parts, err := SplitKey(key)
		if err != nil {
			t.Errorf("%q: %v", key, err)
			continue
		}
		got, err := p.Place(parts)
		if err != nil {
			t.Errorf("%q: %v", key, err)
			continue
		}
		if rel, _ := filepath.Rel(dest, got); rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			t.Errorf("%q placed outside: %s", key, got)
		}
	}
}

// TestNothingEscapesTheRoot throws combinations of separators and dots at the
// whole pipeline and checks that every accepted path stays inside the root.
func TestNothingEscapesTheRoot(t *testing.T) {
	noLeak(t)
	dest := filepath.Join(t.TempDir(), "root")
	if err := os.Mkdir(dest, 0o755); err != nil {
		t.Fatal(err)
	}
	atoms := []string{"a", "..", ".", "", "/", `\`, ":", "C:", "b.", " ", "...", "%2e%2e", "~", "con"}
	var gen func(prefix string, depth int)
	count := 0
	gen = func(prefix string, depth int) {
		if prefix != "" {
			p, _ := NewPlacer(dest)
			parts, err := SplitKey(prefix)
			if err == nil {
				var got string
				if got, err = p.Place(parts); err == nil {
					count++
					rel, rerr := filepath.Rel(dest, got)
					if rerr != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
						t.Errorf("%q escaped: %s", prefix, got)
					}
					if filepath.VolumeName(got) != filepath.VolumeName(dest) {
						t.Errorf("%q changed volume: %s", prefix, got)
					}
				}
			}
		}
		if depth == 0 {
			return
		}
		for _, a := range atoms {
			gen(prefix+a, depth-1)
		}
	}
	gen("", 4)
	if count == 0 {
		t.Fatal("no combination was accepted, so the check proved nothing")
	}
	if all := walkAll(t, dest); len(all) != 0 {
		t.Fatalf("files appeared: %v", all)
	}
}

func TestPathLength(t *testing.T) {
	noLeak(t)
	dest := t.TempDir()
	p, err := NewPlacer(dest)
	if err != nil {
		t.Fatal(err)
	}
	// The longest path that fits: a final name of 30 characters or more makes the
	// temp file (26 characters) no longer than it, so the final path decides.
	room := MaxPath - utf16Len(dest) - 1
	if room < 40 {
		t.Skip("temp folder path too long to build the case")
	}
	name := strings.Repeat("n", min(room, MaxNameUnits))
	parts := []string{name}
	if room > MaxNameUnits {
		parts = []string{strings.Repeat("d", room-MaxNameUnits-1), name}
	}
	if utf16Len(filepath.Join(append([]string{dest}, parts...)...)) != MaxPath {
		t.Fatalf("test setup: %d", utf16Len(filepath.Join(append([]string{dest}, parts...)...)))
	}
	if _, err := p.Place(parts); err != nil {
		t.Fatalf("a path of exactly MaxPath was refused: %v", err)
	}
	over := append(append([]string{}, parts[:len(parts)-1]...), name+"n")
	if _, err := p.Place(over); CodeOf(err) != CodeTooLong && CodeOf(err) != CodeNameTooLong {
		t.Fatalf("one over: %v", err)
	}
	long := []string{strings.Repeat("d", 100), strings.Repeat("e", 100), strings.Repeat("f", 100), "x.bin"}
	if _, err := p.Place(long); CodeOf(err) != CodeTooLong {
		t.Fatalf("deep path: %v", err)
	}
	// A short final name whose temp file is longer than it: dest\X\x fits, but
	// the 26-character temp name beside it does not, so the temp path decides.
	l := MaxPath - utf16Len(dest) - 1 - 2
	if l > MaxNameUnits {
		t.Skip("destination too short to build the case")
	}
	tight := strings.Repeat("t", l)
	fin := filepath.Join(dest, tight, "x")
	if utf16Len(fin) > MaxPath || utf16Len(TempPath(fin, "0123456789abcdef")) <= MaxPath {
		t.Fatalf("test setup: final %d, temp %d", utf16Len(fin), utf16Len(TempPath(fin, "0123456789abcdef")))
	}
	if _, err := p.Place([]string{tight, "x"}); CodeOf(err) != CodeTooLong || !strings.Contains(err.Error(), "temporary") {
		t.Fatalf("temp path over the limit: %v", err)
	}
	if _, err := p.Place([]string{strings.Repeat("u", l-26), "x"}); err != nil {
		t.Fatalf("a path with room for its temp file was refused: %v", err)
	}
	if all := walkAll(t, dest); len(all) != 0 {
		t.Fatalf("files appeared: %v", all)
	}
}

func TestCaseCollisionsAreRefusedInEitherOrder(t *testing.T) {
	noLeak(t)
	for _, order := range [][2]string{{"a.txt", "A.TXT"}, {"A.TXT", "a.txt"}, {"Dir/File.bin", "dir/file.BIN"}, {"Straße.txt", "STRASSE.txt"}} {
		dest := t.TempDir()
		p, _ := NewPlacer(dest)
		first, _ := SplitKey(order[0])
		second, _ := SplitKey(order[1])
		_, err1 := p.Place(first)
		_, err2 := p.Place(second)
		if order[0] == "Straße.txt" { // Not the same name to NTFS: both are fine.
			if err1 != nil || err2 != nil {
				t.Errorf("%v: %v, %v", order, err1, err2)
			}
			continue
		}
		if err1 != nil {
			t.Errorf("%v: first refused: %v", order, err1)
		}
		if CodeOf(err2) != CodeCollision {
			t.Errorf("%v: second: %v", order, err2)
		}
	}
	// A refused name reserves nothing: it can be asked for again after the first is released by a new Placer only,
	// but a refused collision must not hide a later distinct name.
	dest := t.TempDir()
	p, _ := NewPlacer(dest)
	for _, k := range []string{"x/a.txt", "x/A.txt", "x/b.txt"} {
		parts, _ := SplitKey(k)
		_, err := p.Place(parts)
		if (k == "x/A.txt") != (err != nil) {
			t.Errorf("%s: %v", k, err)
		}
	}
}

func TestFileAndFolderNeverShareAName(t *testing.T) {
	noLeak(t)
	for _, order := range [][2]string{{"a", "a/b.txt"}, {"a/b.txt", "a"}, {"A", "a/b.txt"}, {"a/b.txt", "A"}, {"x/y", "X/y/z.txt"}} {
		dest := t.TempDir()
		p, _ := NewPlacer(dest)
		first, _ := SplitKey(order[0])
		second, _ := SplitKey(order[1])
		if _, err := p.Place(first); err != nil {
			t.Errorf("%v: first: %v", order, err)
		}
		if _, err := p.Place(second); CodeOf(err) != CodeCollision {
			t.Errorf("%v: second: %v", order, err)
		}
	}
}

func TestExistingFilesAreNeverReplaced(t *testing.T) {
	noLeak(t)
	dest := t.TempDir()
	keep := filepath.Join(dest, "keep.txt")
	if err := os.WriteFile(keep, []byte("mine"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dest, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	inSub := filepath.Join(dest, "sub", "in.txt")
	if err := os.WriteFile(inSub, []byte("mine too"), 0o644); err != nil {
		t.Fatal(err)
	}
	p, _ := NewPlacer(dest)
	for _, k := range []string{"keep.txt", "sub/in.txt"} {
		parts, _ := SplitKey(k)
		_, err := p.Place(parts)
		if CodeOf(err) != CodeExists || !errors.Is(err, ErrDestinationExists) {
			t.Errorf("%s: %v", k, err)
		}
	}
	if runtime.GOOS == "windows" { // NTFS ignores case, so a different case is the same file.
		parts, _ := SplitKey("KEEP.TXT")
		if _, err := p.Place(parts); CodeOf(err) != CodeExists {
			t.Errorf("case-different existing file: %v", err)
		}
	}
	// A file where a folder is needed.
	parts, _ := SplitKey("keep.txt/inner.txt")
	if _, err := p.Place(parts); CodeOf(err) != CodeBlocked {
		t.Errorf("file in the way: %v", err)
	}
	// A folder where a file is needed.
	parts, _ = SplitKey("sub")
	if _, err := p.Place(parts); CodeOf(err) != CodeExists {
		t.Errorf("folder in the way: %v", err)
	}
	for path, want := range map[string]string{keep: "mine", inSub: "mine too"} {
		if b, _ := os.ReadFile(path); string(b) != want {
			t.Errorf("%s changed: %q", path, b)
		}
	}
	// New names still work, and make their folders only on request.
	parts, _ = SplitKey("sub/new/deep.txt")
	got, err := p.Place(parts)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dest, "sub", "new")); err == nil {
		t.Fatal("Place created a folder")
	}
	if err := p.MakeParents(got); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(filepath.Dir(got)); err != nil || !info.IsDir() {
		t.Fatalf("MakeParents: %v", err)
	}
	if _, err := os.Lstat(got); err == nil {
		t.Fatal("MakeParents created the file")
	}
}

// makeDirLink makes a link to a folder: a symbolic link if the account may,
// else a junction on Windows. It reports false if neither works.
func makeDirLink(t *testing.T, link, target string) bool {
	t.Helper()
	if err := os.Symlink(target, link); err == nil {
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

func TestLinksAreNotWrittenThrough(t *testing.T) {
	noLeak(t)
	parent := t.TempDir()
	outside := filepath.Join(parent, "outside")
	dest := filepath.Join(parent, "dest")
	for _, d := range []string{outside, dest} {
		if err := os.Mkdir(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if !makeDirLink(t, filepath.Join(dest, "link"), outside) {
		t.Skip("cannot make a symbolic link or junction here")
	}
	p, _ := NewPlacer(dest)
	parts, _ := SplitKey("link/file.txt")
	if _, err := p.Place(parts); CodeOf(err) != CodeLink {
		t.Fatalf("through a link: %v", err)
	}
	// A link that is the destination itself exists, and is not replaced.
	parts, _ = SplitKey("link")
	if _, err := p.Place(parts); CodeOf(err) != CodeExists {
		t.Fatalf("link as destination: %v", err)
	}
	if all := walkAll(t, outside); len(all) != 0 {
		t.Fatalf("something was written through the link: %v", all)
	}
}

func TestPlacerRoot(t *testing.T) {
	noLeak(t)
	if _, err := NewPlacer("relative"); err == nil {
		t.Error("relative root accepted")
	}
	if _, err := NewPlacer(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Error("missing root accepted")
	}
	f := filepath.Join(t.TempDir(), "file")
	_ = os.WriteFile(f, nil, 0o644)
	if _, err := NewPlacer(f); err == nil {
		t.Error("file as root accepted")
	}
	if _, err := (&Placer{}).Place(nil); err == nil {
		t.Error("no names accepted")
	}
}

// TestCheckNameStandsAlone: CheckName is also the last line of defence for a
// name that never went through SplitKey, such as a folder name taken from a path.
func TestCheckNameStandsAlone(t *testing.T) {
	noLeak(t)
	for name, code := range map[string]string{
		`a\b`: CodeBackslash, "a/b": CodeEmptySegment, "": CodeEmptySegment, "..": CodeTraversal, ".": CodeTraversal,
		"c:": CodeForbiddenChar, "NUL": CodeReserved, "x.": CodeTrailing, "bad\xff": CodeInvalidText,
	} {
		if got := CodeOf(CheckName(name)); got != code {
			t.Errorf("%q: %q, want %q", name, got, code)
		}
	}
	if err := CheckName("fine.txt"); err != nil {
		t.Error(err)
	}
}
