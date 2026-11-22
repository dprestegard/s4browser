package creds

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials/stscreds"
	"github.com/aws/aws-sdk-go-v2/service/sts"
)

// awsFiles names the shared AWS config and credentials files.
type awsFiles struct {
	config      string
	credentials string
}

// defaultAWSFiles honours AWS_CONFIG_FILE and AWS_SHARED_CREDENTIALS_FILE, as
// the AWS CLI does.
func defaultAWSFiles() awsFiles {
	f := awsFiles{os.Getenv("AWS_CONFIG_FILE"), os.Getenv("AWS_SHARED_CREDENTIALS_FILE")}
	if f.config == "" {
		f.config = config.DefaultSharedConfigFilename()
	}
	if f.credentials == "" {
		f.credentials = config.DefaultSharedCredentialsFilename()
	}
	return f
}

// names lists the profile names in both files. A missing file is normal. An
// unreadable file is reported in warnings, and the other file is still used.
func (f awsFiles) names() (names, warnings []string) {
	seen := map[string]bool{}
	add := func(path string, parse func(section string) (string, bool)) {
		data, err := os.ReadFile(path)
		if errors.Is(err, fs.ErrNotExist) {
			return
		}
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("Could not read %s.", path))
			return
		}
		for _, section := range iniSections(data) {
			if name, ok := parse(section); ok && !seen[name] {
				seen[name] = true
				names = append(names, name)
			}
		}
	}
	add(f.credentials, credentialsSection)
	add(f.config, configSection)
	return names, warnings
}

// credentialsSection maps a section of the credentials file to a profile name.
// That file has no "profile " prefix, and the SDK ignores sections that use one.
func credentialsSection(section string) (string, bool) {
	fields := strings.Fields(section)
	return section, len(fields) == 1
}

// configSection maps a section of the config file to a profile name. Other
// sections, such as sso-session and services, are not profiles.
func configSection(section string) (string, bool) {
	fields := strings.Fields(section)
	switch {
	case len(fields) == 1 && fields[0] == "default":
		return "default", true
	case len(fields) == 2 && fields[0] == "profile":
		return fields[1], true
	}
	return "", false
}

// iniSections returns the section headers of an INI file in order. It reads
// only the headers, so no value from the file is kept in memory.
func iniSections(data []byte) []string {
	var sections []string
	scanner := bufio.NewScanner(bytes.NewReader(bytes.TrimPrefix(data, []byte("\xef\xbb\xbf"))))
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "[") {
			continue
		}
		if end := strings.IndexByte(line, ']'); end > 1 {
			if s := strings.TrimSpace(line[1:end]); s != "" {
				sections = append(sections, s)
			}
		}
	}
	return sections
}

// processCommand returns the profile's credential_process command as written.
// The SDK's parser removes a pair of double quotes that surrounds a whole value,
// which breaks `"C:\Program Files\x.exe"` and `"C:\a b\x.exe" --name "c d"`. The
// raw value is used only if removing its outer quotes gives the SDK's value, so
// the SDK still decides which file and section apply.
func (f awsFiles) processCommand(name, parsed string) string {
	for _, file := range []struct {
		path  string
		parse func(string) (string, bool)
	}{{f.credentials, credentialsSection}, {f.config, configSection}} {
		data, err := os.ReadFile(file.path)
		if err != nil {
			continue
		}
		raw := rawProperty(data, name, "credential_process", file.parse)
		if len(raw) >= 2 && raw[0] == '"' && raw[len(raw)-1] == '"' && raw[1:len(raw)-1] == parsed {
			return raw
		}
	}
	return parsed
}

// rawProperty finds the last value of key in the named profile, removing
// comments the way the SDK does but keeping quotes.
func rawProperty(data []byte, profile, key string, parse func(string) (string, bool)) string {
	var value string
	inProfile := false
	scanner := bufio.NewScanner(bytes.NewReader(bytes.TrimPrefix(data, []byte("\xef\xbb\xbf"))))
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "[") {
			end := strings.IndexByte(trimmed, ']')
			if end > 1 {
				name, ok := parse(strings.TrimSpace(trimmed[1:end]))
				inProfile = ok && name == profile
			}
			continue
		}
		if !inProfile || line == "" || line[0] == ' ' || line[0] == '\t' {
			continue
		}
		for _, marker := range []string{" #", " ;", "\t#", "\t;"} {
			line, _, _ = strings.Cut(line, marker)
		}
		sep := strings.IndexAny(line, "=:")
		if sep > 0 && strings.EqualFold(strings.TrimSpace(line[:sep]), key) {
			value = strings.TrimSpace(line[sep+1:])
		}
	}
	return value
}

