package creds

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials/processcreds"
)

const (
	// processTimeout limits one run of a credential_process helper.
	processTimeout = time.Minute
	// processWaitDelay limits the wait for the helper's output pipes after it
	// exits. A background process that keeps a pipe open must not hang the app.
	processWaitDelay = 2 * time.Second
	// maxHelperOutput limits the stdout kept from a helper.
	maxHelperOutput = 1 << 20
	// maxHelperMessage limits the helper's stderr text kept for an error.
	maxHelperMessage = 200
)

// NewProcessProvider returns a provider that runs a credential_process command
// with no visible window. The SDK's own provider cannot be used from a GUI app:
// on Windows it opens a console window for each run, passes quoted commands to
// cmd.exe incorrectly, leaves the helper running when cancelled, and quotes the
// helper's output in parse errors, which can put secrets in a log.
//
// The helper has no stdin, so a helper that prompts on the console fails.
func NewProcessProvider(command string) aws.CredentialsProvider {
	return processProvider{command}
}

type processProvider struct{ command string }

// Retrieve runs the helper once.
func (p processProvider) Retrieve(ctx context.Context) (aws.Credentials, error) {
	runCtx, cancel := context.WithTimeout(ctx, processTimeout)
	defer cancel()

	stdout := &limitedBuffer{limit: maxHelperOutput}
	stderr := &limitedBuffer{limit: 4 * maxHelperMessage}
	cmd := hiddenCommand(runCtx, p.command)
	cmd.Stdout, cmd.Stderr = stdout, stderr
	cmd.WaitDelay = processWaitDelay

	release, err := startContained(cmd)
	if err != nil {
		return aws.Credentials{}, helperError(ctx, "the helper could not be started", "")
	}
	err = cmd.Wait()
	release()
	// ErrWaitDelay means the helper exited but something it started still holds
	// the pipe. Its output is complete.
	if err != nil && !errors.Is(err, exec.ErrWaitDelay) {
		var exitErr *exec.ExitError
		switch {
		case ctx.Err() == nil && runCtx.Err() != nil:
			return aws.Credentials{}, helperError(ctx, fmt.Sprintf("the helper did not finish within %s", processTimeout), "")
		case errors.As(err, &exitErr):
			return aws.Credentials{}, helperError(ctx, fmt.Sprintf("the helper exited with code %d", exitErr.ExitCode()), stderr.firstLine())
		}
		return aws.Credentials{}, helperError(ctx, "the helper could not be run", stderr.firstLine())
	}
	return parseHelperOutput(ctx, stdout.bytes())
}

// parseHelperOutput decodes the helper's JSON. Decode errors are dropped because
// they can quote the output, which holds the credentials.
func parseHelperOutput(ctx context.Context, out []byte) (aws.Credentials, error) {
	if runtime.GOOS == "windows" {
		out = bytes.ReplaceAll(out, []byte(`\"`), []byte(`"`)) // as the SDK does
	}
	var resp processcreds.CredentialProcessResponse
	if json.Unmarshal(out, &resp) != nil {
		return aws.Credentials{}, helperError(ctx, "the helper's output is not valid credential JSON", "")
	}
	if resp.Version != 1 || resp.AccessKeyID == "" || resp.SecretAccessKey == "" {
		return aws.Credentials{}, helperError(ctx, "the helper's output is missing required fields", "")
	}
	creds := aws.Credentials{
		Source:          processcreds.ProviderName,
		AccessKeyID:     resp.AccessKeyID,
		SecretAccessKey: resp.SecretAccessKey,
		SessionToken:    resp.SessionToken,
		AccountID:       resp.AccountID,
	}
	if resp.Expiration != nil {
		creds.CanExpire = true
		creds.Expires = *resp.Expiration
	}
	return creds, nil
}

// helperError reports a failed run. A cancelled caller gets its context error.
func helperError(ctx context.Context, reason, helperSaid string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if helperSaid != "" {
		return fmt.Errorf("credential_process failed: %s: %s", reason, helperSaid)
	}
	return fmt.Errorf("credential_process failed: %s", reason)
}

// limitedBuffer keeps the first bytes written to it and discards the rest.
type limitedBuffer struct {
	mu    sync.Mutex
	buf   bytes.Buffer
	limit int
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if room := b.limit - b.buf.Len(); room > 0 {
		b.buf.Write(p[:min(room, len(p))])
	}
	return len(p), nil
}

func (b *limitedBuffer) bytes() []byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	return bytes.Clone(b.buf.Bytes())
}

// firstLine returns the first non-empty line, shortened for an error message.
func (b *limitedBuffer) firstLine() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	for line := range strings.SplitSeq(b.buf.String(), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			if r := []rune(line); len(r) > maxHelperMessage {
				return string(r[:maxHelperMessage]) + "..."
			}
			return line
		}
	}
	return ""
}
