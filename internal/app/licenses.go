package app

// Licenses returns s4browser's license followed by the third-party notices. The
// app ships as a single file on Windows, so both texts travel in the binary: the
// root package embeds them and hands them to New. Regenerate the notices with
// go run ./tools/notices.
func (a *App) Licenses() string {
	return "s4browser\n\n" + a.licenseText + "\n\n" + a.noticesText
}
