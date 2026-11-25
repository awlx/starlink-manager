// Package config loads and validates all application configuration from
// environment variables. Invalid configuration causes a startup error with a
// clear message instead of a misbehaving process.
package config

import (
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
)

// AuthMode selects how incoming HTTP requests are authenticated.
type AuthMode string

const (
	// AuthModeOIDC authenticates users against a Keycloak (or other OIDC)
	// provider via the authorization-code flow. This is the default.
	AuthModeOIDC AuthMode = "oidc"
	// AuthModeForwardAuth trusts X-Forwarded-* headers injected by a
	// reverse-proxy forward-auth service (e.g. Traefik + noc-auth). The app
	// must not be reachable without the proxy.
	AuthModeForwardAuth AuthMode = "forwardauth"
	// AuthModeLocalDev treats every request as an authenticated admin. Only
	// meant for local development.
	AuthModeLocalDev AuthMode = "localdev"
)

// Config is the fully parsed application configuration.
type Config struct {
	ListenAddr string

	// Starlink account API (the API the Starlink app/site uses).
	StarlinkAPIBaseURL     string // e.g. https://api.starlink.com
	StarlinkWebBaseURL     string // e.g. https://starlink.com
	StarlinkSSOCookie      string // durable Starlink.Com.Sso cookie value
	StarlinkSessionToken   string // optional pre-minted access token override
	StarlinkRefreshCookies string // optional seed cookie header (Access trio) for token renewal
	StarlinkAccountNumber  string // optional account number (used when the session probe never runs)

	PollInterval       time.Duration // roster/telemetry/status polling cadence
	HistoryWindow      time.Duration // length of persisted per-device history
	CallTimeout        time.Duration // per device-call timeout
	DataDir            string        // persisted history directory
	RosterCacheTTL     time.Duration
	TelemetryTTLOnline time.Duration // telemetry age before "offline" in UI

	AuthMode           AuthMode
	OIDCIssuerURL      string
	OIDCClientID       string
	OIDCClientSecret   string
	OIDCRedirectURL    string
	OIDCAdminGroups    []string
	OIDCAdminClaim     string
	ForwardAuthHeaders ForwardAuthHeaders
	LocalDevUser       bool

	SessionCookieSecret []byte
	SessionCookieSecure bool // set when serving over HTTPS
}

// ForwardAuthHeaders names the headers a forward-auth proxy injects.
type ForwardAuthHeaders struct {
	User   string
	Email  string
	Groups string
}

// getenv is the lookup function used by Load; overridable in tests.
var getenv = os.Getenv

// Load parses the environment into a Config. It returns an error for any
// invalid or missing required value; callers should exit non-zero.
func Load() (Config, error) {
	return load(getenv)
}

