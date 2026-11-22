package main

import (
	"strings"
	"testing"

	"s4browser/internal/app"
)

func TestLicensesAreEmbedded(t *testing.T) {
	text := app.New(licenseText, noticesText).Licenses()
	for _, want := range []string{"MIT License", "Copyright (c) 2026 Derek Prestegard", "THIRD-PARTY SOFTWARE NOTICES", "github.com/wailsapp/wails/v2", "npm:react "} {
		if !strings.Contains(text, want) {
			t.Errorf("Licenses() lacks %q", want)
		}
	}
}
