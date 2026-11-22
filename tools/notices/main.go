// Command notices writes THIRD_PARTY_NOTICES.txt: the license texts and
// notices of everything that ships inside s4browser.
//
// That is the Go standard library, every Go module linked into the app on
// macOS or Windows (with their nested license files), the npm packages bundled
// into the frontend, and Vite's module-preload helper, which Vite adds to the
// bundle. Development tools (Wails CLI, Vite itself, TypeScript) do not ship and
// are not listed.
//
// Run it from the repository root after changing dependencies, with
// frontend/node_modules installed:
//
//	go run ./tools/notices          # rewrite THIRD_PARTY_NOTICES.txt
//	go run ./tools/notices -check   # fail if the file is out of date
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

const outFile = "THIRD_PARTY_NOTICES.txt"

// buildTags are the tags a release build uses for a release. On Windows the
// WebView2 loader is the pure-Go one; the tag native_webview2loader, which would
// embed Microsoft's WebView2Loader.dll, is never set.
const buildTags = "production"

// targets are the platforms the app is released for.
var targets = []string{"darwin", "windows"}

// licenseFile matches the files that carry license terms or notices.
var licenseFile = regexp.MustCompile(`(?i)^(licen[cs]e|copying|notice|patents|unlicense)(\.(txt|md))?$`)

// Component is one shipped piece of third-party software.
type Component struct {
	Name    string
	Version string
	Files   []File // license and notice files, in a stable order
}

// File is one license or notice file of a component.
type File struct {
	Rel  string // path inside the component, e.g. "LICENSE" or "internal/x/LICENSE"
	Text string
}

func main() {
	check := flag.Bool("check", false, "fail if "+outFile+" is out of date instead of writing it")
	flag.Parse()

	comps, err := components()
	if err != nil {
		fail(err)
	}
	text := render(comps)
	if *check {
		old, err := os.ReadFile(outFile)
		if err != nil {
			fail(err)
		}
		if string(old) != text {
			fail(fmt.Errorf("%s is out of date: run go run ./tools/notices", outFile))
		}
		return
	}
	if err := os.WriteFile(outFile, []byte(text), 0o644); err != nil {
		fail(err)
	}
	fmt.Printf("wrote %s: %d components\n", outFile, len(comps))
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "notices:", err)
	os.Exit(1)
}

// components returns everything that ships, Go first, then npm.
func components() ([]Component, error) {
	std, err := goStdlib()
	if err != nil {
		return nil, err
	}
	mods, err := GoModules(".")
	if err != nil {
		return nil, err
	}
	npm, err := npmPackages("frontend")
	if err != nil {
		return nil, err
	}
	out := append([]Component{std}, mods...)
	return append(out, npm...), nil
}

func goStdlib() (Component, error) {
	root, err := exec.Command("go", "env", "GOROOT").Output()
	if err != nil {
		return Component{}, fmt.Errorf("go env GOROOT: %w", err)
	}
	ver, err := exec.Command("go", "env", "GOVERSION").Output()
	if err != nil {
		return Component{}, fmt.Errorf("go env GOVERSION: %w", err)
	}
	files, err := rootFiles(strings.TrimSpace(string(root)))
	if err != nil {
		return Component{}, err
	}
	return Component{Name: "Go standard library and runtime", Version: strings.TrimSpace(string(ver)), Files: files}, nil
}

// goPackage is the part of `go list -json` output that is used here.
type goPackage struct {
	Dir    string
	Module *struct {
		Path    string
		Version string
		Dir     string
		Main    bool
	}
}

