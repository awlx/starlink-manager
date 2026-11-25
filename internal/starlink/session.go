package starlink

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const (
	// ssoCookieName is the durable login cookie from a browser session at
	// starlink.com. It is provided via the STARLINK_SSO_COOKIE env var.
	ssoCookieName = "Starlink.Com.Sso"
	// accessCookieName is the short-lived access token cookie minted by the
	// account API session probe.
	accessCookieName = "Starlink.Com.Access.V1"
	// probeMinInterval is the minimum spacing between session probes; the
	// account API rejects a second probe within the window with 401.
	probeMinInterval = 60 * time.Second
	// tokenFreshFor is how long a minted access token is reused before
	// renewal. Tokens live about 15 minutes.
	tokenFreshFor = 10 * time.Minute
	// browserUA makes requests look like the Starlink web app; the account
	// API answers differently to unknown agents.
	browserUA = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0.0.0 Safari/537.36"
)

// ErrSessionDead means the SSO cookie is no longer accepted: the user has to
// re-authenticate at starlink.com and provide a fresh cookie value.
var ErrSessionDead = errors.New("starlink session invalid: the SSO cookie was rejected, re-authenticate at starlink.com and update STARLINK_SSO_COOKIE")

// ErrProbeThrottled means a session refresh was skipped because a probe ran
// within the last 60 seconds.
var ErrProbeThrottled = errors.New("starlink session probe throttled: at most one probe per 60s")

// Session manages authentication for the Starlink account API: it holds the
// durable SSO cookie, mints short-lived access tokens via the session probe
// (at most once per 60 seconds, single-flight), and can renew a token via
// the refresh-token endpoint.
type Session struct {
	apiBase string // https://api.starlink.com
	webBase string // https://starlink.com
	sso     string

	jar  *cookieJar
	http *http.Client
	log  *slog.Logger

	mu           sync.Mutex
	token        string
	tokenAt      time.Time
	lastProbe    time.Time
	accountNum   string
	accountInfo  map[string]any
	sessionDead  bool
	now          func() time.Time
}

// NewSession creates a session bound to the given base URLs.
func NewSession(apiBase, webBase, ssoCookie string, client *http.Client, log *slog.Logger) *Session {
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	if log == nil {
		log = slog.Default()
	}
	s := &Session{
		apiBase: strings.TrimRight(apiBase, "/"),
		webBase: strings.TrimRight(webBase, "/"),
		sso:     ssoCookie,
		jar:     newCookieJar(),
		http:    client,
		log:     log,
		now:     time.Now,
	}
	client.Jar = s.jar
	// The SSO cookie belongs to the starlink.com domain.
	s.jar.setSSOCookie(hostOf(webBase), ssoCookie)
	return s
}

func hostOf(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return rawURL
	}
	return u.Hostname()
}

// SessionDead reports whether the SSO cookie has been rejected.
func (s *Session) SessionDead() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sessionDead
}

// AccountNumber returns the account number learned from the session probe,
// or "" before the first successful probe.
func (s *Session) AccountNumber() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.accountNum
}

// AccountInfo returns the raw account JSON from the session probe.
func (s *Session) AccountInfo() map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.accountInfo
}

// Token returns a valid access token, minting or renewing one as needed.
func (s *Session) Token(ctx context.Context) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ensureTokenLocked(ctx)
}

// Ensure makes sure a valid access token exists before API calls, minting
// one via the session probe if needed.
func (s *Session) Ensure(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.ensureTokenLocked(ctx)
	return err
}

// Refresh forces token renewal or a fresh probe (used after an
// unauthenticated gateway answer). It keeps the old token for the renewal
// endpoint and falls back to a probe when renewal is rejected.
func (s *Session) Refresh(ctx context.Context) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.sessionDead {
		return "", ErrSessionDead
	}
	if s.token != "" {
		if tok, err := s.renewTokenLocked(ctx); err == nil {
			return tok, nil
		} else if errors.Is(err, errRenewRejected) {
			s.token = ""
		} else {
			s.log.Warn("token renewal failed; falling back to session probe", "err", err)
		}
	}
	now := s.now()
	if !s.lastProbe.IsZero() && now.Sub(s.lastProbe) < probeMinInterval && s.token == "" {
		return "", ErrProbeThrottled
	}
	return s.probeLocked(ctx)
}