// load reads one profile from the files.
func (f awsFiles) load(ctx context.Context, name string) (config.SharedConfig, error) {
	sc, err := config.LoadSharedConfigProfile(ctx, name, func(o *config.LoadSharedConfigOptions) {
		o.ConfigFiles = []string{f.config}
		o.CredentialsFiles = []string{f.credentials}
	})
	if err != nil {
		// The SDK's text is dropped because a parse error can quote the file.
		return config.SharedConfig{}, ErrProfileInvalid
	}
	return sc, nil
}

// classify follows the order in which the SDK chooses a credential source.
func classify(sc config.SharedConfig) Kind {
	switch {
	case sc.Source != nil:
		return KindRole
	case sc.Credentials.HasKeys():
		if sc.Credentials.SessionToken != "" {
			return KindSession
		}
		return KindStatic
	case sc.CredentialSource != "", sc.WebIdentityTokenFile != "",
		sc.SSOSessionName != "", sc.SSOAccountID != "", sc.SSORoleName != "":
		return KindOther
	case sc.LoginSession != "":
		if sc.RoleARN != "" {
			return KindRole
		}
		return KindLogin
	case sc.RoleARN != "":
		return KindOther
	case sc.CredentialProcess != "":
		return KindProcess
	}
	return KindNone
}

// roleChainSupported follows source_profile links to the profile that holds the
// first credentials. A role is supported over a login, static or session
// profile. A credential_process root is refused: the SDK would run that helper
// itself, which opens a console window from a GUI app (see docs/progress.md).
func roleChainSupported(sc config.SharedConfig) bool {
	for range 8 {
		if sc.Source != nil {
			sc = *sc.Source
			continue
		}
		switch classify(sc) {
		case KindStatic, KindSession, KindLogin:
			return true
		case KindRole:
			return sc.LoginSession != ""
		}
		return false
	}
	return false
}

// loginRole is a role profile whose source_profile is an `aws login` profile.
// The SDK's loader rejects it ("source profile has no credentials", up to
// config v1.33.21, because its hasCredentials ignores login_session), so the
// app builds that one hop itself.
type loginRole struct {
	arn, source, sessionName, externalID string
	duration                             time.Duration
	region                               string
}

// rawValue returns the last value of key in the named profile of either file,
// the config file first, without the SDK's parser.
func (f awsFiles) rawValue(name, key string) string {
	for _, file := range []struct {
		path  string
		parse func(string) (string, bool)
	}{{f.config, configSection}, {f.credentials, credentialsSection}} {
		data, err := os.ReadFile(file.path)
		if err != nil {
			continue
		}
		if v := rawProperty(data, name, key, file.parse); v != "" {
			return v
		}
	}
	return ""
}

// loginRole recognises a role profile that sources an aws login profile. A role
// that needs MFA is refused, because the app cannot answer the prompt.
func (f awsFiles) loginRole(ctx context.Context, name string) (loginRole, bool) {
	lr := loginRole{
		arn:         f.rawValue(name, "role_arn"),
		source:      f.rawValue(name, "source_profile"),
		sessionName: f.rawValue(name, "role_session_name"),
		externalID:  f.rawValue(name, "external_id"),
		region:      f.rawValue(name, "region"),
	}
	if lr.arn == "" || lr.source == "" || f.rawValue(name, "mfa_serial") != "" {
		return loginRole{}, false
	}
	src, err := f.load(ctx, lr.source)
	if err != nil || src.LoginSession == "" || src.RoleARN != "" {
		return loginRole{}, false
	}
	if lr.region == "" {
		lr.region = src.Region
	}
	if secs, err := strconv.Atoi(f.rawValue(name, "duration_seconds")); err == nil && secs > 0 {
		lr.duration = time.Duration(secs) * time.Second
	}
	if lr.sessionName == "" {
		lr.sessionName = "s4browser"
	}
	return lr, true
}

// retrieveLoginRole signs in with the source profile's aws login session, then
// assumes the role with it.
func (f awsFiles) retrieveLoginRole(ctx context.Context, name string, lr loginRole) (aws.Credentials, error) {
	region := lr.region
	if region == "" {
		region = "us-east-1"
	}
	cfg, err := config.LoadDefaultConfig(ctx,
		config.WithSharedConfigProfile(lr.source),
		config.WithSharedConfigFiles([]string{f.config}),
		config.WithSharedCredentialsFiles([]string{f.credentials}),
		config.WithRegion(region))
	if err != nil || cfg.Credentials == nil {
		return aws.Credentials{}, ErrProfileInvalid
	}
	// Check the sign-in first, so a failed AssumeRole is not blamed on it.
	if _, err := cfg.Credentials.Retrieve(ctx); err != nil {
		if loginExpired(err) {
			return aws.Credentials{}, loginExpiredError(lr.source)
		}
		return aws.Credentials{}, err
	}
	provider := stscreds.NewAssumeRoleProvider(sts.NewFromConfig(cfg), lr.arn, func(o *stscreds.AssumeRoleOptions) {
		o.RoleSessionName = lr.sessionName
		if lr.externalID != "" {
			o.ExternalID = aws.String(lr.externalID)
		}
		if lr.duration > 0 {
			o.Duration = lr.duration
		}
	})
	return provider.Retrieve(ctx)
}

