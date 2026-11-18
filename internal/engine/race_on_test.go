//go:build race

package engine_test

import (
	"testing"

	"s4browser/internal/engine"
)

const raceEnabled = true

// startMemoryCheck does nothing here: the race detector multiplies memory use, so
// memory assertions exist only in the !race build (docs/engine-design.md section 4).
func startMemoryCheck(*testing.T, engine.Settings, int) func() { return func() {} }
