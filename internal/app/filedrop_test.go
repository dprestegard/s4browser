package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDescribePathsFileFolderAndMixed(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "clip.mkv")
	folder := filepath.Join(dir, "shots")
	missing := filepath.Join(dir, "gone.txt")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(folder, 0o755); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name  string
		paths []string
		kinds []string
	}{
		{"one file", []string{file}, []string{KindFile}},
		{"one folder", []string{folder}, []string{KindFolder}},
		{"mixed", []string{file, folder, missing}, []string{KindFile, KindFolder, KindMissing}},
		{"nothing", nil, nil},
	}
	for _, c := range cases {
		got := describePaths(c.paths)
		if len(got) != len(c.kinds) {
			t.Fatalf("%s: %d items", c.name, len(got))
		}
		for i, it := range got {
			if it.Kind != c.kinds[i] || it.Path != c.paths[i] || !it.Absolute {
				t.Errorf("%s[%d]: %+v", c.name, i, it)
			}
		}
	}
	if it := describePaths([]string{"relative.txt"})[0]; it.Absolute {
		t.Errorf("a relative path must be flagged: %+v", it)
	}
	if it := describePaths([]string{missing})[0]; it.Error == "" {
		t.Errorf("a missing path must carry its error: %+v", it)
	}
}

func TestDropLogRecordsEveryPath(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "a.bin")
	if err := os.WriteFile(file, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	logFile := filepath.Join(dir, "nested", "filedrop.log") // the directory does not exist yet
	l, err := openDropLog(logFile)
	if err != nil {
		t.Fatal(err)
	}
	drop := Drop{At: time.Date(2026, 11, 18, 1, 2, 3, 0, time.UTC), X: 10, Y: 20, Items: describePaths([]string{file, dir})}
	if err := l.Write(drop); err != nil {
		t.Fatal(err)
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	// Reopening appends rather than truncates.
	l, err = openDropLog(logFile)
	if err != nil {
		t.Fatal(err)
	}
	if err := l.Write(drop); err != nil {
		t.Fatal(err)
	}
	_ = l.Close()

	data, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	if strings.Count(text, "drop at (10,20), 2 item(s)") != 2 {
		t.Fatalf("log:\n%s", text)
	}
	if strings.Count(text, "file    absolute=true "+file) != 2 || strings.Count(text, "folder  absolute=true "+dir) != 2 {
		t.Fatalf("log:\n%s", text)
	}
}
