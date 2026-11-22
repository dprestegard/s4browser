package creds

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/zalando/go-keyring"
)

// vaultService is the service name of every vault entry. On Windows an entry
// appears in Credential Manager as "s4browser:profile/<name>". On macOS it is a
// generic password in the login Keychain, service "s4browser", account
// "profile/<name>".
const vaultService = "s4browser"

// Vault stores one secret string per user name. Get and Delete return
// ErrSecretNotFound for a missing entry.
type Vault interface {
	Set(user, secret string) error
	Get(user string) (string, error)
	Delete(user string) error
}

// SystemVault returns the OS credential vault: Windows Credential Manager or the
// macOS Keychain.
func SystemVault() Vault { return osVault{service: vaultService} }

type osVault struct{ service string }

func (v osVault) Set(user, secret string) error {
	return keyring.Set(v.service, user, secret)
}

func (v osVault) Get(user string) (string, error) {
	s, err := keyring.Get(v.service, user)
	if errors.Is(err, keyring.ErrNotFound) {
		return "", ErrSecretNotFound
	}
	return s, err
}

func (v osVault) Delete(user string) error {
	err := keyring.Delete(v.service, user)
	if errors.Is(err, keyring.ErrNotFound) {
		return ErrSecretNotFound
	}
	return err
}

// vaultUser is the vault entry of an app profile.
func vaultUser(name string) string { return "profile/" + name }

// vaultKeys is the JSON stored in the vault.
type vaultKeys struct {
	AccessKeyID     string `json:"accessKeyId"`
	SecretAccessKey string `json:"secretAccessKey"`
}

func writeKeys(v Vault, name, accessKeyID, secretAccessKey string) error {
	blob, err := json.Marshal(vaultKeys{accessKeyID, secretAccessKey})
	if err != nil {
		return fmt.Errorf("creds: encode keys: %w", err)
	}
	if err := v.Set(vaultUser(name), string(blob)); err != nil {
		return fmt.Errorf("creds: save keys to the credential vault: %w", err)
	}
	return nil
}

func readKeys(v Vault, name string) (vaultKeys, error) {
	blob, err := v.Get(vaultUser(name))
	if err != nil {
		if errors.Is(err, ErrSecretNotFound) {
			return vaultKeys{}, ErrSecretNotFound
		}
		return vaultKeys{}, fmt.Errorf("creds: read keys from the credential vault: %w", err)
	}
	var keys vaultKeys
	// The decode error is dropped on purpose: it can quote the stored value.
	if json.Unmarshal([]byte(blob), &keys) != nil || keys.AccessKeyID == "" || keys.SecretAccessKey == "" {
		return vaultKeys{}, fmt.Errorf("%w: the stored entry is damaged", ErrSecretNotFound)
	}
	return keys, nil
}