func load(getenv func(string) string) (Config, error) {
	cfg := Config{}

	cfg.ListenAddr = strings.TrimSpace(getenv("LISTEN_ADDR"))
	if cfg.ListenAddr == "" {
		cfg.ListenAddr = ":8080"
	}

	cfg.StarlinkAPIBaseURL = strings.TrimSpace(getenv("STARLINK_API_BASE_URL"))
	if cfg.StarlinkAPIBaseURL == "" {
		cfg.StarlinkAPIBaseURL = "https://api.starlink.com"
	}
	if err := validBaseURL(cfg.StarlinkAPIBaseURL); err != nil {
		return cfg, fmt.Errorf("STARLINK_API_BASE_URL: %w", err)
	}
	cfg.StarlinkAPIBaseURL = strings.TrimRight(cfg.StarlinkAPIBaseURL, "/")

	cfg.StarlinkWebBaseURL = strings.TrimSpace(getenv("STARLINK_WEB_BASE_URL"))
	if cfg.StarlinkWebBaseURL == "" {
		cfg.StarlinkWebBaseURL = "https://starlink.com"
	}
	if err := validBaseURL(cfg.StarlinkWebBaseURL); err != nil {
		return cfg, fmt.Errorf("STARLINK_WEB_BASE_URL: %w", err)
	}
	cfg.StarlinkWebBaseURL = strings.TrimRight(cfg.StarlinkWebBaseURL, "/")

	cfg.StarlinkSSOCookie = strings.TrimSpace(getenv("STARLINK_SSO_COOKIE"))
	cfg.StarlinkSessionToken = strings.TrimSpace(getenv("STARLINK_SESSION_TOKEN"))
	cfg.StarlinkRefreshCookies = strings.TrimSpace(getenv("STARLINK_REFRESH_COOKIES"))
	cfg.StarlinkAccountNumber = strings.TrimSpace(getenv("STARLINK_ACCOUNT_NUMBER"))
	if cfg.StarlinkSSOCookie == "" && cfg.StarlinkSessionToken == "" && cfg.StarlinkRefreshCookies == "" {
		return cfg, errors.New("STARLINK_SSO_COOKIE is required: set it to the Starlink.Com.Sso cookie value from a browser login at starlink.com (or provide STARLINK_SESSION_TOKEN to use a pre-minted access token, or STARLINK_REFRESH_COOKIES to seed the session cookies)")
	}

	var err error
	cfg.PollInterval, err = duration(getenv("STARLINK_POLL_INTERVAL"), 30*time.Second)
	if err != nil {
		return cfg, fmt.Errorf("STARLINK_POLL_INTERVAL: %w", err)
	}
	if cfg.PollInterval < 2*time.Second {
		return cfg, errors.New("STARLINK_POLL_INTERVAL must be at least 2s (Starlink refreshes device telemetry about every 2 minutes; shorter intervals add load without fresher data)")
	}

	cfg.HistoryWindow, err = duration(getenv("STARLINK_HISTORY_WINDOW"), 24*time.Hour)
	if err != nil {
		return cfg, fmt.Errorf("STARLINK_HISTORY_WINDOW: %w", err)
	}

	cfg.CallTimeout, err = duration(getenv("STARLINK_CALL_TIMEOUT"), 12*time.Second)
	if err != nil {
		return cfg, fmt.Errorf("STARLINK_CALL_TIMEOUT: %w", err)
	}
	if cfg.CallTimeout < time.Second {
		return cfg, errors.New("STARLINK_CALL_TIMEOUT must be at least 1s")
	}

	cfg.DataDir = strings.TrimSpace(getenv("STARLINK_DATA_DIR"))
	if cfg.DataDir == "" {
		cfg.DataDir = "data"
	}

	cfg.RosterCacheTTL, err = duration(getenv("STARLINK_ROSTER_CACHE_TTL"), 5*time.Minute)
	if err != nil {
		return cfg, fmt.Errorf("STARLINK_ROSTER_CACHE_TTL: %w", err)
	}
	cfg.TelemetryTTLOnline, err = duration(getenv("STARLINK_TELEMETRY_ONLINE_TTL"), 5*time.Minute)
	if err != nil {
		return cfg, fmt.Errorf("STARLINK_TELEMETRY_ONLINE_TTL: %w", err)
	}

	// Auth mode.
	mode := strings.ToLower(strings.TrimSpace(getenv("AUTH_MODE")))
	localDev := strings.TrimSpace(getenv("LOCAL_DEV_USER"))
	switch strings.ToLower(localDev) {
	case "", "false", "0", "no":
		localDev = ""
	case "true", "1", "yes":
		localDev = "true"
	default:
		return cfg, fmt.Errorf("LOCAL_DEV_USER must be true or false, got %q", localDev)
	}
	if localDev == "true" && mode != "" && mode != "localdev" {
		return cfg, fmt.Errorf("LOCAL_DEV_USER=true conflicts with AUTH_MODE=%q (LOCAL_DEV_USER implies AUTH_MODE=localdev)", mode)
	}
	if localDev == "true" {
		mode = "localdev"
	} else if mode == "localdev" {
		// localdev makes every request an admin; require the explicit opt-in
		// so a stray AUTH_MODE alone cannot open the app.
		return cfg, errors.New("AUTH_MODE=localdev requires LOCAL_DEV_USER=true")
	}
	if mode == "" {
		mode = string(AuthModeOIDC)
	}
	switch AuthMode(mode) {
	case AuthModeOIDC, AuthModeForwardAuth, AuthModeLocalDev:
		cfg.AuthMode = AuthMode(mode)
	default:
		return cfg, fmt.Errorf("AUTH_MODE must be one of oidc, forwardauth, localdev (got %q)", mode)
	}
	cfg.LocalDevUser = cfg.AuthMode == AuthModeLocalDev

	// OIDC settings: required for oidc mode, ignored otherwise.
	if cfg.AuthMode == AuthModeOIDC {
		cfg.OIDCIssuerURL = strings.TrimSpace(getenv("OIDC_ISSUER_URL"))
		cfg.OIDCClientID = strings.TrimSpace(getenv("OIDC_CLIENT_ID"))
		cfg.OIDCClientSecret = strings.TrimSpace(getenv("OIDC_CLIENT_SECRET"))
		cfg.OIDCRedirectURL = strings.TrimSpace(getenv("OIDC_REDIRECT_URL"))
		for name, val := range map[string]string{
			"OIDC_ISSUER_URL":    cfg.OIDCIssuerURL,
			"OIDC_CLIENT_ID":     cfg.OIDCClientID,
			"OIDC_CLIENT_SECRET": cfg.OIDCClientSecret,
			"OIDC_REDIRECT_URL":  cfg.OIDCRedirectURL,
		} {
			if val == "" {
				return cfg, fmt.Errorf("%s is required when AUTH_MODE=oidc", name)
			}
		}
	}

	cfg.OIDCAdminClaim = strings.TrimSpace(getenv("OIDC_ADMIN_CLAIM"))
	if cfg.OIDCAdminClaim == "" {
		cfg.OIDCAdminClaim = "groups"
	}
	cfg.OIDCAdminGroups = splitList(getenv("OIDC_ADMIN_GROUPS"))
	if len(cfg.OIDCAdminGroups) == 0 && cfg.AuthMode != AuthModeLocalDev {
		return cfg, errors.New("OIDC_ADMIN_GROUPS is required: comma-separated group/role claim values that grant admin rights (leave out only if you want a read-only-only deployment)")
	}

	cfg.ForwardAuthHeaders = ForwardAuthHeaders{
		User:   headerDefault(getenv("FORWARD_AUTH_USER_HEADER"), "X-Forwarded-User"),
		Email:  headerDefault(getenv("FORWARD_AUTH_EMAIL_HEADER"), "X-Forwarded-Email"),
		Groups: headerDefault(getenv("FORWARD_AUTH_GROUPS_HEADER"), "X-Forwarded-Groups"),
	}
	if cfg.AuthMode == AuthModeForwardAuth {
		for name, val := range map[string]string{
			"FORWARD_AUTH_USER_HEADER":   cfg.ForwardAuthHeaders.User,
			"FORWARD_AUTH_EMAIL_HEADER":  cfg.ForwardAuthHeaders.Email,
			"FORWARD_AUTH_GROUPS_HEADER": cfg.ForwardAuthHeaders.Groups,
		} {
			if val == "" {
				return cfg, fmt.Errorf("%s must not be empty when AUTH_MODE=forwardauth", name)
			}
		}
	}

	secret := strings.TrimSpace(getenv("SESSION_COOKIE_SECRET"))
	switch {
	case secret != "":
		if len(secret) < 32 {
			return cfg, errors.New("SESSION_COOKIE_SECRET must be at least 32 characters (generate one with: openssl rand -hex 32)")
		}
		cfg.SessionCookieSecret = []byte(secret)
	case cfg.AuthMode == AuthModeLocalDev:
		// Ephemeral random secret is fine for local development.
		cfg.SessionCookieSecret = randomBytes(32)
	default:
		return cfg, errors.New("SESSION_COOKIE_SECRET is required: generate one with openssl rand -hex 32")
	}

	cfg.SessionCookieSecure = strings.EqualFold(strings.TrimSpace(getenv("SESSION_COOKIE_SECURE")), "true")
	secureExplicit := strings.TrimSpace(getenv("SESSION_COOKIE_SECURE")) != ""
	// Default to a secure cookie whenever the OIDC redirect URL is HTTPS
	// (i.e. the app itself is served over HTTPS). An explicit env value wins.
	if !secureExplicit && strings.HasPrefix(cfg.OIDCRedirectURL, "https://") {
		cfg.SessionCookieSecure = true
	}

	return cfg, nil
}

// duration parses a Go duration string with the given default when unset.
func duration(raw string, def time.Duration) (time.Duration, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return def, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("invalid duration %q (use e.g. 30s, 5m, 2h)", raw)
	}
	return d, nil
}

// splitList parses a comma-separated list, trimming whitespace and dropping
// empty entries.
func splitList(raw string) []string {
	var out []string
	for _, item := range strings.Split(raw, ",") {
		item = strings.TrimSpace(item)
		if item != "" {
			out = append(out, item)
		}
	}
	return out
}

func headerDefault(raw, def string) string {
	if strings.TrimSpace(raw) == "" {
		return def
	}
	return strings.TrimSpace(raw)
}

func validBaseURL(raw string) error {
	if !strings.HasPrefix(raw, "http://") && !strings.HasPrefix(raw, "https://") {
		return fmt.Errorf("must be an http(s) URL, got %q", raw)
	}
	return nil
}

// randomBytes returns n cryptographically random bytes.
func randomBytes(n int) []byte {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(fmt.Sprintf("reading random bytes: %v", err))
	}
	return b
}
