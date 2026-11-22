// Package creds finds the profiles the app can sign in with and turns a profile
// into a refreshing AWS credentials provider.
//
// A profile comes from one of two places. App profiles hold static keys in the
// operating system's credential vault, and only their name, region and a short
// key hint are written to disk. AWS profiles are read from the shared AWS config
// and credentials files, which the app never writes.
package creds

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// Source says where a profile is defined.
type Source string

// Profile sources.
const (
	SourceApp Source = "app"
	SourceAWS Source = "aws"
)

// Kind says how a profile obtains credentials.
type Kind string

// Profile kinds. KindVaultKeys, KindStatic, KindSession, KindProcess, KindLogin
// and KindRole are the supported ones.
const (
	// KindVaultKeys is a static key pair held in the OS vault.
	KindVaultKeys Kind = "vault-keys"
	// KindStatic is a long-lived key pair in the shared AWS files.
	KindStatic Kind = "static"
	// KindSession is a temporary key pair with a session token in the shared AWS
	// files, usually written by an external sign-in tool. Nothing refreshes it
	// automatically: it is re-read from the file after a new sign-in.
	KindSession Kind = "session"
	// KindProcess runs a credential_process helper.
	KindProcess Kind = "credential-process"
	// KindLogin is a profile written by `aws login` (login_session). The SDK
	// refreshes its credentials from the token cache that the CLI keeps.
	KindLogin Kind = "login"
	// KindRole assumes a role (role_arn) using another profile as its source.
	// It is supported when the chain ends in a login, static, session or
	// credential_process profile.
	KindRole Kind = "role"
	// KindOther is an SSO or web identity profile, or a role whose chain ends in
	// one. The SDK may be able to use it, but it is untested and unsupported.
	KindOther Kind = "other"
	// KindNone is a profile with no usable credentials, such as a region-only
	// section.
	KindNone Kind = "none"
)

// Profile describes one selectable profile. It never holds secret values.
type Profile struct {
	// ID is unique across sources: "app:<name>" or "aws:<name>". It is the value
	// to use for engine.ObjectRef.Profile.
	ID     string `json:"id"`
	Name   string `json:"name"`
	Source Source `json:"source"`
	Kind   Kind   `json:"kind"`
	// Region is the profile's default region, if it has one.
	Region string `json:"region,omitempty"`
	// KeyHint is the end of an app profile's access key ID.
	KeyHint string `json:"keyHint,omitempty"`
	// Supported is false for kinds that are untested or cannot work.
	Supported bool `json:"supported"`
	// Problem explains why the profile cannot be used as it stands.
	Problem string `json:"problem,omitempty"`
}

// Errors returned by the package. None of them carries a secret value.
var (
	ErrInvalidName     = errors.New("creds: a profile name is 1 to 64 letters, digits, dots, dashes or underscores, starting with a letter or digit")
	ErrInvalidKeys     = errors.New("creds: the access key ID or secret access key is not valid")
	ErrProfileExists   = errors.New("creds: a profile with this name already exists")
	ErrProfileNotFound = errors.New("creds: profile not found")
	ErrReadOnlyProfile = errors.New("creds: AWS profiles are managed in the AWS config files")
	ErrSecretNotFound  = errors.New("creds: the keys for this profile are not in the credential vault")
	ErrNoCredentials   = errors.New("creds: this profile has no credentials")
	ErrProfileInvalid  = errors.New("creds: the AWS config files could not be read for this profile")
	// ErrLoginExpired means an aws login session cannot supply credentials. It
	// carries no token text.
	ErrLoginExpired = errors.New("creds: the aws login session has expired or is missing")
)

const (
	appPrefix = string(SourceApp) + ":"
	awsPrefix = string(SourceAWS) + ":"
)

var (
	namePattern      = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
	accessKeyPattern = regexp.MustCompile(`^[A-Za-z0-9]{16,128}$`)
)

// AppID returns the ID of the app profile called name.
func AppID(name string) string { return appPrefix + name }

// AWSID returns the ID of the AWS profile called name.
func AWSID(name string) string { return awsPrefix + name }

// parseID splits a profile ID into its source and name.
func parseID(id string) (Source, string, error) {
	switch {
	case strings.HasPrefix(id, appPrefix) && len(id) > len(appPrefix):
		return SourceApp, id[len(appPrefix):], nil
	case strings.HasPrefix(id, awsPrefix) && len(id) > len(awsPrefix):
		return SourceAWS, id[len(awsPrefix):], nil
	}
	return "", "", fmt.Errorf("%w: %q", ErrProfileNotFound, id)
}

// validateKeys trims pasted keys and checks their shape. The error never
// repeats the values.
func validateKeys(accessKeyID, secretAccessKey string) (string, string, error) {
	accessKeyID = strings.TrimSpace(accessKeyID)
	secretAccessKey = strings.TrimSpace(secretAccessKey)
	if !accessKeyPattern.MatchString(accessKeyID) || secretAccessKey == "" ||
		len(secretAccessKey) > 512 || strings.ContainsAny(secretAccessKey, " \t\r\n") {
		return "", "", ErrInvalidKeys
	}
	return accessKeyID, secretAccessKey, nil
}

// keyHint keeps only the last four characters of an access key ID.
func keyHint(accessKeyID string) string {
	return "..." + accessKeyID[len(accessKeyID)-4:]
}
