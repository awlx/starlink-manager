package config

import (
	"strings"
	"testing"
	"time"
)

func baseEnv() map[string]string {
	return map[string]string{
		"STARLINK_SSO_COOKIE":   "test-sso-cookie-value",
		"SESSION_COOKIE_SECRET": strings.Repeat("x", 32),
		"OIDC_ISSUER_URL":       "https://keycloak.example/realms/ffmuc",
		"OIDC_CLIENT_ID":        "starlink-manager",
		"OIDC_CLIENT_SECRET":    "test-secret",
		"OIDC_REDIRECT_URL":     "https://starlink.ffmuc.net/callback",
		"OIDC_ADMIN_GROUPS":     "ffmuc-admin,orga",
	}
}

func loader(env map[string]string) func(string) string {
	return func(key string) string { return env[key] }
}

func TestLoadDefaults(t *testing.T) {
	cfg, err := load(loader(baseEnv()))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.ListenAddr != ":8080" {
		t.Errorf("ListenAddr = %q, want :8080", cfg.ListenAddr)
	}
	if cfg.StarlinkAPIBaseURL != "https://api.starlink.com" {
		t.Errorf("StarlinkAPIBaseURL = %q", cfg.StarlinkAPIBaseURL)
	}
	if cfg.StarlinkWebBaseURL != "https://starlink.com" {
		t.Errorf("StarlinkWebBaseURL = %q", cfg.StarlinkWebBaseURL)
	}
	if cfg.PollInterval != 30*time.Second {
		t.Errorf("PollInterval = %v", cfg.PollInterval)
	}
	if cfg.HistoryWindow != 24*time.Hour {
		t.Errorf("HistoryWindow = %v", cfg.HistoryWindow)
	}
	if cfg.CallTimeout != 12*time.Second {
		t.Errorf("CallTimeout = %v", cfg.CallTimeout)
	}
	if cfg.DataDir != "data" {
		t.Errorf("DataDir = %q", cfg.DataDir)
	}
	if cfg.AuthMode != AuthModeOIDC {
		t.Errorf("AuthMode = %q, want oidc", cfg.AuthMode)
	}
	if cfg.OIDCAdminClaim != "groups" {
		t.Errorf("OIDCAdminClaim = %q", cfg.OIDCAdminClaim)
	}
	if !cfg.SessionCookieSecure {
		t.Errorf("SessionCookieSecure should default to true for HTTPS redirect URL")
	}
}

func TestLoadMinimalEnv(t *testing.T) {
	env := map[string]string{
		"STARLINK_SSO_COOKIE":   "c",
		"SESSION_COOKIE_SECRET": strings.Repeat("s", 32),
		"OIDC_ISSUER_URL":       "http://localhost:8081/realms/test",
		"OIDC_CLIENT_ID":        "id",
		"OIDC_CLIENT_SECRET":    "secret",
		"OIDC_REDIRECT_URL":     "http://localhost:8080/callback",
		"OIDC_ADMIN_GROUPS":     "admins",
	}
	cfg, err := load(loader(env))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.SessionCookieSecure {
		t.Errorf("http redirect URL should not enable secure cookies by default")
	}
}

func TestLoadMissingRequired(t *testing.T) {
	cases := []struct {
		name string
		drop string
		want string
	}{
		{"sso cookie", "STARLINK_SSO_COOKIE", "STARLINK_SSO_COOKIE"},
		{"issuer", "OIDC_ISSUER_URL", "OIDC_ISSUER_URL"},
		{"client id", "OIDC_CLIENT_ID", "OIDC_CLIENT_ID"},
		{"client secret", "OIDC_CLIENT_SECRET", "OIDC_CLIENT_SECRET"},
		{"redirect", "OIDC_REDIRECT_URL", "OIDC_REDIRECT_URL"},
		{"admin groups", "OIDC_ADMIN_GROUPS", "OIDC_ADMIN_GROUPS"},
		{"session secret", "SESSION_COOKIE_SECRET", "SESSION_COOKIE_SECRET"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := baseEnv()
			delete(env, tc.drop)
			_, err := load(loader(env))
			if err == nil {
				t.Fatalf("expected error about %s", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q should mention %s", err, tc.want)
			}
		})
	}
}