// loginExpiredError names the profile the user must sign in again.
func loginExpiredError(profile string) error {
	return fmt.Errorf("%w for profile %q: run `aws login --profile %s`, then choose Check",
		ErrLoginExpired, profile, profile)
}

// usesLogin reports whether a profile or any source it chains to is an
// `aws login` session.
func usesLogin(sc config.SharedConfig) bool {
	for range 8 {
		if sc.LoginSession != "" {
			return true
		}
		if sc.Source == nil {
			return false
		}
		sc = *sc.Source
	}
	return false
}

// loginExpired reports whether err comes from an aws login session that cannot
// supply credentials. The SDK gives these errors no type, so the fixed phrases
// of its login provider are matched. The match is deliberately narrow so a
// failed AssumeRole is not blamed on the sign-in.
func loginExpired(err error) bool {
	text := strings.ToLower(err.Error())
	for _, phrase := range []string{
		"load login token", "validate login token", "please reauthenticate",
		"create oauth2 token", "token file not found",
	} {
		if strings.Contains(text, phrase) {
			return true
		}
	}
	return false
}

// describe builds the Profile for an AWS profile name.
func (f awsFiles) describe(ctx context.Context, name string) Profile {
	p := Profile{ID: AWSID(name), Name: name, Source: SourceAWS}
	sc, err := f.load(ctx, name)
	if err != nil {
		if lr, ok := f.loginRole(ctx, name); ok {
			p.Kind, p.Region, p.Supported = KindRole, lr.region, true
			return p
		}
		p.Kind, p.Problem = KindNone, "The AWS config files could not be read for this profile."
		return p
	}
	p.Kind, p.Region = classify(sc), sc.Region
	switch p.Kind {
	case KindStatic, KindSession, KindProcess, KindLogin:
		p.Supported = true
	case KindRole:
		if p.Supported = roleChainSupported(sc); !p.Supported {
			p.Problem = "This role must use an aws login, static key or session profile as its source_profile."
		}
	case KindOther:
		p.Problem = "SSO, web identity and credential_source profiles are not supported and may not work."
	case KindNone:
		p.Problem = "This profile has no credentials."
	}
	return p
}

// retrieve fetches credentials for an AWS profile. It reads the files on every
// call, so keys written by a new sign-in are picked up without a restart.
func (f awsFiles) retrieve(ctx context.Context, name string) (aws.Credentials, error) {
	sc, err := f.load(ctx, name)
	if err != nil {
		if lr, ok := f.loginRole(ctx, name); ok {
			return f.retrieveLoginRole(ctx, name, lr)
		}
		return aws.Credentials{}, err
	}
	switch classify(sc) {
	case KindStatic, KindSession:
		return sc.Credentials, nil
	case KindProcess:
		return NewProcessProvider(f.processCommand(name, sc.CredentialProcess)).Retrieve(ctx)
	case KindLogin, KindRole, KindOther:
		// STS and the sign-in service need a region. The profile's own region
		// wins, and the login token's sign-in region overrides it inside the SDK.
		region := sc.Region
		if region == "" {
			region = "us-east-1"
		}
		cfg, err := config.LoadDefaultConfig(ctx,
			config.WithSharedConfigProfile(name),
			config.WithSharedConfigFiles([]string{f.config}),
			config.WithSharedCredentialsFiles([]string{f.credentials}),
			config.WithRegion(region))
		if err != nil {
			return aws.Credentials{}, ErrProfileInvalid
		}
		if cfg.Credentials == nil {
			return aws.Credentials{}, ErrNoCredentials
		}
		creds, err := cfg.Credentials.Retrieve(ctx)
		if err != nil && usesLogin(sc) && loginExpired(err) {
			// The SDK's text is dropped. It is not secret, but the app's own
			// message says what to do.
			return aws.Credentials{}, loginExpiredError(name)
		}
		return creds, err
	}
	return aws.Credentials{}, ErrNoCredentials
}
