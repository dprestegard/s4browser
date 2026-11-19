package creds

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"

	"s4browser/internal/s3x"
)

const (
	stateFileName    = "profiles.json"
	stateFileVersion = 1
	// refreshAhead renews expiring credentials this long before they expire, so
	// a request signed just before expiry does not fail on arrival.
	refreshAhead = time.Minute
)

// Options configures a Manager.
type Options struct {
	// Dir holds profiles.json. It is created when needed.
	Dir string
	// Vault stores the keys of app profiles. Default SystemVault.
	Vault Vault
	// AWSConfigFile and AWSCredentialsFile override the shared AWS files. By
	// default they follow the AWS environment variables and ~/.aws.
	AWSConfigFile      string
	AWSCredentialsFile string
}

// Listing is the result of Manager.List.
type Listing struct {
	Profiles []Profile `json:"profiles"`
	// ActiveID is the selected profile, or empty if none is selected.
	ActiveID string `json:"activeId"`
	// Warnings describe AWS files that could not be read.
	Warnings []string `json:"warnings,omitempty"`
}

// Status describes credentials that were loaded successfully. It holds no secret.
type Status struct {
	KeyHint string `json:"keyHint"`
	// Expires is the expiry time, or nil if the credentials do not report one.
	Expires *time.Time `json:"expires,omitempty"`
}

// Manager owns the profile list, the active selection and one credentials cache
// per profile. It is safe for concurrent use.
type Manager struct {
	vault Vault
	files awsFiles
	path  string

	mu     sync.Mutex
	state  stateFile
	caches map[string]*aws.CredentialsCache
}

type stateFile struct {
	Version  int          `json:"version"`
	Active   string       `json:"active,omitempty"`
	Profiles []appProfile `json:"profiles"`
}

type appProfile struct {
	Name    string `json:"name"`
	Region  string `json:"region,omitempty"`
	KeyHint string `json:"keyHint"`
}

// Open loads the saved profiles. A damaged file is an error and is left in
// place, because replacing it would lose the user's profile list.
func Open(o Options) (*Manager, error) {
	if o.Dir == "" {
		return nil, errors.New("creds: Options.Dir is required")
	}
	m := &Manager{
		vault:  o.Vault,
		files:  defaultAWSFiles(),
		path:   filepath.Join(o.Dir, stateFileName),
		state:  stateFile{Version: stateFileVersion},
		caches: map[string]*aws.CredentialsCache{},
	}
	if m.vault == nil {
		m.vault = SystemVault()
	}
	if o.AWSConfigFile != "" {
		m.files.config = o.AWSConfigFile
	}
	if o.AWSCredentialsFile != "" {
		m.files.credentials = o.AWSCredentialsFile
	}
	data, err := os.ReadFile(m.path)
	if errors.Is(err, fs.ErrNotExist) {
		return m, nil
	}
	if err != nil {
		return nil, fmt.Errorf("creds: read saved profiles: %w", err)
	}
	if err := json.Unmarshal(data, &m.state); err != nil {
		return nil, fmt.Errorf("creds: %s is damaged: %w", m.path, err)
	}
	if m.state.Version != stateFileVersion {
		return nil, fmt.Errorf("creds: %s has unsupported version %d", m.path, m.state.Version)
	}
	return m, nil
}

// save writes the state atomically.
func (m *Manager) save(next stateFile) error {
	data, err := json.MarshalIndent(next, "", "  ")
	if err != nil {
		return fmt.Errorf("creds: encode profiles: %w", err)
	}
	dir := filepath.Dir(m.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("creds: save profiles: %w", err)
	}
	tmp, err := os.CreateTemp(dir, stateFileName+".*.tmp")
	if err != nil {
		return fmt.Errorf("creds: save profiles: %w", err)
	}
	defer os.Remove(tmp.Name()) // fails harmlessly after the rename
	_, err = tmp.Write(data)
	if err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp.Name(), m.path)
	}
	if err != nil {
		return fmt.Errorf("creds: save profiles: %w", err)
	}
	m.state = next
	return nil
}

