//go:build !windows

package creds

// reportWindow has nothing to report: other systems open no console window.
func reportWindow(string) {}
