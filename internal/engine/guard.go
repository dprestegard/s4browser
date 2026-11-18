package engine

import (
	"fmt"
	"regexp"
	"strings"
)

// TestKeyRoot is the only key prefix test tooling may write under.
const TestKeyRoot = "s4browser-test/"

// WriteGuard approves every remote mutation: create, part upload, complete,
// abort and put. Internal/fakes3 and the s4engine CLI wrap their ObjectStore so
// that a refused key never reaches the network.
type WriteGuard interface {
	// CheckWrite returns an error wrapping ErrWriteRefused for a forbidden key.
	CheckWrite(key string) error
}

var runIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`)

// TestPrefixGuard refuses writes outside s4browser-test/<run-id>/.
type TestPrefixGuard struct {
	prefix string
}

// NewTestPrefixGuard returns a guard for runID, which must be 1 to 64 letters,
// digits, underscores or hyphens and start with a letter or digit.
func NewTestPrefixGuard(runID string) (*TestPrefixGuard, error) {
	if !runIDPattern.MatchString(runID) {
		return nil, fmt.Errorf("%w: invalid run ID %q", ErrWriteRefused, runID)
	}
	return &TestPrefixGuard{prefix: TestKeyRoot + runID + "/"}, nil
}

// Prefix returns the permitted key prefix.
func (g *TestPrefixGuard) Prefix() string { return g.prefix }

// CheckWrite allows only keys strictly below the run prefix whose path segments
// are all non-empty and contain no dot-segments or backslashes.
func (g *TestPrefixGuard) CheckWrite(key string) error {
	rest, ok := strings.CutPrefix(key, g.prefix)
	if !ok || rest == "" {
		return fmt.Errorf("%w: key %q is outside %q", ErrWriteRefused, key, g.prefix)
	}
	if strings.ContainsAny(key, "\\\x00") {
		return fmt.Errorf("%w: key %q has a forbidden character", ErrWriteRefused, key)
	}
	for segment := range strings.SplitSeq(rest, "/") {
		if segment == "" || segment == "." || segment == ".." {
			return fmt.Errorf("%w: key %q has an unsafe path segment", ErrWriteRefused, key)
		}
	}
	return nil
}
