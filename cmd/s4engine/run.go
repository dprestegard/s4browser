package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"s4browser/internal/fakes3"
)

// Exit codes.
const (
	exitOK          = 0
	exitFailed      = 1
	exitUsage       = 2
	exitInterrupted = 130
)

// env holds what run needs from its surroundings, so tests can drive it.
type env struct {
	stdout, stderr io.Writer
	// fake, when set, is used by --fake instead of starting a new server. Tests
	// share one server across commands. The caller closes it.
	fake *fakes3.Server
}

func defaultEnv() env { return env{stdout: os.Stdout, stderr: os.Stderr} }

// usageError is reported with exit code 2.
type usageError struct{ msg string }

func (e *usageError) Error() string { return e.msg }

func usagef(format string, args ...any) error { return &usageError{fmt.Sprintf(format, args...)} }

const usageText = `s4engine: headless S3 transfer tool for s4browser

Usage: s4engine <command> [flags] [arguments]

Commands:
  upload    upload local files to s3://<bucket>/<root><run-id>/
  download  download objects under <root><run-id>/ to a directory
  verify    compare the SHA-256 of two local files
  generate  write a deterministic test file of a given size
  cleanup   abort multipart uploads and delete objects under the run prefix

Target (upload, download, cleanup): either --fake, or --profile P --region R --bucket B.
Every write, abort and delete is restricted to <root><run-id>/. The root is
s4browser-test/ unless --root names another prefix of at least one folder. Flags may appear before or after arguments.
Run "s4engine <command> -h" for the flags of a command.

Exit codes: 0 success, 1 transfer or verification failure, 2 usage error or
refused destination, 130 interrupted.
`

func run(ctx context.Context, args []string, e env) int {
	if len(args) == 0 {
		fmt.Fprint(e.stderr, usageText)
		return exitUsage
	}
	var err error
	code := exitOK
	switch args[0] {
	case "upload":
		code, err = cmdTransfer(ctx, e, args[1:], true)
	case "download":
		code, err = cmdTransfer(ctx, e, args[1:], false)
	case "verify":
		code, err = cmdVerify(ctx, e, args[1:])
	case "generate":
		code, err = cmdGenerate(ctx, e, args[1:])
	case "cleanup":
		code, err = cmdCleanup(ctx, e, args[1:])
	case "help", "-h", "--help":
		fmt.Fprint(e.stdout, usageText)
		return exitOK
	default:
		fmt.Fprintf(e.stderr, "s4engine: unknown command %q\n\n%s", args[0], usageText)
		return exitUsage
	}
	switch {
	case errors.Is(err, flag.ErrHelp):
		return exitOK
	case err != nil:
		var ue *usageError
		if errors.As(err, &ue) {
			fmt.Fprintf(e.stderr, "s4engine %s: %v\n", args[0], ue)
			return exitUsage
		}
		fmt.Fprintf(e.stderr, "s4engine %s: %v\n", args[0], err)
		return exitFailed
	}
	return code
}

// newFlagSet returns a flag set that reports errors to stderr and never exits.
func newFlagSet(name string, e env) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(e.stderr)
	return fs
}

// parseFlags parses flags that may be mixed with positional arguments.
func parseFlags(fs *flag.FlagSet, args []string) ([]string, error) {
	var positional []string
	for {
		if err := fs.Parse(args); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return nil, err
			}
			return nil, &usageError{err.Error()}
		}
		args = fs.Args()
		if len(args) == 0 {
			return positional, nil
		}
		positional = append(positional, args[0])
		args = args[1:]
	}
}
