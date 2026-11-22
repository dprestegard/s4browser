package main

import (
	"strings"
	"testing"
)

func TestLicensesAreEmbedded(t *testing.T) {
	text := (&App{}).Licenses()
	for _, want := range []string{"MIT License", "Copyright (c) 2026 Derek Prestegard", "THIRD-PARTY SOFTWARE NOTICES", "github.com/wailsapp/wails/v2", "npm:react "} {
		if !strings.Contains(text, want) {
			t.Errorf("Licenses() lacks %q", want)
		}
	}
}
