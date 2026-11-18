package fsx

import (
	"path/filepath"
	"strings"
	"testing"
)

const testJobID = "0123456789abcdef"

func TestTempPathStaysBesideFinalFile(t *testing.T) {
	final := filepath.Join("media", "show", strings.Repeat("n", 250)+".mkv")
	got := TempPath(final, testJobID)
	if filepath.Dir(got) != filepath.Dir(final) {
		t.Fatalf("temp %q is not beside %q", got, final)
	}
	if base := filepath.Base(got); base != "~s4-0123456789abcdef.s4tmp" {
		t.Fatalf("temp name %q", base)
	}
	if !IsTempPath(got) {
		t.Fatalf("IsTempPath rejected %q", got)
	}
}

func TestIsTempPathRejectsOtherFiles(t *testing.T) {
	for _, path := range []string{
		"movie.mkv",
		"movie.s4tmp",
		"~s4-short.s4tmp",
		"~s4-0123456789ABCDEF.s4tmp",
		"~s4-0123456789abcdef.s4tmp.mkv",
		"x~s4-0123456789abcdef.s4tmp",
		"~s4-0123456789abcdef0.s4tmp",
	} {
		if IsTempPath(path) {
			t.Errorf("accepted %q", path)
		}
	}
}
