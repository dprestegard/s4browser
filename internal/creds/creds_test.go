package creds

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"s4browser/internal/engine"
)

const (
	fakeAccess  = "AKIATESTFAKEKEY00001"
	fakeSecret  = "fake-secret-value-not-real"
	fakeAccess2 = "AKIATESTFAKEKEY00002"
	fakeSecret2 = "fake-secret-value-two-not-real"
	procAccess  = "ASIATESTPROCESSKEY01"
	procSecret  = "process-secret-not-real"
	procToken   = "process-token-not-real"
)

// TestMain doubles as the credential_process helper: the tests run this binary
// again with S4_CRED_HELPER set.
func TestMain(m *testing.M) {
	if mode := os.Getenv("S4_CRED_HELPER"); mode != "" {
		runHelper(mode)
		return
	}
	os.Exit(m.Run())
}

func runHelper(mode string) {
	full := fmt.Sprintf(`{"Version":1,"AccessKeyId":%q,"SecretAccessKey":%q,"SessionToken":%q,"Expiration":"2099-01-01T00:00:00Z"}`, procAccess, procSecret, procToken)
	switch mode {
	case "ok":
		fmt.Print(full)
	case "count":
		f, err := os.OpenFile(os.Getenv("S4_HELPER_COUNT"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		if err != nil {
			os.Exit(9)
		}
		_, _ = f.WriteString("x")
		_ = f.Close()
		fmt.Print(full)
	case "window":
		reportWindow(os.Getenv("S4_WINDOW_REPORT"))
		fmt.Print(full)
	case "fail":
		fmt.Fprintln(os.Stderr, "please sign in again")
		os.Exit(3)
	case "garbage":
		fmt.Printf(`{"Version":1,"SecretAccessKey":%q,`, procSecret) // truncated JSON
	case "noversion":
		fmt.Printf(`{"AccessKeyId":%q,"SecretAccessKey":%q}`, procAccess, procSecret)
	case "slow":
		time.Sleep(2 * time.Second)
		fmt.Print(full)
	case "hang":
		if path := os.Getenv("S4_HELPER_PID"); path != "" {
			_ = os.WriteFile(path+".tmp", fmt.Append(nil, os.Getpid()), 0o600)
			_ = os.Rename(path+".tmp", path)
		}
		time.Sleep(2 * time.Minute)
	}
	os.Exit(0)
}

// memVault is an in-memory Vault that ignores case, as Credential Manager does.
type memVault struct {
	mu      sync.Mutex
	entries map[string]string
	setErr  error
	delErr  error
}

func newMemVault() *memVault { return &memVault{entries: map[string]string{}} }

func (v *memVault) Set(user, secret string) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.setErr != nil {
		return v.setErr
	}
	v.entries[strings.ToLower(user)] = secret
	return nil
}

func (v *memVault) Get(user string) (string, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	s, ok := v.entries[strings.ToLower(user)]
	if !ok {
		return "", ErrSecretNotFound
	}
	return s, nil
}

func (v *memVault) Delete(user string) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.delErr != nil {
		return v.delErr
	}
	if _, ok := v.entries[strings.ToLower(user)]; !ok {
		return ErrSecretNotFound
	}
	delete(v.entries, strings.ToLower(user))
	return nil
}

type fixture struct {
	dir, config, credentials string
	vault                    *memVault
}

// newFixture uses temporary AWS files, so the real ~/.aws is never read.
func newFixture(t *testing.T, configText, credentialsText string) *fixture {
	t.Helper()
	dir := t.TempDir()
	f := &fixture{dir: dir, config: filepath.Join(dir, "config"), credentials: filepath.Join(dir, "credentials"), vault: newMemVault()}
	write(t, f.config, configText)
	write(t, f.credentials, credentialsText)
	return f
}

func write(t *testing.T, path, text string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
}

