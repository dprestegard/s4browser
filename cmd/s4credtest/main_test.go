package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeAWS points the SDK at temporary AWS files holding fake session keys.
func fakeAWS(t *testing.T) string {
	dir := t.TempDir()
	creds := filepath.Join(dir, "credentials")
	cfg := filepath.Join(dir, "config")
	if err := os.WriteFile(creds, []byte("[src]\naws_access_key_id = AKIAFAKE\naws_secret_access_key = secretsecret\naws_session_token = tok\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfg, []byte("[profile src]\nregion = us-west-2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", creds)
	t.Setenv("AWS_CONFIG_FILE", cfg)
	return dir
}

func TestModes(t *testing.T) {
	dir := fakeAWS(t)
	control := filepath.Join(dir, "control")
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	get := func() (output, error) {
		var buf bytes.Buffer
		err := run(context.Background(), []string{"-source", "src", "-control", control, "-ttl", "2m"}, &buf, now)
		var out output
		if err == nil {
			err = json.Unmarshal(buf.Bytes(), &out)
		}
		return out, err
	}

	out, err := get() // No control file: valid.
	if err != nil || out.Version != 1 || out.AccessKeyID != "AKIAFAKE" || out.SecretAccessKey != "secretsecret" || out.SessionToken != "tok" || out.Expiration != "2026-11-20T12:02:00Z" {
		t.Fatalf("valid: %+v %v", out, err)
	}
	if err := os.WriteFile(control, []byte("broken\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err = get()
	if err != nil || out.AccessKeyID != "AKIAFAKE" || out.SecretAccessKey != "secretsecret" || out.SessionToken == "tok" || out.SessionToken == "" {
		t.Fatalf("broken: %+v %v", out, err)
	}
	if err := os.WriteFile(control, []byte("fail"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := get(); err == nil {
		t.Fatal("fail mode printed credentials")
	}
	if err := os.WriteFile(control, []byte("maybe"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := get(); err == nil {
		t.Fatal("an unknown mode was accepted")
	}

	log, err := os.ReadFile(control + ".log")
	if err != nil || strings.Count(string(log), "\n") != 3 || strings.Contains(string(log), "secret") || strings.Contains(string(log), "AKIA") {
		t.Fatalf("log %q %v", log, err)
	}
	if err := run(context.Background(), []string{"-source", "src", "-control", control, "-ttl", "30s"}, &bytes.Buffer{}, now); err == nil {
		t.Fatal("a TTL under a minute was accepted")
	}
}
