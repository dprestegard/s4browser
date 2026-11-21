// Command s4engine is the headless transfer tool for the s4browser engine. It
// drives the real engine packages against the local fake S3 (--fake) or against
// real S3 (--profile, --region, --bucket), and it is the tool the stage 0.7 gate
// uses. Every remote write, abort and delete goes through the test-prefix guard
// for <root><run-id>/ (root s4browser-test/ unless --root gives another), and no
// flag disables it.
//
// Exit codes: 0 success, 1 transfer or verification failure, 2 usage error or a
// refused destination, 130 interrupted.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	go func() {
		// After the first signal restore the default behaviour, so a second
		// Ctrl+C ends the process at once if cleanup hangs.
		<-ctx.Done()
		stop()
	}()
	os.Exit(run(ctx, os.Args[1:], defaultEnv()))
}
