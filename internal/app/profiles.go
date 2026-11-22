package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"time"

	"s4browser/internal/creds"
)

// profileCheckTimeout covers a credential_process helper, which may take up to
// a minute.
const profileCheckTimeout = 75 * time.Second

// openProfiles opens the profile store in the app's config folder.
func openProfiles() (*creds.Manager, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return nil, err
	}
	return creds.Open(creds.Options{Dir: filepath.Join(dir, "s4browser")})
}

// ProfileState is what the profile panel shows.
type ProfileState struct {
	Profiles []creds.Profile `json:"profiles"`
	ActiveID string          `json:"activeId"`
	Warnings []string        `json:"warnings,omitempty"`
	// Error says why the profile store could not be opened.
	Error string `json:"error,omitempty"`
}

// ProfileCheck is the result of loading a profile's credentials.
type ProfileCheck struct {
	OK      bool       `json:"ok"`
	Message string     `json:"message,omitempty"`
	KeyHint string     `json:"keyHint,omitempty"`
	Expires *time.Time `json:"expires,omitempty"`
}

func (a *App) profileManager() (*creds.Manager, error) {
	if a.profiles == nil {
		return nil, errors.New("the profile store is not available: " + a.profilesErr)
	}
	return a.profiles, nil
}

// Profiles lists the profiles and the current selection.
func (a *App) Profiles() ProfileState {
	m, err := a.profileManager()
	if err != nil {
		return ProfileState{Profiles: []creds.Profile{}, Error: err.Error()}
	}
	l := m.List(a.ctx)
	return ProfileState{Profiles: l.Profiles, ActiveID: l.ActiveID, Warnings: l.Warnings}
}

// SelectProfile makes a profile the active one.
func (a *App) SelectProfile(id string) error {
	m, err := a.profileManager()
	if err != nil {
		return err
	}
	return m.SetActive(a.ctx, id)
}

// AddKeyProfile stores a static key pair in the OS vault under a new profile.
func (a *App) AddKeyProfile(name, accessKeyID, secretAccessKey, region string) error {
	m, err := a.profileManager()
	if err != nil {
		return err
	}
	_, err = m.AddKeys(name, accessKeyID, secretAccessKey, region)
	return err
}

// DeleteProfile removes an app profile and its vault entry.
func (a *App) DeleteProfile(id string) error {
	m, err := a.profileManager()
	if err != nil {
		return err
	}
	return m.Delete(id)
}

// CurrentCredentials reports the credentials a profile's cache holds now,
// loading them only if they are missing or expired, so a credential_process
// profile's badge can follow the helper's newer credentials. It makes no AWS
// request and tells no one.
func (a *App) CurrentCredentials(id string) ProfileCheck {
	m, err := a.profileManager()
	if err != nil {
		return ProfileCheck{Message: err.Error()}
	}
	ctx, cancel := context.WithTimeout(a.ctx, profileCheckTimeout)
	defer cancel()
	st, err := m.Current(ctx, id)
	if err != nil {
		return ProfileCheck{Message: err.Error()}
	}
	return ProfileCheck{OK: true, KeyHint: st.KeyHint, Expires: st.Expires}
}

// CheckProfile loads a profile's credentials again, running its helper if it has
// one. It makes no AWS request. Success tells transfers waiting for this
// profile's credentials to try again.
func (a *App) CheckProfile(id string) ProfileCheck {
	m, err := a.profileManager()
	if err != nil {
		return ProfileCheck{Message: err.Error()}
	}
	ctx, cancel := context.WithTimeout(a.ctx, profileCheckTimeout)
	defer cancel()
	st, err := m.Reload(ctx, id)
	if err != nil {
		return ProfileCheck{Message: err.Error()}
	}
	return ProfileCheck{OK: true, KeyHint: st.KeyHint, Expires: st.Expires}
}
