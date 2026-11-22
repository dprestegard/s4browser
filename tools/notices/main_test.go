package main

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// TestNoticesCoverEverything fails when a Go module linked into the app, or an
// npm package bundled into it, is missing from THIRD_PARTY_NOTICES.txt at the
// version that ships. Run go run ./tools/notices to fix it.
func TestNoticesCoverEverything(t *testing.T) {
	b, err := os.ReadFile("../../" + outFile)
	if err != nil {
		t.Fatal(err)
	}
	notices := string(b)

	mods, err := GoModules("../..")
	if err != nil {
		t.Fatal(err)
	}
	if len(mods) == 0 {
		t.Fatal("no modules found")
	}
	for _, m := range mods {
		if !strings.Contains(notices, "\n"+m.Name+" "+m.Version+"\n") {
			t.Errorf("%s %s is not in %s", m.Name, m.Version, outFile)
		}
		for _, f := range m.Files {
			if !strings.Contains(notices, strings.TrimSpace(strings.ReplaceAll(f.Text, "\r\n", "\n"))) {
				t.Errorf("%s: the text of %s is not in %s", m.Name, f.Rel, outFile)
			}
		}
	}

	lock, err := os.ReadFile("../../frontend/package-lock.json")
	if err != nil {
		t.Fatal(err)
	}
	var l struct {
		Packages map[string]struct {
			Version string `json:"version"`
			Dev     bool   `json:"dev"`
		} `json:"packages"`
	}
	if err := json.Unmarshal(lock, &l); err != nil {
		t.Fatal(err)
	}
	for path, p := range l.Packages {
		if path == "" || p.Dev || !strings.HasPrefix(path, "node_modules/") {
			continue
		}
		line := "\nnpm:" + strings.TrimPrefix(path, "node_modules/") + " " + p.Version + "\n"
		if !strings.Contains(notices, line) {
			t.Errorf("%s %s is not in %s", path, p.Version, outFile)
		}
	}
}