func (f *fixture) open(t *testing.T) *Manager {
	t.Helper()
	m, err := Open(Options{Dir: filepath.Join(f.dir, "state"), Vault: f.vault, AWSConfigFile: f.config, AWSCredentialsFile: f.credentials})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func find(t *testing.T, l Listing, id string) Profile {
	t.Helper()
	for _, p := range l.Profiles {
		if p.ID == id {
			return p
		}
	}
	t.Fatalf("profile %q not listed in %+v", id, l.Profiles)
	return Profile{}
}

func TestListClassifiesAWSProfiles(t *testing.T) {
	f := newFixture(t, `
# a comment
[default]
region = us-west-2
[profile proc]
region = eu-west-1
credential_process = helper.exe --flag
[profile role]
role_arn = arn:aws:iam::123456789012:role/x
source_profile = keys
[profile sso]
sso_account_id = 123456789012
sso_role_name = r
sso_region = us-east-1
sso_start_url = https://example.invalid/start
[profile bare]
region = us-east-1
[sso-session corp]
sso_region = us-east-1
sso_start_url = https://example.invalid/start
[services local]
s3 =
  endpoint_url = https://example.invalid
[notaprofile]
region = us-east-1
`, "[default]\naws_access_key_id = "+fakeAccess+"\naws_secret_access_key = "+fakeSecret+"\n"+
		"[keys]\naws_access_key_id = "+fakeAccess+"\naws_secret_access_key = "+fakeSecret+"\n"+
		"[temp]\naws_access_key_id = "+fakeAccess+"\naws_secret_access_key = "+fakeSecret+"\naws_session_token = tok\n")
	l := f.open(t).List(context.Background())

	want := map[string]struct {
		kind      Kind
		supported bool
	}{
		"default": {KindStatic, true}, "keys": {KindStatic, true}, "temp": {KindSession, true},
		"proc": {KindProcess, true}, "role": {KindOther, false}, "sso": {KindOther, false}, "bare": {KindNone, false},
	}
	if len(l.Profiles) != len(want) {
		t.Fatalf("got %d profiles: %+v", len(l.Profiles), l.Profiles)
	}
	for name, w := range want {
		p := find(t, l, AWSID(name))
		if p.Kind != w.kind || p.Supported != w.supported {
			t.Errorf("%s: kind %s supported %v, want %s %v (%s)", name, p.Kind, p.Supported, w.kind, w.supported, p.Problem)
		}
		if !w.supported && p.Problem == "" {
			t.Errorf("%s: an unsupported profile must say why", name)
		}
	}
	if got := find(t, l, AWSID("proc")).Region; got != "eu-west-1" {
		t.Errorf("region %q", got)
	}
}

func TestMissingAWSFilesAreNotAnError(t *testing.T) {
	dir := t.TempDir()
	m, err := Open(Options{Dir: dir, Vault: newMemVault(), AWSConfigFile: filepath.Join(dir, "no1"), AWSCredentialsFile: filepath.Join(dir, "no2")})
	if err != nil {
		t.Fatal(err)
	}
	l := m.List(context.Background())
	if len(l.Profiles) != 0 || len(l.Warnings) != 0 {
		t.Fatalf("%+v", l)
	}
}

func TestStaticAndSessionKeysFromFile(t *testing.T) {
	f := newFixture(t, "", "[keys]\naws_access_key_id = "+fakeAccess+"\naws_secret_access_key = "+fakeSecret+"\n"+
		"[temp]\naws_access_key_id = "+fakeAccess+"\naws_secret_access_key = "+fakeSecret+"\naws_session_token = tok1\n")
	m := f.open(t)
	ctx := context.Background()
	p, err := m.Provider(AWSID("keys"))
	if err != nil {
		t.Fatal(err)
	}
	c, err := p.Retrieve(ctx)
	if err != nil || c.AccessKeyID != fakeAccess || c.SecretAccessKey != fakeSecret {
		t.Fatalf("%q %v", c.AccessKeyID, err)
	}

	// A new sign-in rewrites the file. Refresh must find the new session.
	p, _ = m.Provider(AWSID("temp"))
	if c, err = p.Retrieve(ctx); err != nil || c.SessionToken != "tok1" {
		t.Fatalf("%q %v", c.SessionToken, err)
	}
	write(t, f.credentials, "[temp]\naws_access_key_id = "+fakeAccess2+"\naws_secret_access_key = "+fakeSecret2+"\naws_session_token = tok2\n")
	if c, _ = p.Retrieve(ctx); c.SessionToken != "tok1" {
		t.Fatal("credentials must stay cached until a refresh")
	}
	st, err := m.Refresh(ctx, AWSID("temp"))
	if err != nil || st.KeyHint != "...0002" {
		t.Fatalf("%+v %v", st, err)
	}
	if c, _ = p.Retrieve(ctx); c.SessionToken != "tok2" || c.AccessKeyID != fakeAccess2 {
		t.Fatalf("the same provider must return the new session, got %q", c.SessionToken)
	}
}

func TestAppProfileLifecycle(t *testing.T) {
	f := newFixture(t, "", "")
	m := f.open(t)
	ctx := context.Background()

	added, err := m.AddKeys("work", "  "+fakeAccess+"\n", fakeSecret+"\n", " us-west-2 ")
	if err != nil {
		t.Fatal(err)
	}
	if added.ID != "app:work" || added.Region != "us-west-2" || added.KeyHint != "...0001" {
		t.Fatalf("%+v", added)
	}
	provider, err := m.Provider(added.ID)
	if err != nil {
		t.Fatal(err)
	}
	c, err := provider.Retrieve(ctx)
	if err != nil || c.AccessKeyID != fakeAccess || c.SecretAccessKey != fakeSecret {
		t.Fatalf("%q %v", c.AccessKeyID, err)
	}

	// No key material on disk, in any file the manager wrote.
	err = filepath.WalkDir(filepath.Join(f.dir, "state"), func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, err := os.ReadFile(path)
		if err == nil && (strings.Contains(string(data), fakeSecret) || strings.Contains(string(data), fakeAccess)) {
			t.Errorf("%s holds key material", path)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}

	// New keys reach the provider that already exists.
	if err := m.UpdateKeys(added.ID, fakeAccess2, fakeSecret2); err != nil {
		t.Fatal(err)
	}
	if c, _ = provider.Retrieve(ctx); c.AccessKeyID != fakeAccess2 {
		t.Fatalf("got %q after UpdateKeys", c.AccessKeyID)
	}

	// The profile and the selection survive a restart.
	if err := m.SetActive(ctx, added.ID); err != nil {
		t.Fatal(err)
	}
	m = f.open(t)
	l := m.List(ctx)
	if l.ActiveID != added.ID || find(t, l, added.ID).KeyHint != "...0002" {
		t.Fatalf("%+v", l)
	}

	provider, err = m.Provider(added.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.Retrieve(ctx); err != nil {
		t.Fatal(err)
	}
	if err := m.Delete(added.ID); err != nil {
		t.Fatal(err)
	}
	if len(f.vault.entries) != 0 {
		t.Fatal("the vault entry must be removed")
	}
	if l = m.List(ctx); len(l.Profiles) != 0 || l.ActiveID != "" {
		t.Fatalf("%+v", l)
	}
	if _, err := provider.Retrieve(ctx); err == nil {
		// The old provider still holds cached keys only if Delete failed to
		// invalidate them.
		t.Fatal("a deleted profile must stop producing credentials")
	} else if !errors.Is(err, engine.ErrCredentialsUnavailable) {
		t.Fatalf("%v", err)
	}
}

func TestAddKeysValidation(t *testing.T) {
	f := newFixture(t, "", "")
	m := f.open(t)
	for _, name := range []string{"", "a b", "../x", "-x", strings.Repeat("a", 65), "a:b", "a/b"} {
		if _, err := m.AddKeys(name, fakeAccess, fakeSecret, ""); !errors.Is(err, ErrInvalidName) {
			t.Errorf("name %q: %v", name, err)
		}
	}
	for _, k := range [][2]string{{"", fakeSecret}, {fakeAccess, ""}, {"short", fakeSecret}, {fakeAccess, "two words"}, {"AKIA!!!!!!!!!!!!!!!!", fakeSecret}} {
		_, err := m.AddKeys("ok", k[0], k[1], "")
		if !errors.Is(err, ErrInvalidKeys) {
			t.Errorf("keys %q: %v", k[0], err)
		}
		if err != nil && k[1] != "" && strings.Contains(err.Error(), k[1]) {
			t.Errorf("error repeats the secret: %v", err)
		}
	}
	if _, err := m.AddKeys("Work", fakeAccess, fakeSecret, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := m.AddKeys("work", fakeAccess2, fakeSecret2, ""); !errors.Is(err, ErrProfileExists) {
		t.Fatalf("names that differ only by case share a vault entry: %v", err)
	}
	c, err := mustProvider(t, m, "app:Work").Retrieve(context.Background())
	if err != nil || c.AccessKeyID != fakeAccess {
		t.Fatalf("the first profile's keys were replaced: %q %v", c.AccessKeyID, err)
	}
}

func mustProvider(t *testing.T, m *Manager, id string) interface {
	Retrieve(context.Context) (awsCreds, error)
} {
	t.Helper()
	p, err := m.Provider(id)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestAWSProfilesAreReadOnly(t *testing.T) {
	f := newFixture(t, "", "[keys]\naws_access_key_id = "+fakeAccess+"\naws_secret_access_key = "+fakeSecret+"\n")
	m := f.open(t)
	before, _ := os.ReadFile(f.credentials)
	if err := m.Delete(AWSID("keys")); !errors.Is(err, ErrReadOnlyProfile) {
		t.Fatalf("%v", err)
	}
	if err := m.UpdateKeys(AWSID("keys"), fakeAccess2, fakeSecret2); !errors.Is(err, ErrReadOnlyProfile) {
		t.Fatalf("%v", err)
	}
	if err := m.SetActive(context.Background(), AWSID("keys")); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(f.credentials)
	if string(before) != string(after) {
		t.Fatal("the AWS credentials file was changed")
	}
}

func TestSwitchingKeepsEachProfilesCredentials(t *testing.T) {
	f := newFixture(t, "", "[keys]\naws_access_key_id = "+fakeAccess2+"\naws_secret_access_key = "+fakeSecret2+"\n")
	m := f.open(t)
	ctx := context.Background()
	if _, err := m.AddKeys("work", fakeAccess, fakeSecret, ""); err != nil {
		t.Fatal(err)
	}
	a, b := mustProvider(t, m, "app:work"), mustProvider(t, m, "aws:keys")
	for _, id := range []string{"app:work", "aws:keys", "app:work"} {
		if err := m.SetActive(ctx, id); err != nil {
			t.Fatal(err)
		}
		if got := m.List(ctx).ActiveID; got != id {
			t.Fatalf("active %q, want %q", got, id)
		}
		// A transfer that holds a provider keeps its own profile after a switch.
		ca, err1 := a.Retrieve(ctx)
		cb, err2 := b.Retrieve(ctx)
		if err1 != nil || err2 != nil || ca.AccessKeyID != fakeAccess || cb.AccessKeyID != fakeAccess2 {
			t.Fatalf("%q %q %v %v", ca.AccessKeyID, cb.AccessKeyID, err1, err2)
		}
	}
	if err := m.SetActive(ctx, "aws:nosuch"); !errors.Is(err, ErrProfileNotFound) {
		t.Fatalf("%v", err)
	}
	if got := m.List(ctx).ActiveID; got != "app:work" {
		t.Fatalf("a refused switch changed the selection to %q", got)
	}
}

func TestActiveProfileThatDisappearedIsNotActive(t *testing.T) {
	f := newFixture(t, "", "[keys]\naws_access_key_id = "+fakeAccess+"\naws_secret_access_key = "+fakeSecret+"\n")
	m := f.open(t)
	ctx := context.Background()
	if err := m.SetActive(ctx, AWSID("keys")); err != nil {
		t.Fatal(err)
	}
	write(t, f.credentials, "")
	if got := m.List(ctx).ActiveID; got != "" {
		t.Fatalf("active %q", got)
	}
}

func TestVaultFailuresLeaveNoHalfProfile(t *testing.T) {
	f := newFixture(t, "", "")
	m := f.open(t)
	f.vault.setErr = errors.New("vault locked")
	if _, err := m.AddKeys("work", fakeAccess, fakeSecret, ""); err == nil {
		t.Fatal("want an error")
	}
	if l := m.List(context.Background()); len(l.Profiles) != 0 {
		t.Fatalf("%+v", l.Profiles)
	}
	f.vault.setErr = nil
	p, err := m.AddKeys("work", fakeAccess, fakeSecret, "")
	if err != nil {
		t.Fatal(err)
	}
	f.vault.delErr = errors.New("vault locked")
	if err := m.Delete(p.ID); err == nil {
		t.Fatal("want an error")
	}
	if l := m.List(context.Background()); len(l.Profiles) != 1 {
		t.Fatal("a profile whose secret is still stored must stay listed")
	}
	f.vault.delErr = nil
	delete(f.vault.entries, vaultUser("work"))
	if err := m.Delete(p.ID); err != nil {
		t.Fatalf("a missing vault entry must not block the delete: %v", err)
	}
}

func TestMissingVaultEntryIsACredentialError(t *testing.T) {
	f := newFixture(t, "", "")
	m := f.open(t)
	p, err := m.AddKeys("work", fakeAccess, fakeSecret, "")
	if err != nil {
		t.Fatal(err)
	}
	delete(f.vault.entries, vaultUser("work"))
	_, err = m.Refresh(context.Background(), p.ID)
	if !errors.Is(err, ErrSecretNotFound) || !errors.Is(err, engine.ErrCredentialsUnavailable) {
		t.Fatalf("%v", err)
	}
	if engine.Classify(err) != engine.ClassCredential {
		t.Fatalf("class %v", engine.Classify(err))
	}
}

func TestDamagedStateFileIsKept(t *testing.T) {
	f := newFixture(t, "", "")
	state := filepath.Join(f.dir, "state")
	if err := os.MkdirAll(state, 0o700); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(state, stateFileName), "{not json")
	if _, err := Open(Options{Dir: state, Vault: f.vault}); err == nil {
		t.Fatal("want an error")
	}
	data, _ := os.ReadFile(filepath.Join(state, stateFileName))
	if string(data) != "{not json" {
		t.Fatal("the damaged file was replaced")
	}
}

func TestUnparsableProfileDoesNotLeakOrHideOthers(t *testing.T) {
	f := newFixture(t, "[profile loop]\nrole_arn = arn:aws:iam::123456789012:role/x\nsource_profile = loop2\n"+
		"[profile loop2]\nrole_arn = arn:aws:iam::123456789012:role/y\nsource_profile = missing\n",
		"[good]\naws_access_key_id = "+fakeAccess+"\naws_secret_access_key = "+fakeSecret+"\n")
	l := f.open(t).List(context.Background())
	if !find(t, l, AWSID("good")).Supported {
		t.Fatal("a broken profile must not affect a good one")
	}
	if p := find(t, l, AWSID("loop")); p.Supported || p.Problem == "" {
		t.Fatalf("%+v", p)
	}
}

// helperExe copies this test binary to a folder whose name has a space and
// returns the quoted command that runs it.
func helperExe(t *testing.T) string {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(exe)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "helper dir")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	copyPath := filepath.Join(dir, "cred helper.exe")
	if err := os.WriteFile(copyPath, data, 0o700); err != nil {
		t.Fatal(err)
	}
	return `"` + copyPath + `"`
}

func processFixture(t *testing.T, mode string) (*fixture, *Manager) {
	t.Helper()
	t.Setenv("S4_CRED_HELPER", mode)
	f := newFixture(t, "[profile proc]\ncredential_process = "+helperExe(t)+"\n", "")
	return f, f.open(t)
}

func TestCredentialProcessQuotedPath(t *testing.T) {
	_, m := processFixture(t, "ok")
	st, err := m.Refresh(context.Background(), AWSID("proc"))
	if err != nil {
		t.Fatal(err)
	}
	if st.KeyHint != "...EY01" || st.Expires == nil || st.Expires.Year() != 2099 {
		t.Fatalf("%+v", st)
	}
	p, _ := m.Provider(AWSID("proc"))
	c, err := p.Retrieve(context.Background())
	if err != nil || c.AccessKeyID != procAccess || c.SecretAccessKey != procSecret || c.SessionToken != procToken {
		t.Fatalf("%q %v", c.AccessKeyID, err)
	}
}

// The SDK's parser removes outer quotes, so these commands fail without
// awsFiles.processCommand.
func TestCredentialProcessKeepsQuotesTheParserRemoves(t *testing.T) {
	t.Setenv("S4_CRED_HELPER", "ok")
	exe := helperExe(t)
	for name, command := range map[string]string{
		"whole value": exe,
		"last arg":    exe + ` --name "c d"`,
		"comment":     exe + ` --name "c d" # a comment`,
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t, "[profile other]\ncredential_process = wrong.exe\n[profile proc]\nregion = us-east-1\ncredential_process = "+command+"\n", "")
			if _, err := f.open(t).Refresh(context.Background(), AWSID("proc")); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestCredentialProcessBatchFile(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("windows only")
	}
	dir := filepath.Join(t.TempDir(), "with space")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(dir, "helper.cmd")
	write(t, script, fmt.Sprintf("@echo {\"Version\":1,\"AccessKeyId\":\"%s\",\"SecretAccessKey\":\"%s\"}\r\n", procAccess, procSecret))
	p := NewProcessProvider(`"` + script + `"`)
	c, err := p.Retrieve(context.Background())
	if err != nil || c.AccessKeyID != procAccess {
		t.Fatalf("%q %v", c.AccessKeyID, err)
	}
}

func TestCredentialProcessRunsOncePerExpiry(t *testing.T) {
	_, m := processFixture(t, "count")
	counter := filepath.Join(t.TempDir(), "count")
	t.Setenv("S4_HELPER_COUNT", counter)
	p, _ := m.Provider(AWSID("proc"))
	var wg sync.WaitGroup
	for range 16 {
		wg.Go(func() {
			if _, err := p.Retrieve(context.Background()); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	data, err := os.ReadFile(counter)
	if err != nil || len(data) != 1 {
		t.Fatalf("the helper ran %d times: %v", len(data), err)
	}
}

func TestCredentialProcessFailures(t *testing.T) {
	cases := []struct{ mode, want string }{
		{"fail", "code 3: please sign in again"},
		{"garbage", "not valid credential JSON"},
		{"noversion", "missing required fields"},
	}
	for _, tc := range cases {
		t.Run(tc.mode, func(t *testing.T) {
			_, m := processFixture(t, tc.mode)
			_, err := m.Refresh(context.Background(), AWSID("proc"))
			if !errors.Is(err, engine.ErrCredentialsUnavailable) {
				t.Fatalf("%v", err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not contain %q", err, tc.want)
			}
			if strings.Contains(err.Error(), procSecret) {
				t.Fatalf("error leaks the secret: %v", err)
			}
		})
	}
}

// The SDK's credentials cache detaches a refresh from the caller's context, so
// the caller returns at once while the helper runs on, up to processTimeout.
// TestCancelEndsTheHelperProcess covers cancelling the provider itself.
func TestRefreshReturnsWhenCallerGivesUp(t *testing.T) {
	t.Setenv("S4_CRED_HELPER", "slow")
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if strings.ContainsAny(exe, " \"") {
		t.Skipf("test binary path %q needs quoting", exe)
	}
	f := newFixture(t, "[profile proc]\ncredential_process = "+exe+"\n", "")
	m := f.open(t)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := m.Refresh(ctx, AWSID("proc")); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("%v", err)
	}
	if d := time.Since(start); d > 1500*time.Millisecond {
		t.Fatalf("Refresh took %s", d)
	}
	// Join the run still in progress, so the helper has ended when the test does.
	p, err := m.Provider(AWSID("proc"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Retrieve(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestMissingHelperFails(t *testing.T) {
	p := NewProcessProvider(`"` + filepath.Join(t.TempDir(), "no-such-helper.exe") + `"`)
	if _, err := p.Retrieve(context.Background()); err == nil {
		t.Fatal("want an error")
	}
}

func TestUnknownProfileIDs(t *testing.T) {
	m := newFixture(t, "", "").open(t)
	for _, id := range []string{"", "work", "app:", "aws:", "other:x"} {
		if _, err := m.Provider(id); !errors.Is(err, ErrProfileNotFound) {
			t.Errorf("%q: %v", id, err)
		}
	}
	if _, err := m.Refresh(context.Background(), AWSID("nosuch")); !errors.Is(err, engine.ErrCredentialsUnavailable) {
		t.Fatalf("%v", err)
	}
}

// TestCredentialChangesAreAnnounced: everything that changes keys tells the
// listeners, so waiting transfers and deferred cleanup retry at once. Refresh,
// which the credential prober calls, must not, or a probe would start a probe.
func TestCredentialChangesAreAnnounced(t *testing.T) {
	f := newFixture(t, "", "[temp]\naws_access_key_id = "+fakeAccess+"\naws_secret_access_key = "+fakeSecret+"\naws_session_token = tok1\n")
	m := f.open(t)
	var mu sync.Mutex
	var heard []string
	m.OnCredentialsChanged(func(id string) {
		// The lock must be free, so a listener may call back into the Manager.
		_ = m.List(context.Background())
		mu.Lock()
		heard = append(heard, id)
		mu.Unlock()
	})
	got := func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), heard...)
	}
	ctx := context.Background()

	if _, err := m.Refresh(ctx, AWSID("temp")); err != nil {
		t.Fatal(err)
	}
	if len(got()) != 0 {
		t.Fatalf("Refresh announced a change: %v", got())
	}
	if _, err := m.Reload(ctx, AWSID("temp")); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Reload(ctx, AWSID("missing")); err == nil {
		t.Fatal("reloading a missing profile succeeded")
	}
	if _, err := m.AddKeys("work", fakeAccess, fakeSecret, "us-east-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.AddKeys("work", fakeAccess, fakeSecret, "us-east-1"); err == nil {
		t.Fatal("a duplicate profile was added")
	}
	if err := m.UpdateKeys(AppID("work"), fakeAccess2, fakeSecret2); err != nil {
		t.Fatal(err)
	}
	f.vault.setErr = errors.New("vault is locked")
	if err := m.UpdateKeys(AppID("work"), fakeAccess, fakeSecret); err == nil {
		t.Fatal("a failed vault write succeeded")
	}
	f.vault.setErr = nil
	if err := m.Delete(AppID("work")); err != nil {
		t.Fatal(err)
	}
	want := []string{AWSID("temp"), AppID("work"), AppID("work"), AppID("work")}
	if !slices.Equal(got(), want) {
		t.Fatalf("heard %v, want %v", got(), want)
	}
}
