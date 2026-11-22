package main

import (
	"embed"

	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
	"github.com/wailsapp/wails/v2/pkg/options/mac"
	"github.com/wailsapp/wails/v2/pkg/options/windows"
)

//go:embed all:frontend/dist
var assets embed.FS

//go:embed build/appicon.png
var appIcon []byte

func main() {
	// Create an instance of the app structure
	app := NewApp()

	// Create application with options
	err := wails.Run(&options.App{
		Title:     "s4browser",
		Width:     1280,
		Height:    800,
		MinWidth:  1024,
		MinHeight: 640,
		AssetServer: &assetserver.Options{
			Assets: assets,
		},
		// --bg0 of the dark theme, so the window does not flash before the
		// page paints. The page switches the title bar when the theme changes.
		BackgroundColour: &options.RGBA{R: 0x0F, G: 0x10, B: 0x12, A: 255},
		Windows:          &windows.Options{Theme: windows.Dark},
		// On macOS the page's top bar is the title bar: Wails cannot switch a
		// Mac title bar's theme from the page, so a visible one would follow
		// the system appearance instead of the app's theme. The traffic lights
		// sit inset over the top bar's left end (see html[data-platform=mac]).
		Mac: &mac.Options{
			TitleBar: mac.TitleBarHiddenInset(),
			About: &mac.AboutInfo{
				Title:   "s4browser",
				Message: "Desktop S3 client for large media transfers.\nMIT License. Third-party licenses are under View settings ▸ Licenses.",
				Icon:    appIcon,
			},
		},
		OnStartup:        app.startup,
		OnShutdown:       app.shutdown,
		// Quitting cancels unfinished transfers, so it asks first.
		OnBeforeClose: app.beforeClose,
		// Native file drop: the window resolves dropped files and folders to
		// absolute paths in Go. File data never crosses the UI bridge.
		DragAndDrop: &options.DragAndDrop{EnableFileDrop: true},
		Bind: []interface{}{
			app,
		},
	})

	if err != nil {
		println("Error:", err.Error())
	}
}
