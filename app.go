package main

import (
	"context"
	"os"
	"path/filepath"
	"time"

	"github.com/wailsapp/wails/v2/pkg/runtime"

	"s4browser/internal/browse"
	"s4browser/internal/creds"
	"s4browser/internal/queue"
)

// App is the stage 0.6 file-drop spike. It has no engine and no S3 access: it
// only receives dropped paths from the native window, classifies them in Go and
// shows and logs the result. File contents never reach the frontend, and no
// path leaves this process.
type App struct {
	ctx context.Context
	log *dropLog
	// logErr is the reason the log file could not be opened, shown on screen.
	logErr string
	// profiles is nil if the profile store could not be opened, and profilesErr
	// then says why.
	profiles    *creds.Manager
	profilesErr string
	// browse is nil if browsing could not start, and browseErr then says why.
	browse    *browse.Service
	browseErr string
	// queue is nil if transfers could not start, and queueErr then says why.
	queue    *queue.Service
	queueErr string
}

// NewApp creates the App.
func NewApp() *App { return &App{} }

// logPath is where the spike logs drops: %AppData%\s4browser\filedrop.log on
// Windows.
func logPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "s4browser", "filedrop.log"), nil
}

// startup opens the log and registers the native file-drop callback.
func (a *App) startup(ctx context.Context) {
	a.ctx = ctx
	path, err := logPath()
	if err == nil {
		a.log, err = openDropLog(path)
	}
	if err != nil {
		a.logErr = err.Error()
	}
	if a.profiles, err = openProfiles(); err != nil {
		a.profilesErr = err.Error()
	}
	if a.browse, err = a.openBrowse(); err != nil {
		a.browseErr = err.Error()
	}
	if a.queue, err = a.openQueue(); err != nil {
		a.queueErr = err.Error()
	}
	runtime.OnFileDrop(ctx, a.onDrop)
}

func (a *App) shutdown(context.Context) {
	a.closeQueue()
	if a.browse != nil {
		a.browse.Close()
	}
	if a.log != nil {
		_ = a.log.Close()
	}
}

// onDrop runs on the Go side of the bridge with the absolute paths Windows
// resolved for the drop. It classifies and logs them, then sends the frontend
// only the path strings and their kinds.
func (a *App) onDrop(x, y int, paths []string) {
	drop := Drop{At: time.Now(), X: x, Y: y, Items: describePaths(paths)}
	if a.log != nil {
		if err := a.log.Write(drop); err != nil {
			drop.LogError = err.Error()
		}
	}
	runtime.EventsEmit(a.ctx, "s4:drop", drop)
}

// StartInfo describes the spike's log for the screen.
type StartInfo struct {
	LogPath  string `json:"logPath"`
	LogError string `json:"logError,omitempty"`
}

// Info returns where drops are logged.
func (a *App) Info() StartInfo {
	if a.log != nil {
		return StartInfo{LogPath: a.log.Path()}
	}
	return StartInfo{LogError: a.logErr}
}
