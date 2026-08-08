// Package config resolves credentials and defaults.
//
// Precedence, highest first: explicit flags, then environment variables, then
// the selected profile in the config file, then the file's [default] profile.
// Env-only auth works fine for one ad account; profiles exist because most
// people end up juggling several.
package config

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Config is the resolved settings for one invocation.
type Config struct {
	AccessToken string
	AppSecret   string
	AccountID   string
	APIVersion  string
	Profile     string
}

// Profile is one named section of the config file.
type Profile struct {
	Name        string
	AccessToken string
	AppSecret   string
	AccountID   string
	APIVersion  string
}

// File is a parsed config file.
type File struct {
	Path     string
	Profiles map[string]Profile
}

const (
	envToken      = "META_ACCESS_TOKEN"
	envSecret     = "META_APP_SECRET"
	envAccount    = "META_ACCOUNT_ID"
	envAPIVersion = "META_API_VERSION"
	envProfile    = "META_PROFILE"
	envConfigPath = "META_ADS_CONFIG"

	defaultProfile = "default"
)

// Path returns the config file location, honouring META_ADS_CONFIG and
// XDG_CONFIG_HOME.
func Path() string {
	if p := os.Getenv(envConfigPath); p != "" {
		return p
	}
	base := os.Getenv("XDG_CONFIG_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "meta-ads.toml"
		}
		base = filepath.Join(home, ".config")
	}
	return filepath.Join(base, "meta-ads", "config.toml")
}

// Load reads and parses the config file. A missing file is not an error --
// environment-only usage is fully supported.
func Load() (*File, error) {
	path := Path()
	f := &File{Path: path, Profiles: map[string]Profile{}}

	fh, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return f, nil
		}
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	defer fh.Close()

	current := defaultProfile
	scanner := bufio.NewScanner(fh)
	line := 0
	for scanner.Scan() {
		line++
		text := strings.TrimSpace(scanner.Text())
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}
		if strings.HasPrefix(text, "[") && strings.HasSuffix(text, "]") {
			current = strings.TrimSpace(text[1 : len(text)-1])
			if current == "" {
				return nil, fmt.Errorf("%s:%d: empty profile name", path, line)
			}
			continue
		}
		key, value, ok := strings.Cut(text, "=")
		if !ok {
			return nil, fmt.Errorf("%s:%d: expected key = value", path, line)
		}
		key = strings.TrimSpace(key)
		value = unquote(strings.TrimSpace(value))

		p := f.Profiles[current]
		p.Name = current
		switch key {
		case "access_token":
			p.AccessToken = value
		case "app_secret":
			p.AppSecret = value
		case "account_id":
			p.AccountID = value
		case "api_version":
			p.APIVersion = value
		default:
			return nil, fmt.Errorf("%s:%d: unknown key %q", path, line, key)
		}
		f.Profiles[current] = p
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	return f, nil
}

func unquote(s string) string {
	if len(s) >= 2 {
		if (s[0] == '"' && s[len(s)-1] == '"') || (s[0] == '\'' && s[len(s)-1] == '\'') {
			return s[1 : len(s)-1]
		}
	}
	return s
}

// Names lists the profiles defined in the file, sorted.
func (f *File) Names() []string {
	out := make([]string, 0, len(f.Profiles))
	for name := range f.Profiles {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// Overrides are the values supplied on the command line.
type Overrides struct {
	Profile     string
	AccessToken string
	AppSecret   string
	AccountID   string
	APIVersion  string
}

// Resolve merges flags, environment and file into the settings to use.
// defaultAPIVersion is the version the embedded tree was generated against; it
// applies only when nothing else specifies one.
func Resolve(o Overrides, defaultAPIVersion string) (Config, error) {
	file, err := Load()
	if err != nil {
		return Config{}, err
	}

	name := firstNonEmpty(o.Profile, os.Getenv(envProfile), defaultProfile)
	p, ok := file.Profiles[name]
	if !ok && o.Profile != "" {
		known := file.Names()
		if len(known) == 0 {
			return Config{}, fmt.Errorf("profile %q requested but %s defines none", name, file.Path)
		}
		return Config{}, fmt.Errorf("profile %q not found in %s (have: %s)",
			name, file.Path, strings.Join(known, ", "))
	}

	cfg := Config{
		Profile:     name,
		AccessToken: firstNonEmpty(o.AccessToken, os.Getenv(envToken), p.AccessToken),
		AppSecret:   firstNonEmpty(o.AppSecret, os.Getenv(envSecret), p.AppSecret),
		AccountID:   firstNonEmpty(o.AccountID, os.Getenv(envAccount), p.AccountID),
		APIVersion:  firstNonEmpty(o.APIVersion, os.Getenv(envAPIVersion), p.APIVersion, defaultAPIVersion),
	}
	if cfg.APIVersion != "" && !strings.HasPrefix(cfg.APIVersion, "v") {
		cfg.APIVersion = "v" + cfg.APIVersion
	}
	return cfg, nil
}

// RequireToken returns an actionable error when no access token was found.
func (c Config) RequireToken() error {
	if c.AccessToken != "" {
		return nil
	}
	return fmt.Errorf("no access token: set %s, or add access_token to a profile in %s",
		envToken, Path())
}

// NormalizeAccountID accepts either "123" or "act_123" and returns the act_
// form the Graph API expects.
func NormalizeAccountID(id string) string {
	if id == "" || strings.HasPrefix(id, "act_") {
		return id
	}
	return "act_" + id
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
