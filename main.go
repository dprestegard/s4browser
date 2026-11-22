package main

import (
	"embed"

	"github.com/wailsapp/wails/v3/pkg/application"

	"s4browser/internal/app"
)

//go:embed all:frontend/dist
var assets embed.FS

//go:embed build/appicon.png
var appIcon []byte

// The app ships as a single file on Windows, so its license and the notices of
// the third-party software inside it travel in the binary and are shown by the
// Licenses dialog. Regenerate the notices with go run ./tools/notices.

//go:embed LICENSE
var licenseText string

//go:embed THIRD_PARTY_NOTICES.txt
var noticesText string

func main() {
	a := app.New(licenseText, noticesText)

	wapp := application.New(application.Options{
		Name:        "s4browser",
		Description: "Desktop S3 client for large media transfers",
		Icon:        appIcon,
		Services:    []application.Service{application.NewService(a)},
		Assets: application.AssetOptions{
			Handler: application.BundledAssetFileServer(assets),
		},
		Mac: application.MacOptions{
			ApplicationShouldTerminateAfterLastWindowClosed: true,
		},
	})

	window := wapp.Window.NewWithOptions(application.WebviewWindowOptions{
		Title:     "s4browser",
		Width:     1280,
		Height:    800,
		MinWidth:  1024,
		MinHeight: 640,
		URL:       "/",
		// --bg0 of the dark theme, so the window does not flash before the
		// page paints. The page switches the title bar when the theme changes.
		BackgroundColour: application.NewRGB(0x0F, 0x10, 0x12),
		Windows:          application.WindowsWindow{Theme: application.SystemDefault},
		// On macOS the page's top bar is the title bar: a visible one would
		// follow the system appearance instead of the app's theme. The traffic
		// lights sit inset over the top bar's left end (see
		// html[data-platform=mac]).
		Mac: application.MacWindow{TitleBar: application.MacTitleBarHiddenInset},
		// Native file drop: see app.Attach.
		EnableFileDrop: true,
	})
	app.Attach(a, wapp, window)

	if err := wapp.Run(); err != nil {
		println("Error:", err.Error())
	}
}
