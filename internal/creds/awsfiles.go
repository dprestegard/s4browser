package creds

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
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
		return KindOther
	case sc.Credentials.HasKeys():
		if sc.Credentials.SessionToken != "" {
			return KindSession
		}
		return KindStatic
	case sc.CredentialSource != "", sc.WebIdentityTokenFile != "", sc.RoleARN != "",
		sc.SSOSessionName != "", sc.SSOAccountID != "", sc.SSORoleName != "", sc.LoginSession != "":
		return KindOther
	case sc.CredentialProcess != "":
		return KindProcess
	}
	return KindNone
}

// describe builds the Profile for an AWS profile name.
func (f awsFiles) describe(ctx context.Context, name string) Profile {
	p := Profile{ID: AWSID(name), Name: name, Source: SourceAWS}
	sc, err := f.load(ctx, name)
	if err != nil {
		p.Kind, p.Problem = KindNone, "The AWS config files could not be read for this profile."
		return p
	}
	p.Kind, p.Region = classify(sc), sc.Region
	switch p.Kind {
	case KindStatic, KindSession, KindProcess:
		p.Supported = true
	case KindOther:
		p.Problem = "Role, SSO and web identity profiles are not supported and may not work."
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
		return aws.Credentials{}, err
	}
	switch classify(sc) {
	case KindStatic, KindSession:
		return sc.Credentials, nil
	case KindProcess:
		return NewProcessProvider(f.processCommand(name, sc.CredentialProcess)).Retrieve(ctx)
	case KindOther:
		cfg, err := config.LoadDefaultConfig(ctx,
			config.WithSharedConfigProfile(name),
			config.WithSharedConfigFiles([]string{f.config}),
			config.WithSharedCredentialsFiles([]string{f.credentials}))
		if err != nil {
			return aws.Credentials{}, ErrProfileInvalid
		}
		if cfg.Credentials == nil {
			return aws.Credentials{}, ErrNoCredentials
		}
		return cfg.Credentials.Retrieve(ctx)
	}
	return aws.Credentials{}, ErrNoCredentials
}