// clone copies the state so a failed save leaves the current state unchanged.
func (s stateFile) clone() stateFile {
	s.Profiles = slices.Clone(s.Profiles)
	return s
}

func (m *Manager) findApp(name string) int {
	return slices.IndexFunc(m.state.Profiles, func(p appProfile) bool { return p.Name == name })
}

// List returns app profiles, then AWS profiles, each sorted by name. The AWS
// files are read on every call, so edits appear without a restart.
func (m *Manager) List(ctx context.Context) Listing {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.listLocked(ctx)
}

func (m *Manager) listLocked(ctx context.Context) Listing {
	out := Listing{Profiles: []Profile{}}
	for _, p := range m.state.Profiles {
		out.Profiles = append(out.Profiles, Profile{
			ID: AppID(p.Name), Name: p.Name, Source: SourceApp, Kind: KindVaultKeys,
			Region: p.Region, KeyHint: p.KeyHint, Supported: true,
		})
	}
	names, warnings := m.files.names()
	out.Warnings = warnings
	for _, name := range names {
		out.Profiles = append(out.Profiles, m.files.describe(ctx, name))
	}
	slices.SortFunc(out.Profiles, func(a, b Profile) int {
		if a.Source != b.Source {
			return strings.Compare(string(a.Source), string(b.Source))
		}
		return strings.Compare(a.Name, b.Name)
	})
	if slices.ContainsFunc(out.Profiles, func(p Profile) bool { return p.ID == m.state.Active }) {
		out.ActiveID = m.state.Active
	}
	return out
}

// SetActive selects a profile and remembers it across restarts. Running
// transfers are not affected, because each one keeps the profile it started with.
func (m *Manager) SetActive(ctx context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !slices.ContainsFunc(m.listLocked(ctx).Profiles, func(p Profile) bool { return p.ID == id }) {
		return fmt.Errorf("%w: %q", ErrProfileNotFound, id)
	}
	next := m.state.clone()
	next.Active = id
	return m.save(next)
}

// AddKeys creates an app profile. The keys go to the vault and only a hint of
// the access key ID is written to disk.
func (m *Manager) AddKeys(name, accessKeyID, secretAccessKey, region string) (Profile, error) {
	if !namePattern.MatchString(name) {
		return Profile{}, ErrInvalidName
	}
	accessKeyID, secretAccessKey, err := validateKeys(accessKeyID, secretAccessKey)
	if err != nil {
		return Profile{}, err
	}
	region = strings.TrimSpace(region)
	m.mu.Lock()
	defer m.mu.Unlock()
	// Windows Credential Manager ignores case, so "Work" and "work" would share
	// one vault entry.
	if slices.ContainsFunc(m.state.Profiles, func(p appProfile) bool { return strings.EqualFold(p.Name, name) }) {
		return Profile{}, ErrProfileExists
	}
	if err := writeKeys(m.vault, name, accessKeyID, secretAccessKey); err != nil {
		return Profile{}, err
	}
	next := m.state.clone()
	entry := appProfile{Name: name, Region: region, KeyHint: keyHint(accessKeyID)}
	next.Profiles = append(next.Profiles, entry)
	if err := m.save(next); err != nil {
		_ = m.vault.Delete(vaultUser(name))
		return Profile{}, err
	}
	m.invalidateLocked(AppID(name))
	return Profile{
		ID: AppID(name), Name: name, Source: SourceApp, Kind: KindVaultKeys,
		Region: region, KeyHint: entry.KeyHint, Supported: true,
	}, nil
}

