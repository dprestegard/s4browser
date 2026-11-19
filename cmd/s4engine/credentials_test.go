package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"s4browser/internal/engine"
)

const (
	staticKey    = "AKIATESTSTATICKEY000"
	staticSecret = "static-secret-value-not-real"
	procKey      = "ASIATESTPROCESSKEY00"
	procSecret   = "process-secret-value-not-real"
	procToken    = "process-session-token-not-real"
)

// TestCredentialHelper is not a test: the credential_process of the config
// below runs this binary and it prints credentials as JSON.
func TestCredentialHelper(t *testing.T) {
	switch os.Getenv("S4_CRED_HELPER") {
	case "":
		return
	case "fail":
		fmt.Fprintln(os.Stderr, "helper: no credentials today")
		os.Exit(3)
	}
	fmt.Printf(`{"Version":1,"AccessKeyId":%q,"SecretAccessKey":%q,"SessionToken":%q,"Expiration":"2099-01-01T00:00:00Z"}`, procKey, procSecret, procToken)
	os.Exit(0)
}

// useTempAWSFiles points the SDK at temporary files with fake values, so the
// real ~/.aws is never read.
func useTempAWSFiles(t *testing.T, helper string) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if strings.ContainsAny(exe, " \"") {
		t.Skipf("test binary path %q needs shell quoting", exe)
	}
	dir := t.TempDir()
	creds := filepath.Join(dir, "credentials")
	cfg := filepath.Join(dir, "config")
	must(t, os.WriteFile(creds, []byte(fmt.Sprintf("[static]\naws_access_key_id = %s\naws_secret_access_key = %s\n", staticKey, staticSecret)), 0o600))
	must(t, os.WriteFile(cfg, []byte(fmt.Sprintf("[profile proc]\ncredential_process = %s -test.run=^TestCredentialHelper$\n", exe)), 0o600))
	t.Setenv("AWS_CONFIG_FILE", cfg)
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", creds)
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")
	t.Setenv("S4_CRED_HELPER", helper)
	for _, name := range []string{"AWS_PROFILE", "AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "AWS_SESSION_TOKEN", "AWS_WEB_IDENTITY_TOKEN_FILE", "AWS_ROLE_ARN"} {
		t.Setenv(name, "")
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func TestLoadStaticKeys(t *testing.T) {
	useTempAWSFiles(t, "")
	c, err := loadCredentials(context.Background(), "static", "eu-west-1")
	must(t, err)
	got, err := c.provider.Retrieve(context.Background())
	must(t, err)
	if got.AccessKeyID != staticKey || got.SecretAccessKey != staticSecret {
		t.Fatalf("credentials: %q", got.AccessKeyID)
	}
	if c.cfg.Region != "eu-west-1" {
		t.Fatalf("region %q", c.cfg.Region)
	}
	must(t, c.Reload(context.Background(), "static"))
}

func TestLoadCredentialProcess(t *testing.T) {
	useTempAWSFiles(t, "ok")
	c, err := loadCredentials(context.Background(), "proc", "us-east-1")
	must(t, err)
	got, err := c.provider.Retrieve(context.Background())
	must(t, err)
	if got.AccessKeyID != procKey || got.SecretAccessKey != procSecret || got.SessionToken != procToken {
		t.Fatalf("credentials: %q", got.AccessKeyID)
	}
	if !got.CanExpire {
		t.Fatal("an expiring credential_process result must be refreshable")
	}
	// Reload empties both caches and runs the process again.
	must(t, c.Reload(context.Background(), "proc"))
}

func TestCredentialFailuresAreMarked(t *testing.T) {
	useTempAWSFiles(t, "fail")
	c, err := loadCredentials(context.Background(), "proc", "us-east-1")
	must(t, err) // loading is lazy: the process runs on the first request
	_, err = c.provider.Retrieve(context.Background())
	if !errors.Is(err, engine.ErrCredentialsUnavailable) {
		t.Fatalf("a failing credential_process must match ErrCredentialsUnavailable: %v", err)
	}
	if err := c.Reload(context.Background(), "proc"); err == nil {
		t.Fatal("the probe must fail while the process fails")
	}
	if cls := engine.Classify(err); cls != engine.ClassCredential {
		t.Fatalf("class %v, want credential", cls)
	}
	if strings.Contains(err.Error(), procSecret) || strings.Contains(err.Error(), staticSecret) {
		t.Fatalf("error leaks a secret: %v", err)
	}
}

func TestUnknownProfileFails(t *testing.T) {
	useTempAWSFiles(t, "")
	if _, err := loadCredentials(context.Background(), "nosuch", "us-east-1"); err == nil {
		t.Fatal("an unknown profile must fail")
	}
}
