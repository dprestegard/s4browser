package main

import _ "embed"

// The app ships as a single file on Windows, so its license and the notices of
// the third-party software inside it travel in the binary and are shown by the
// Licenses dialog. Regenerate the notices with go run ./tools/notices.

//go:embed LICENSE
var licenseText string

//go:embed THIRD_PARTY_NOTICES.txt
var noticesText string

// Licenses returns s4browser's license followed by the third-party notices.
func (a *App) Licenses() string {
	return "s4browser\n\n" + licenseText + "\n\n" + noticesText
}
