package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("META_ADS_CONFIG", path)
	return path
}

func clearEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{"META_ACCESS_TOKEN", "META_APP_SECRET", "META_ACCOUNT_ID", "META_API_VERSION", "META_PROFILE"} {
		t.Setenv(k, "")
	}
}

func TestMissingFileIsNotAnError(t *testing.T) {
	clearEnv(t)
	t.Setenv("META_ADS_CONFIG", filepath.Join(t.TempDir(), "absent.toml"))
	t.Setenv("META_ACCESS_TOKEN", "from-env")

	cfg, err := Resolve(Overrides{}, "v26.0")
	if err != nil {
		t.Fatalf("environment-only usage must work without a config file: %v", err)
	}
	if cfg.AccessToken != "from-env" {
		t.Errorf("token = %q", cfg.AccessToken)
	}
}

func TestPrecedence(t *testing.T) {
	clearEnv(t)
	writeConfig(t, `
access_token = "from-file"
account_id = "act_file"

[work]
access_token = "work-token"
account_id = "act_work"
`)

	// File fills in what nothing else provides.
	cfg, err := Resolve(Overrides{}, "v26.0")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.AccessToken != "from-file" || cfg.AccountID != "act_file" {
		t.Errorf("default profile = %+v", cfg)
	}

	// Environment beats the file.
	t.Setenv("META_ACCESS_TOKEN", "from-env")
	cfg, _ = Resolve(Overrides{}, "v26.0")
	if cfg.AccessToken != "from-env" {
		t.Errorf("env should beat file, got %q", cfg.AccessToken)
	}

	// A flag beats both.
	cfg, _ = Resolve(Overrides{AccessToken: "from-flag"}, "v26.0")
	if cfg.AccessToken != "from-flag" {
		t.Errorf("flag should beat env, got %q", cfg.AccessToken)
	}
}

func TestNamedProfile(t *testing.T) {
	clearEnv(t)
	writeConfig(t, `
access_token = "default-token"

[work]
access_token = "work-token"
account_id = "act_work"
api_version = "v25.0"
`)

	cfg, err := Resolve(Overrides{Profile: "work"}, "v26.0")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.AccessToken != "work-token" || cfg.AccountID != "act_work" {
		t.Errorf("work profile = %+v", cfg)
	}
	if cfg.APIVersion != "v25.0" {
		t.Errorf("profile should be able to pin an api version, got %q", cfg.APIVersion)
	}
}

func TestUnknownProfileNamesTheKnownOnes(t *testing.T) {
	clearEnv(t)
	writeConfig(t, "[work]\naccess_token = \"x\"\n")

	_, err := Resolve(Overrides{Profile: "personal"}, "v26.0")
	if err == nil {
		t.Fatal("expected an error for an unknown profile")
	}
	if !strings.Contains(err.Error(), "work") {
		t.Errorf("error should list the profiles that do exist: %v", err)
	}
}

func TestMalformedConfigIsReported(t *testing.T) {
	clearEnv(t)
	writeConfig(t, "this is not a key value pair\n")
	if _, err := Resolve(Overrides{}, "v26.0"); err == nil {
		t.Fatal("expected a parse error")
	}

	writeConfig(t, "unknown_key = \"x\"\n")
	if _, err := Resolve(Overrides{}, "v26.0"); err == nil {
		t.Fatal("a typo'd key should be reported, not silently ignored")
	}
}

func TestAPIVersionGetsVPrefix(t *testing.T) {
	clearEnv(t)
	t.Setenv("META_ADS_CONFIG", filepath.Join(t.TempDir(), "absent.toml"))
	t.Setenv("META_API_VERSION", "25.0")

	cfg, err := Resolve(Overrides{}, "v26.0")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.APIVersion != "v25.0" {
		t.Errorf("api version = %q, want v25.0", cfg.APIVersion)
	}
}

func TestNormalizeAccountID(t *testing.T) {
	cases := map[string]string{
		"123":      "act_123",
		"act_123":  "act_123",
		"":         "",
		"act_act1": "act_act1",
	}
	for in, want := range cases {
		if got := NormalizeAccountID(in); got != want {
			t.Errorf("NormalizeAccountID(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRequireTokenPointsSomewhere(t *testing.T) {
	err := Config{}.RequireToken()
	if err == nil {
		t.Fatal("expected an error with no token")
	}
	if !strings.Contains(err.Error(), "META_ACCESS_TOKEN") {
		t.Errorf("error should say how to fix it: %v", err)
	}
	if err := (Config{AccessToken: "x"}).RequireToken(); err != nil {
		t.Errorf("a token present should be fine: %v", err)
	}
}

func TestQuotesAreOptional(t *testing.T) {
	clearEnv(t)
	writeConfig(t, "access_token = bare-value\naccount_id = 'single'\n")
	cfg, err := Resolve(Overrides{}, "v26.0")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.AccessToken != "bare-value" || cfg.AccountID != "single" {
		t.Errorf("cfg = %+v", cfg)
	}
}
