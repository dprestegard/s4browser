package app

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Kinds reported for a dropped path.
const (
	KindFile    = "file"
	KindFolder  = "folder"
	KindOther   = "other"   // a device, pipe or similar
	KindMissing = "missing" // the path no longer exists
	KindError   = "error"   // it exists but cannot be examined
)

// DroppedPath is one entry of a drop. It holds the path string and its kind,
// never file data.
type DroppedPath struct {
	Path string `json:"path"`
	Kind string `json:"kind"`
	// Absolute is false if the path the window reported was not absolute, which
	// would be a bug in the drop path resolution.
	Absolute bool   `json:"absolute"`
	Error    string `json:"error,omitempty"`
}

// Drop is one drop event: a file, a folder or a mixed selection.
type Drop struct {
	At       time.Time     `json:"at"`
	X        int           `json:"x"`
	Y        int           `json:"y"`
	Items    []DroppedPath `json:"items"`
	LogError string        `json:"logError,omitempty"`
}

// describePaths classifies each path with one os.Stat. It follows links, so a
// symlink to a folder reports "folder", which is how the later upload stages
// will treat it.
func describePaths(paths []string) []DroppedPath {
	items := make([]DroppedPath, 0, len(paths))
	for _, p := range paths {
		item := DroppedPath{Path: p, Absolute: filepath.IsAbs(p)}
		info, err := os.Stat(p)
		switch {
		case err == nil && info.IsDir():
			item.Kind = KindFolder
		case err == nil && info.Mode().IsRegular():
			item.Kind = KindFile
		case err == nil:
			item.Kind = KindOther
		case os.IsNotExist(err):
			item.Kind, item.Error = KindMissing, err.Error()
		default:
			item.Kind, item.Error = KindError, err.Error()
		}
		items = append(items, item)
	}
	return items
}

// dropLog appends one line per dropped path to a file.
type dropLog struct {
	mu   sync.Mutex
	f    *os.File
	path string
}

func openDropLog(path string) (*dropLog, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, err
	}
	return &dropLog{f: f, path: path}, nil
}

// Path returns the log file's path.
func (l *dropLog) Path() string { return l.path }

// Write logs one drop: a header line, then one line per path.
func (l *dropLog) Write(d Drop) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	stamp := d.At.UTC().Format(time.RFC3339Nano)
	if _, err := fmt.Fprintf(l.f, "%s drop at (%d,%d), %d item(s)\n", stamp, d.X, d.Y, len(d.Items)); err != nil {
		return err
	}
	for _, it := range d.Items {
		line := fmt.Sprintf("%s   %-7s absolute=%t %s", stamp, it.Kind, it.Absolute, it.Path)
		if it.Error != "" {
			line += "  (" + it.Error + ")"
		}
		if _, err := fmt.Fprintln(l.f, line); err != nil {
			return err
		}
	}
	return l.f.Sync()
}

// Close closes the file.
func (l *dropLog) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.f.Close()
}