// ensureTokenLocked must be called with s.mu held. It serializes minting so
// concurrent callers share a single probe (single-flight), and enforces the
// 60s minimum probe interval.
func (s *Session) ensureTokenLocked(ctx context.Context) (string, error) {
	if s.sessionDead {
		return "", ErrSessionDead
	}
	now := s.now()
	if s.token != "" && now.Sub(s.tokenAt) < tokenFreshFor {
		return s.token, nil
	}
	if s.token != "" && now.Sub(s.lastProbe) >= probeMinInterval {
		// Try the cheap renewal first; fall back to a full probe on failure.
		if tok, err := s.renewTokenLocked(ctx); err == nil {
			return tok, nil
		} else if !errors.Is(err, errRenewRejected) {
			s.log.Warn("token renewal failed; falling back to session probe", "err", err)
		}
	}
	if !s.lastProbe.IsZero() && now.Sub(s.lastProbe) < probeMinInterval {
		if s.token != "" {
			// Keep using the old token rather than hammering the probe.
			return s.token, nil
		}
		return "", ErrProbeThrottled
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	return s.probeLocked(ctx)
}

// errRenewRejected marks a 401/403 from the renewal endpoint.
var errRenewRejected = errors.New("renewal rejected")

// renewTokenLocked mints a fresh token via the refresh-token endpoint. Per
// the account API the minted token must be sent both as a bearer header and
// as the access cookie, and the request must be a GET.
func (s *Session) renewTokenLocked(ctx context.Context) (string, error) {
	u := s.webBase + "/api/auth/auth/refresh-token"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", browserUA)
	req.Header.Set("Accept", "application/json")
	if s.token != "" {
		req.Header.Set("Authorization", "Bearer "+s.token)
	}
	resp, err := s.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return "", errRenewRejected
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("refresh-token returned status %d", resp.StatusCode)
	}
	var payload struct {
		AccessToken string `json:"accessToken"`
		Token       string `json:"token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return "", fmt.Errorf("refresh-token response: %w", err)
	}
	tok := payload.AccessToken
	if tok == "" {
		tok = payload.Token
	}
	if tok == "" {
		return "", errors.New("refresh-token response contained no access token")
	}
	s.applyTokenLocked(tok, s.now())
	return tok, nil
}

// applyTokenLocked stores the token and installs the access cookie so that
// gateway calls that require the cookie receive it.
func (s *Session) applyTokenLocked(tok string, at time.Time) {
	s.token = tok
	s.tokenAt = at
	// Mirror the token into the cookie jar for the starlink.com domain.
	u := &url.URL{Scheme: "https", Host: "starlink.com", Path: "/"}
	s.jar.SetCookies(u, []*http.Cookie{{
		Name:   accessCookieName,
		Value:  tok,
		Path:   "/",
		Domain: "starlink.com",
		Secure: true,
	}})
}

// probeLocked performs the session probe: GET /auth-rp/auth/user with the
// SSO cookie. The response contains the account JSON and sets the short-
// lived access token cookie.
func (s *Session) probeLocked(ctx context.Context) (string, error) {
	s.lastProbe = s.now()
	u := s.apiBase + "/auth-rp/auth/user"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", browserUA)
	req.Header.Set("Accept", "application/json")
	resp, err := s.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	switch resp.StatusCode {
	case http.StatusOK:
		// fallthrough below
	case http.StatusUnauthorized, http.StatusForbidden:
		s.sessionDead = true
		s.token = ""
		return "", fmt.Errorf("%w (probe answered HTTP %d)", ErrSessionDead, resp.StatusCode)
	default:
		return "", fmt.Errorf("session probe returned unexpected status %d", resp.StatusCode)
	}

	var account map[string]any
	if err := json.Unmarshal(body, &account); err != nil {
		return "", fmt.Errorf("session probe returned non-JSON body: %w", err)
	}
	s.accountInfo = account
	s.accountNum = findAccountNumber(account)

	// Pick up the minted access token cookie, if the probe set one.
	tok := ""
	for _, c := range resp.Cookies() {
		if strings.EqualFold(c.Name, accessCookieName) && c.Value != "" {
			tok = c.Value
		}
	}
	if tok != "" {
		s.applyTokenLocked(tok, s.now())
		s.sessionDead = false
		s.log.Info("minted a fresh Starlink access token via session probe")
		return tok, nil
	}
	// Probe answered but did not mint a cookie token; try renewal.
	s.sessionDead = false
	if tok2, err := s.renewTokenLocked(ctx); err == nil {
		return tok2, nil
	}
	if s.token != "" {
		return s.token, nil
	}
	return "", errors.New("session probe succeeded but no access token could be minted")
}

// findAccountNumber searches the account JSON for an account number value.
func findAccountNumber(m map[string]any) string {
	candidates := []string{"accountNumber", "account_number", "accNumber", "account"}
	for _, key := range candidates {
		if v, ok := m[key]; ok {
			switch val := v.(type) {
			case string:
				if val != "" {
					return val
				}
			case map[string]any:
				if inner := findAccountNumber(val); inner != "" {
					return inner
				}
			}
		}
	}
	// Some payloads nest the account under a wrapper object.
	for _, v := range m {
		if sub, ok := v.(map[string]any); ok {
			if inner := findAccountNumber(sub); inner != "" {
				return inner
			}
		}
	}
	return ""
}

// request performs an authenticated HTTP request against the account API.
// On 401/403 it re-authenticates once and retries (at most two attempts).
func (s *Session) request(ctx context.Context, method, rawURL string, body []byte, contentType string) (*http.Response, []byte, error) {
	// Make sure we have a valid token before the first attempt.
	if err := s.Ensure(ctx); err != nil {
		return nil, nil, err
	}
	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		if attempt > 0 {
			// Re-authenticate before retrying: stale-out the token so the
			// renewal path runs, falling back to a probe if rejected.
			s.mu.Lock()
			s.tokenAt = time.Time{}
			_, terr := s.ensureTokenLocked(ctx)
			s.mu.Unlock()
			if terr != nil {
				return nil, nil, terr
			}
		}
		resp, data, err := s.attempt(ctx, method, rawURL, body, contentType)
		if err == nil {
			return resp, data, nil
		}
		var httpErr *httpStatusError
		if errors.As(err, &httpErr) && (httpErr.status == http.StatusUnauthorized || httpErr.status == http.StatusForbidden) {
			lastErr = err
			continue
		}
		return nil, nil, err
	}
	return nil, nil, lastErr
}

type httpStatusError struct {
	status int
	body   string
}

func (e *httpStatusError) Error() string {
	return fmt.Sprintf("HTTP %d: %s", e.status, e.body)
}

func (s *Session) attempt(ctx context.Context, method, rawURL string, body []byte, contentType string) (*http.Response, []byte, error) {
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, rawURL, rdr)
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("User-Agent", browserUA)
	req.Header.Set("Accept", "application/json")
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	s.mu.Lock()
	tok := s.token
	s.mu.Unlock()
	if tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	resp, err := s.http.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, nil, err
	}
	if resp.StatusCode >= 400 {
		return nil, nil, &httpStatusError{status: resp.StatusCode, body: truncateForLog(data)}
	}
	return resp, data, nil
}

func truncateForLog(b []byte) string {
	if len(b) > 512 {
		return string(b[:512]) + "..."
	}
	return string(b)
}