func TestLoadInvalidValues(t *testing.T) {
	cases := []struct {
		name, key, value, want string
	}{
		{"bad poll interval", "STARLINK_POLL_INTERVAL", "banana", "STARLINK_POLL_INTERVAL"},
		{"too short poll", "STARLINK_POLL_INTERVAL", "1s", "at least 2s"},
		{"bad history window", "STARLINK_HISTORY_WINDOW", "-5m", "STARLINK_HISTORY_WINDOW"},
		{"bad call timeout", "STARLINK_CALL_TIMEOUT", "500ms", "STARLINK_CALL_TIMEOUT"},
		{"bad auth mode", "AUTH_MODE", "oauth1", "AUTH_MODE"},
		{"short cookie secret", "SESSION_COOKIE_SECRET", "short", "at least 32"},
		{"bad base url", "STARLINK_API_BASE_URL", "ftp://x", "STARLINK_API_BASE_URL"},
		{"bad localdev flag", "LOCAL_DEV_USER", "maybe", "LOCAL_DEV_USER"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := baseEnv()
			env[tc.key] = tc.value
			_, err := load(loader(env))
			if err == nil {
				t.Fatalf("expected error for %s=%q", tc.key, tc.value)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q should contain %q", err, tc.want)
			}
		})
	}
}

func TestLoadForwardAuthMode(t *testing.T) {
	env := map[string]string{
		"STARLINK_SSO_COOKIE":   "c",
		"SESSION_COOKIE_SECRET": strings.Repeat("s", 32),
		"AUTH_MODE":             "forwardauth",
		"OIDC_ADMIN_GROUPS":     "admin",
	}
	cfg, err := load(loader(env))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.AuthMode != AuthModeForwardAuth {
		t.Fatalf("AuthMode = %q", cfg.AuthMode)
	}
	if cfg.ForwardAuthHeaders.User != "X-Forwarded-User" {
		t.Errorf("ForwardAuthHeaders.User = %q", cfg.ForwardAuthHeaders.User)
	}
	if cfg.ForwardAuthHeaders.Groups != "X-Forwarded-Groups" {
		t.Errorf("ForwardAuthHeaders.Groups = %q", cfg.ForwardAuthHeaders.Groups)
	}
	if cfg.OIDCAdminClaim != "groups" {
		t.Errorf("OIDCAdminClaim = %q", cfg.OIDCAdminClaim)
	}
}

func TestLoadLocalDevMode(t *testing.T) {
	env := map[string]string{"STARLINK_SSO_COOKIE": "c", "LOCAL_DEV_USER": "true"}
	cfg, err := load(loader(env))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.AuthMode != AuthModeLocalDev || !cfg.LocalDevUser {
		t.Fatalf("AuthMode = %q, LocalDevUser = %v", cfg.AuthMode, cfg.LocalDevUser)
	}
	if len(cfg.OIDCAdminGroups) != 0 {
		t.Errorf("OIDCAdminGroups = %v, want empty", cfg.OIDCAdminGroups)
	}
	if len(cfg.SessionCookieSecret) != 32 {
		t.Errorf("expected ephemeral 32-byte cookie secret")
	}
}

func TestLoadLocalDevConflict(t *testing.T) {
	env := baseEnv()
	env["LOCAL_DEV_USER"] = "true"
	env["AUTH_MODE"] = "oidc"
	_, err := load(loader(env))
	if err == nil || !strings.Contains(err.Error(), "conflicts") {
		t.Fatalf("expected conflict error, got %v", err)
	}
}

func TestSplitList(t *testing.T) {
	got := splitList(" a , b ,, c ")
	want := []string{"a", "b", "c"}
	if len(got) != len(want) {
		t.Fatalf("splitList = %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("splitList[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestLoadLocalDevRequiresOptIn(t *testing.T) {
	env := map[string]string{"STARLINK_SSO_COOKIE": "c", "AUTH_MODE": "localdev"}
	if _, err := load(loader(env)); err == nil || !strings.Contains(err.Error(), "LOCAL_DEV_USER=true") {
		t.Fatalf("AUTH_MODE=localdev without LOCAL_DEV_USER: err = %v, want opt-in error", err)
	}
	env["AUTH_MODE"] = "LocalDev"
	env["LOCAL_DEV_USER"] = "true"
	cfg, err := load(loader(env))
	if err != nil || cfg.AuthMode != AuthModeLocalDev {
		t.Fatalf("mixed-case AUTH_MODE with LOCAL_DEV_USER: cfg %q err %v", cfg.AuthMode, err)
	}
}

func TestLoadPollConcurrency(t *testing.T) {
	for _, tc := range []struct {
		raw  string
		want int
	}{{"", 4}, {"8", 8}, {"0", 1}, {"-3", 1}, {"64", 16}, {" 16 ", 16}} {
		env := baseEnv()
		env["STARLINK_POLL_CONCURRENCY"] = tc.raw
		cfg, err := load(loader(env))
		if err != nil {
			t.Fatalf("%q: %v", tc.raw, err)
		}
		if cfg.PollConcurrency != tc.want {
			t.Errorf("%q: PollConcurrency = %d, want %d", tc.raw, cfg.PollConcurrency, tc.want)
		}
	}
	env := baseEnv()
	env["STARLINK_POLL_CONCURRENCY"] = "four"
	if _, err := load(loader(env)); err == nil {
		t.Error("non-integer STARLINK_POLL_CONCURRENCY accepted")
	}
}
