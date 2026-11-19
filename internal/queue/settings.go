package queue

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"time"

	"s4browser/internal/engine"
)

// SettingsFileName is the settings file, next to profiles.json.
const SettingsFileName = "transfer-settings.json"

const settingsVersion = 1

// TransferSettings are the limits the user can change while transfers run. The
// defaults are the engine's.
type TransferSettings struct {
	// ParallelFiles is how many files transfer at once.
	ParallelFiles int `json:"parallelFiles"`
	// PartsPerFile is how many parts of one file are in flight at once.
	PartsPerFile int `json:"partsPerFile"`
	// TotalParts is how many parts are in flight across all files.
	TotalParts int `json:"totalParts"`
	// PartSize is the preferred part size in bytes, or 0 for the default (64 MiB).
	// It applies to jobs that have not been planned yet.
	PartSize int64 `json:"partSize"`
	// UploadBytesPerSec and DownloadBytesPerSec cap the total rate of each
	// direction. 0 means unlimited. They apply at once.
	UploadBytesPerSec   int64 `json:"uploadBytesPerSec"`
	DownloadBytesPerSec int64 `json:"downloadBytesPerSec"`
}

// SettingsLimits are the accepted ranges, for the settings dialog.
type SettingsLimits struct {
	MinParallelFiles int   `json:"minParallelFiles"`
	MaxParallelFiles int   `json:"maxParallelFiles"`
	MinPartsPerFile  int   `json:"minPartsPerFile"`
	MaxPartsPerFile  int   `json:"maxPartsPerFile"`
	MinTotalParts    int   `json:"minTotalParts"`
	MaxTotalParts    int   `json:"maxTotalParts"`
	MinPartSize      int64 `json:"minPartSize"`
	MaxPartSize      int64 `json:"maxPartSize"`
}

// SettingsState is what the settings dialog needs.
type SettingsState struct {
	Current  TransferSettings `json:"current"`
	Defaults TransferSettings `json:"defaults"`
	Limits   SettingsLimits   `json:"limits"`
}

func fromEngine(s engine.Settings) TransferSettings {
	return TransferSettings{
		ParallelFiles: s.MaxActiveFiles, PartsPerFile: s.MaxPartsPerFile, TotalParts: s.MaxActiveParts,
		PartSize: s.PartSize, UploadBytesPerSec: s.UploadBytesPerSec, DownloadBytesPerSec: s.DownloadBytesPerSec,
	}
}

func (t TransferSettings) engine() engine.Settings {
	return engine.Settings{
		MaxActiveFiles: t.ParallelFiles, MaxPartsPerFile: t.PartsPerFile, MaxActiveParts: t.TotalParts,
		PartSize: t.PartSize, UploadBytesPerSec: t.UploadBytesPerSec, DownloadBytesPerSec: t.DownloadBytesPerSec,
	}
}

// DefaultTransferSettings returns the engine defaults.
func DefaultTransferSettings() TransferSettings { return fromEngine(engine.DefaultSettings()) }

type settingsDisk struct {
	Version int `json:"version"`
	TransferSettings
}

// settingsFile keeps the current settings and their file.
type settingsFile struct {
	path string
	mu   sync.Mutex
	cur  TransferSettings
}

// openSettings reads the settings. A missing file gives the defaults. A damaged
// or invalid one is set aside, the defaults are used and a warning is returned.
func openSettings(dir string) (*settingsFile, string) {
	f := &settingsFile{path: filepath.Join(dir, SettingsFileName), cur: DefaultTransferSettings()}
	data, err := os.ReadFile(f.path)
	if errors.Is(err, fs.ErrNotExist) {
		return f, ""
	}
	if err == nil {
		d := settingsDisk{Version: settingsVersion, TransferSettings: f.cur}
		if err = json.Unmarshal(data, &d); err == nil && d.Version != settingsVersion {
			err = fmt.Errorf("unsupported version %d", d.Version)
		}
		if err == nil {
			err = d.TransferSettings.engine().Validate()
		}
		if err == nil {
			f.cur = d.TransferSettings
			return f, ""
		}
	}
	moved := SettingsFileName + ".bad-" + time.Now().UTC().Format("20060102T150405")
	if rerr := os.Rename(f.path, filepath.Join(dir, moved)); rerr != nil {
		return f, fmt.Sprintf("The transfer settings file could not be used (%v), so the defaults apply.", err)
	}
	return f, fmt.Sprintf("The transfer settings file could not be used (%v). It was kept as %s and the defaults apply.", err, moved)
}

func (f *settingsFile) current() TransferSettings {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.cur
}

// save writes the settings atomically and makes them current.
func (f *settingsFile) save(t TransferSettings) error {
	data, err := json.MarshalIndent(settingsDisk{Version: settingsVersion, TransferSettings: t}, "", "  ")
	if err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := os.MkdirAll(filepath.Dir(f.path), 0o700); err != nil {
		return err
	}
	tmp := f.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, f.path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	f.cur = t
	return nil
}

// Settings returns the current settings with the defaults and limits.
func (s *Service) Settings() SettingsState {
	return SettingsState{
		Current: s.settings.current(), Defaults: DefaultTransferSettings(),
		Limits: SettingsLimits{
			MinParallelFiles: engine.MinActiveFiles, MaxParallelFiles: engine.MaxActiveFilesLimit,
			MinPartsPerFile: engine.MinPartsPerFile, MaxPartsPerFile: engine.MaxPartsPerFileLimit,
			MinTotalParts: engine.MinActiveParts, MaxTotalParts: engine.MaxActivePartsLimit,
			MinPartSize: engine.MinPartSize, MaxPartSize: engine.MaxPartSize,
		},
	}
}

// SetSettings validates the settings, saves them and applies them to the running
// engine. Concurrency limits apply to the next dispatch and never interrupt a
// running part, bandwidth caps apply at once and the part size applies to jobs
// not yet planned. Nothing is applied if the file cannot be saved.
func (s *Service) SetSettings(ctx context.Context, t TransferSettings) (SettingsState, error) {
	if err := t.engine().Validate(); err != nil {
		return SettingsState{}, describe(err)
	}
	s.setMu.Lock()
	defer s.setMu.Unlock()
	prev := s.settings.current()
	if err := s.settings.save(t); err != nil {
		return SettingsState{}, fmt.Errorf("the settings could not be saved: %w", err)
	}
	if err := s.eng.UpdateSettings(ctx, t.engine()); err != nil {
		// Keep the file and the engine in agreement.
		_ = s.settings.save(prev)
		return SettingsState{}, describe(err)
	}
	return s.Settings(), nil
}
