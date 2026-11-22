//go:build !devfake

package app

import (
	"s4browser/internal/browse"
	"s4browser/internal/queue"
)

func devProfile(id string) string { return id }

func devBrowseOptions(*browse.Options) error { return nil }

func devQueueOptions(*queue.Options) error { return nil }

func devPick(pickKind) ([]string, bool) { return nil, false }