// UpdateKeys replaces the keys of an app profile. Transfers using the profile
// pick up the new keys the next time they fetch credentials.
func (m *Manager) UpdateKeys(id, accessKeyID, secretAccessKey string) error {
	source, name, err := parseID(id)
	if err != nil {
		return err
	}
	if source != SourceApp {
		return ErrReadOnlyProfile
	}
	accessKeyID, secretAccessKey, err = validateKeys(accessKeyID, secretAccessKey)
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	i := m.findApp(name)
	if i < 0 {
		return fmt.Errorf("%w: %q", ErrProfileNotFound, id)
	}
	if err := writeKeys(m.vault, name, accessKeyID, secretAccessKey); err != nil {
		return err
	}
	m.invalidateLocked(id)
	next := m.state.clone()
	next.Profiles[i].KeyHint = keyHint(accessKeyID)
	return m.save(next)
}

// Delete removes an app profile and its vault entry. The vault entry goes first:
// if the second step fails, the profile is still listed and can be deleted again,
// so no secret is left behind unseen.
func (m *Manager) Delete(id string) error {
	source, name, err := parseID(id)
	if err != nil {
		return err
	}
	if source != SourceApp {
		return ErrReadOnlyProfile
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	i := m.findApp(name)
	if i < 0 {
		return fmt.Errorf("%w: %q", ErrProfileNotFound, id)
	}
	if err := m.vault.Delete(vaultUser(name)); err != nil && !errors.Is(err, ErrSecretNotFound) {
		return fmt.Errorf("creds: remove keys from the credential vault: %w", err)
	}
	m.invalidateLocked(id)
	next := m.state.clone()
	next.Profiles = slices.Delete(next.Profiles, i, i+1)
	if next.Active == id {
		next.Active = ""
	}
	return m.save(next)
}

// Provider returns the refreshing credentials provider of a profile. Failures
// match engine.ErrCredentialsUnavailable. The same provider is returned for the
// life of the Manager, so clients built from it survive key changes.
func (m *Manager) Provider(id string) (aws.CredentialsProvider, error) {
	return m.cache(id)
}

func (m *Manager) cache(id string) (*aws.CredentialsCache, error) {
	source, name, err := parseID(id)
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if c := m.caches[id]; c != nil {
		return c, nil
	}
	base := aws.CredentialsProviderFunc(func(ctx context.Context) (aws.Credentials, error) {
		if source == SourceApp {
			keys, err := readKeys(m.vault, name)
			if err != nil {
				return aws.Credentials{}, err
			}
			return aws.Credentials{AccessKeyID: keys.AccessKeyID, SecretAccessKey: keys.SecretAccessKey, Source: "s4browser vault"}, nil
		}
		return m.files.retrieve(ctx, name)
	})
	c := aws.NewCredentialsCache(s3x.MarkCredentialErrors(base), func(o *aws.CredentialsCacheOptions) {
		o.ExpiryWindow = refreshAhead
	})
	m.caches[id] = c
	return c, nil
}

func (m *Manager) invalidateLocked(id string) {
	if c := m.caches[id]; c != nil {
		c.Invalidate()
	}
}

// Refresh discards a profile's cached credentials and loads them again. It runs
// the credential_process helper or re-reads the vault or AWS files. It makes no
// AWS request, so success does not prove that AWS accepts the credentials.
//
// If ctx ends first, Refresh returns but the SDK's cache lets a running helper
// finish, for at most processTimeout. Concurrent refreshes share that one run.
func (m *Manager) Refresh(ctx context.Context, id string) (Status, error) {
	c, err := m.cache(id)
	if err != nil {
		return Status{}, err
	}
	c.Invalidate()
	creds, err := c.Retrieve(ctx)
	if err != nil {
		return Status{}, err
	}
	st := Status{}
	if len(creds.AccessKeyID) >= 4 {
		st.KeyHint = keyHint(creds.AccessKeyID)
	}
	if creds.CanExpire {
		// The cache reports expiry early by its refresh window.
		expires := creds.Expires.Add(refreshAhead)
		st.Expires = &expires
	}
	return st, nil
}
