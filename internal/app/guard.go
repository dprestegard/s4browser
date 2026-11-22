package app

import (
	"fmt"
	"log"
	"os"

	"s4browser/internal/engine"
)

// writePrefixEnv names the environment variable that confines every write the
// app makes to one key prefix, for verification runs against a real bucket.
// Uploads, deletes, new folders and tag changes outside it are refused before
// any request is sent. Reads are not limited. Normal use leaves it unset.
const writePrefixEnv = "S4_WRITE_PREFIX"

// refuseAll is the guard used when S4_WRITE_PREFIX is set but invalid: a
// mistyped prefix must not turn the guard off.
type refuseAll struct{ err error }

func (r refuseAll) CheckWrite(string) error { return r.err }

// writeGuardFromEnv returns the guard S4_WRITE_PREFIX asks for, or nil when it
// is unset.
func writeGuardFromEnv() engine.WriteGuard {
	prefix, set := os.LookupEnv(writePrefixEnv)
	if !set {
		return nil
	}
	g, err := engine.NewPrefixGuard(prefix)
	if err != nil {
		log.Printf("%s: %v; every write is refused", writePrefixEnv, err)
		return refuseAll{fmt.Errorf("%w (%s is set but invalid)", err, writePrefixEnv)}
	}
	log.Printf("%s: writes are limited to %q", writePrefixEnv, g.Prefix())
	return g
}

// checkWrite applies the write guard, if any, to one key.
func (a *App) checkWrite(key string) error {
	if a.writeGuard == nil {
		return nil
	}
	return a.writeGuard.CheckWrite(key)
}