// GoModules returns the modules linked into the package at dir on any release
// target, with the license files at each module's root and any nested ones in
// the directories of linked packages.
func GoModules(dir string) ([]Component, error) {
	type mod struct {
		c    Component
		dir  string
		seen map[string]bool
	}
	mods := map[string]*mod{}
	for _, goos := range targets {
		cmd := exec.Command("go", "list", "-deps", "-json=Dir,Module", "-tags", buildTags, ".")
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GOOS="+goos, "GOARCH="+map[string]string{"darwin": "arm64", "windows": "amd64"}[goos], "CGO_ENABLED=1")
		out, err := cmd.Output()
		if err != nil {
			return nil, fmt.Errorf("go list for %s: %w", goos, err)
		}
		dec := json.NewDecoder(bytes.NewReader(out))
		for dec.More() {
			var p goPackage
			if err := dec.Decode(&p); err != nil {
				return nil, err
			}
			if p.Module == nil || p.Module.Main {
				continue
			}
			m := mods[p.Module.Path]
			if m == nil {
				files, err := rootFiles(p.Module.Dir)
				if err != nil {
					return nil, err
				}
				if len(files) == 0 {
					return nil, fmt.Errorf("%s %s has no license file", p.Module.Path, p.Module.Version)
				}
				m = &mod{c: Component{Name: p.Module.Path, Version: p.Module.Version, Files: files}, dir: p.Module.Dir, seen: map[string]bool{}}
				for _, f := range files {
					m.seen[f.Rel] = true
				}
				mods[p.Module.Path] = m
			}
			// Nested license files between the package and the module root.
			for d := p.Dir; len(d) > len(m.dir) && strings.HasPrefix(d, m.dir); d = filepath.Dir(d) {
				entries, err := os.ReadDir(d)
				if err != nil {
					return nil, err
				}
				for _, e := range entries {
					if e.IsDir() || !licenseFile.MatchString(e.Name()) {
						continue
					}
					rel, _ := filepath.Rel(m.dir, filepath.Join(d, e.Name()))
					rel = filepath.ToSlash(rel)
					if m.seen[rel] {
						continue
					}
					b, err := os.ReadFile(filepath.Join(d, e.Name()))
					if err != nil {
						return nil, err
					}
					m.seen[rel] = true
					m.c.Files = append(m.c.Files, File{Rel: rel, Text: string(b)})
				}
			}
		}
	}
	out := make([]Component, 0, len(mods))
	for _, m := range mods {
		sort.Slice(m.c.Files, func(i, j int) bool { return m.c.Files[i].Rel < m.c.Files[j].Rel })
		out = append(out, m.c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// rootFiles reads the license and notice files directly in dir.
func rootFiles(dir string) ([]File, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var files []File
	for _, e := range entries {
		if e.IsDir() || !licenseFile.MatchString(e.Name()) {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			return nil, err
		}
		files = append(files, File{Rel: e.Name(), Text: string(b)})
	}
	return files, nil
}

// npmPackages returns the frontend's production dependencies from its lock
// file, which are what Vite bundles, plus Vite's own module-preload helper.
func npmPackages(frontend string) ([]Component, error) {
	b, err := os.ReadFile(filepath.Join(frontend, "package-lock.json"))
	if err != nil {
		return nil, err
	}
	var lock struct {
		Packages map[string]struct {
			Version string `json:"version"`
			Dev     bool   `json:"dev"`
		} `json:"packages"`
	}
	if err := json.Unmarshal(b, &lock); err != nil {
		return nil, err
	}
	var out []Component
	for path, p := range lock.Packages {
		if path == "" || p.Dev || !strings.HasPrefix(path, "node_modules/") {
			continue
		}
		dir := filepath.Join(frontend, filepath.FromSlash(path))
		files, err := rootFiles(dir)
		if err != nil {
			return nil, fmt.Errorf("%s: %w (run npm install in frontend)", path, err)
		}
		if len(files) == 0 && path == "node_modules/@wailsio/runtime" {
			// The npm package ships without its license file (package.json says
			// MIT). It is built from the Wails repository, whose LICENSE is the
			// one in the Go module.
			files, err = wailsLicense()
			if err != nil {
				return nil, err
			}
		}
		if len(files) == 0 {
			return nil, fmt.Errorf("%s %s has no license file", path, p.Version)
		}
		out = append(out, Component{Name: "npm:" + strings.TrimPrefix(path, "node_modules/"), Version: p.Version, Files: files})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })

	vite, err := viteHelper(frontend, lock.Packages["node_modules/vite"].Version)
	if err != nil {
		return nil, err
	}
	return append(out, vite), nil
}

// wailsLicense reads the license at the root of the Wails v3 Go module.
func wailsLicense() ([]File, error) {
	dir, err := exec.Command("go", "list", "-m", "-f", "{{.Dir}}", "github.com/wailsapp/wails/v3").Output()
	if err != nil {
		return nil, fmt.Errorf("go list -m wails: %w", err)
	}
	return rootFiles(strings.TrimSpace(string(dir)))
}

// viteHelper is Vite's core license, which covers the module-preload polyfill
// Vite writes into the bundle. The rest of Vite's LICENSE.md lists Vite's own
// bundled dependencies, which do not reach the app.
func viteHelper(frontend, version string) (Component, error) {
	b, err := os.ReadFile(filepath.Join(frontend, "node_modules", "vite", "LICENSE.md"))
	if err != nil {
		return Component{}, fmt.Errorf("vite license: %w (run npm install in frontend)", err)
	}
	text := string(b)
	i := strings.Index(text, "# Licenses of bundled dependencies")
	if i < 0 {
		return Component{}, fmt.Errorf("vite LICENSE.md changed layout: find the core license by hand")
	}
	text = strings.TrimSpace(text[:i]) + "\n"
	return Component{Name: "npm:vite (module-preload helper only)", Version: version, Files: []File{{Rel: "LICENSE.md (core license)", Text: text}}}, nil
}

// spdx names the license of a text, for the summary table only.
func spdx(text string) string {
	t := strings.ToLower(text)
	switch {
	case strings.Contains(t, "unlicense") && strings.Contains(t, "mit"):
		return "Unlicense OR MIT"
	case strings.Contains(t, "apache license") && strings.Contains(t, "version 2.0"):
		return "Apache-2.0"
	case strings.Contains(t, "permission is hereby granted, free of charge"):
		return "MIT"
	case strings.Contains(t, "isc license") || strings.Contains(t, "permission to use, copy, modify, and/or distribute"):
		return "ISC"
	case strings.Contains(t, "neither the name") && strings.Contains(t, "redistribution and use"):
		return "BSD-3-Clause"
	case strings.Contains(t, "redistribution and use"):
		return "BSD-2-Clause"
	case strings.Contains(t, "additional ip rights grant"):
		return "patent grant"
	}
	return "notice"
}

// render writes the summary, then each distinct text once, naming who uses it.
func render(comps []Component) string {
	type entry struct {
		text  string
		users []string
	}
	var texts []*entry
	index := map[string]int{}
	refs := make([][]string, len(comps))
	for i, c := range comps {
		for _, f := range c.Files {
			norm := strings.TrimSpace(strings.ReplaceAll(f.Text, "\r\n", "\n"))
			n, ok := index[norm]
			if !ok {
				texts = append(texts, &entry{text: norm})
				n = len(texts)
				index[norm] = n
			}
			label := c.Name
			if f.Rel != "LICENSE" && f.Rel != "LICENSE.txt" && f.Rel != "LICENSE.md" {
				label += " (" + f.Rel + ")"
			}
			texts[n-1].users = append(texts[n-1].users, label)
			refs[i] = append(refs[i], fmt.Sprintf("[%d] %s", n, spdx(norm)))
		}
	}

	var b strings.Builder
	b.WriteString(`THIRD-PARTY SOFTWARE NOTICES AND LICENSES

s4browser is licensed under the MIT License (see LICENSE). It includes the
third-party software listed below, each under its own license. Their license
texts and notices follow the list. This file is generated by
"go run ./tools/notices"; do not edit it by hand.

`)
	b.WriteString("COMPONENTS\n\n")
	for i, c := range comps {
		fmt.Fprintf(&b, "%s %s\n    %s\n", c.Name, c.Version, strings.Join(refs[i], ", "))
	}
	b.WriteString("\n\nLICENSE TEXTS AND NOTICES\n")
	for i, e := range texts {
		fmt.Fprintf(&b, "\n%s\n[%d] Used by: %s\n%s\n\n%s\n", strings.Repeat("=", 78), i+1, strings.Join(e.users, ", "), strings.Repeat("-", 78), e.text)
	}
	return b.String()
}
