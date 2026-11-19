//go:build !devfake

package main

import "s4browser/internal/browse"

func devProfile(id string) string { return id }

func devBrowseOptions(*browse.Options) error { return nil }
