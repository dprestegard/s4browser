package app

import (
	"context"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"

	"github.com/wailsapp/wails/v3/pkg/application"
	"github.com/wailsapp/wails/v3/pkg/events"

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
	// wapp and window are set by Attach before the app runs.
	wapp   *application.App
	window *application.WebviewWindow
	// quitConfirmed lets the close that follows an answered "Quit anyway?" through.
	quitConfirmed atomic.Bool
	log           *dropLog
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
	if a.wapp != nil {
		a.wapp.Event.Emit(event, payload)
	}
}

// ServiceStartup opens the log, the profile store, browsing and the queue. Wails
// calls it when the application starts; it is not bound to the frontend.
func (a *App) ServiceStartup(ctx context.Context, _ application.ServiceOptions) error {
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
	return nil
}

// ServiceShutdown stops the queue and closes the stores. Wails calls it on quit.
func (a *App) ServiceShutdown() error {
	a.closeQueue()
	if a.browse != nil {
		a.browse.Close()
	}
	if a.log != nil {
		_ = a.log.Close()
	}
	return nil
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
	a.emit("s4:drop", drop)
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

// Attach wires the window into a. It is a package function rather than an App
// method because Wails binds every exported method of App to the frontend, and
// the page must not be able to call it. Native file drop and the quit
// confirmation are registered here.
func Attach(a *App, wapp *application.App, window *application.WebviewWindow) {
	a.wapp, a.window = wapp, window
	// Native file drop: the window resolves dropped files and folders to
	// absolute paths in Go. File data never crosses the UI bridge.
	window.OnWindowEvent(events.Common.WindowFilesDropped, func(e *application.WindowEvent) {
		var x, y int
		if d := e.Context().DropTargetDetails(); d != nil {
			x, y = d.X, d.Y
		}
		a.onDrop(x, y, e.Context().DroppedFiles())
	})
	// Quitting cancels unfinished transfers, so it asks first. The question is
	// asked off the event hook so the hook can return and keep the window open.
	window.RegisterHook(events.Common.WindowClosing, func(e *application.WindowEvent) {
		if a.quitConfirmed.Load() {
			return
		}
		e.Cancel()
		go func() {
			if !a.beforeClose(a.bindingContext()) {
				a.quitConfirmed.Store(true)
				wapp.Quit()
			}
		}()
	})
}
