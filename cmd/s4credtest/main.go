// Command s4credtest is a credential_process helper for the stage 4.3
// credential expiry check. It is test tooling, not part of the app.
//
// It loads the credentials of a source profile with the AWS SDK and prints them
// in the credential_process format with a short Expiration, so the app has to
// run it again every few minutes. A control file switches what it hands out:
//
//	valid   the source profile's credentials (also when the file is missing)
//	broken  the same keys with a damaged session token, so S3 refuses every
//	        request with InvalidToken, as it refuses a session that has ended.
//	        (Changing only the secret does not work: the SDK reuses its derived
//	        signing key while the access key ID and the day are unchanged.)
//	fail    no credentials: the helper exits 1, as a helper whose sign-in lapsed
//
// Every run appends one line to <control>.log: the time, the mode and the
// expiry. No key material is written anywhere but stdout.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/config"
)

type output struct {
	Version         int    `json:"Version"`
	AccessKeyID     string `json:"AccessKeyId"`
	SecretAccessKey string `json:"SecretAccessKey"`
	SessionToken    string `json:"SessionToken,omitempty"`
	Expiration      string `json:"Expiration"`
}

func main() {
	if err := run(context.Background(), os.Args[1:], os.Stdout, time.Now()); err != nil {
		fmt.Fprintln(os.Stderr, "s4credtest:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, stdout io.Writer, now time.Time) error {
	fs := flag.NewFlagSet("s4credtest", flag.ContinueOnError)
	source := fs.String("source", "", "profile whose credentials are handed out")
	ttl := fs.Duration("ttl", 3*time.Minute, "lifetime reported in Expiration, at least 1m")
	control := fs.String("control", "", "control file holding valid, broken or fail")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *source == "" || *control == "" {
		return errors.New("-source and -control are required")
	}
	if *ttl < time.Minute {
		return errors.New("-ttl must be at least 1m, because the app refreshes one minute ahead")
	}
	mode, err := readMode(*control)
	if err != nil {
		return err
	}
	expires := now.Add(*ttl).UTC()
	logRun(*control, now, mode, expires)
	if mode == "fail" {
		return errors.New("the control file says fail")
	}

	cfg, err := config.LoadDefaultConfig(ctx, config.WithSharedConfigProfile(*source))
	if err != nil {
		return fmt.Errorf("load profile %s: %w", *source, err)
	}
	creds, err := cfg.Credentials.Retrieve(ctx)
	if err != nil {
		return fmt.Errorf("credentials of profile %s: %w", *source, err)
	}
	if creds.CanExpire && creds.Expires.Before(expires) {
		expires = creds.Expires.UTC()
	}
	out := output{Version: 1, AccessKeyID: creds.AccessKeyID, SecretAccessKey: creds.SecretAccessKey,
		SessionToken: creds.SessionToken, Expiration: expires.Format(time.RFC3339)}
	if mode == "broken" {
		if creds.SessionToken == "" {
			return errors.New("broken mode needs a source profile with a session token")
		}
		out.SessionToken = "broken" + creds.SessionToken[min(6, len(creds.SessionToken)):]
	}
	return json.NewEncoder(stdout).Encode(out)
}

func readMode(path string) (string, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return "valid", nil
	}
	if err != nil {
		return "", err
	}
	switch mode := strings.TrimSpace(string(data)); mode {
	case "", "valid":
		return "valid", nil
	case "broken", "fail":
		return mode, nil
	default:
		return "", fmt.Errorf("control file %s holds %q, want valid, broken or fail", path, mode)
	}
}

// logRun records the run without any key material. A failure to log is ignored.
func logRun(control string, now time.Time, mode string, expires time.Time) {
	f, err := os.OpenFile(control+".log", os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintf(f, "%s mode=%s expires=%s\n", now.UTC().Format(time.RFC3339), mode, expires.Format(time.RFC3339))
}
