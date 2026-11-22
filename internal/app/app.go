package app

import (
	"context"
	"os"
	"path/filepath"
	"time"

	"github.com/wailsapp/wails/v2/pkg/runtime"

	"s4browser/internal/browse"
	"s4browser/internal/creds"
	"s4browser/internal/engine"
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
	// objects holds counted deletes and the cancel of the one running.
	objects objectWork
	// maxDelete caps one delete (objects.DefaultMaxDelete when 0); tests lower it.
	maxDelete int
	// emitHook, when set, receives events instead of the window. Tests set it.
	emitHook func(event string, payload any)
	// writeGuard, when set, refuses writes outside one key prefix. It comes from
	// S4_WRITE_PREFIX (see guard.go). Nil allows all writes.
	writeGuard engine.WriteGuard
	// licenseText and noticesText are shown by the Licenses dialog.
	licenseText, noticesText string
}

// New creates the App. licenseText and noticesText are the texts the Licenses
// dialog shows.
func New(licenseText, noticesText string) *App {
	return &App{licenseText: licenseText, noticesText: noticesText}
}

// logPath is where the spike logs drops: %AppData%\s4browser\filedrop.log on
// Windows.
func logPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "s4browser", "filedrop.log"), nil
}

// emit sends an event to the window, or to emitHook in tests.
func (a *App) emit(event string, payload any) {
	if a.emitHook != nil {
		a.emitHook(event, payload)
		return
	}
	runtime.EventsEmit(a.ctx, event, payload)
}

// startup opens the log and registers the native file-drop callback.
func (a *App) startup(ctx context.Context) {
	a.ctx = ctx
	a.writeGuard = writeGuardFromEnv()
	path, err := logPath()
	if err == nil {
		a.log, err = openDropLog(path)
	}
	if err != nil {
		a.logErr = err.Error()
	}
	if a.profiles, err = openProfiles(); err != nil {
		a.profilesErr = err.Error()
	} else {
		// The window reloads what failed for want of credentials, such as the
		// bucket list, when a profile's credentials change.
		a.profiles.OnCredentialsChanged(func(id string) { a.emit("s4:credentials", id) })
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

// Hooks are the window lifecycle callbacks wails.Run takes. They are a package
// function rather than App methods because Wails binds every exported method of
// App to the frontend, and the page must not be able to call these.
type Hooks struct {
	OnStartup     func(context.Context)
	OnShutdown    func(context.Context)
	OnBeforeClose func(context.Context) bool
}

// LifecycleHooks returns a's lifecycle callbacks.
func LifecycleHooks(a *App) Hooks {
	return Hooks{OnStartup: a.startup, OnShutdown: a.shutdown, OnBeforeClose: a.beforeClose}
}
