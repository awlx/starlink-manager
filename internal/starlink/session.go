package starlink

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
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

	jar        *cookieJar
	http       *http.Client // auth client: carries the cookie jar (probe + renewal)
	dataClient *http.Client // data client: never sends cookies (Bearer-only endpoints)
	log        *slog.Logger

	// mu serializes token minting (single-flight) and is held across the
	// probe/login/renewal HTTP calls. Read-only accessors used by page
	// handlers must not take it, or rendering would block on minting.
	mu        sync.Mutex
	token     string // written under mu and infoMu; read under either
	tokenAt   time.Time
	lastProbe time.Time
	now       func() time.Time

	// probeFailures counts consecutive failed session probes/renewals.
	// After deadAfterFailures consecutive failures the session latches as
	// dead so a flaky API does not spin probes forever.
	probeFailures atomic.Int32

	// recoveryProbe is set while a post-cooldown recovery probe is in
	// flight; a failure on that probe latches dead immediately (one
	// strike) so the app does not flap between probing and failing. It is
	// cleared by any successful authenticated call (clearDead), which can
	// happen without s.mu (MarkAlive), hence atomic.
	recoveryProbe atomic.Bool

	// infoMu guards the fields read by UI handlers and data requests.
	infoMu      sync.Mutex
	accountNum  string
	accountInfo map[string]any

	// deadSince stores the unix time at which the session was latched as
	// dead, or 0 while the session is considered alive. A latched session
	// recovers after deadCooldown so a single transient 401 cannot brick
	// polling permanently.
	deadSince atomic.Int64
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
		apiBase:    strings.TrimRight(apiBase, "/"),
		webBase:    strings.TrimRight(webBase, "/"),
		sso:        ssoCookie,
		jar:        newCookieJar(),
		http:       client,
		dataClient: &http.Client{Timeout: client.Timeout},
		log:        log,
		now:        time.Now,
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

// DataClient returns a client that never sends cookies. The account API's
// data endpoints (roster, telemetry, device gateway) reject requests that
// carry cookies alongside the bearer token (401 token_expired), so those
// calls must use this client.
func (s *Session) DataClient() *http.Client {
	return s.dataClient
}

// SessionDead reports whether the SSO cookie has been rejected.
func (s *Session) SessionDead() bool {
	return s.deadSince.Load() != 0
}

// deadCooldown is how long a latched session stays latched before a
// recovery probe is attempted again. It covers both a restarted process
// (whose in-memory probe window resets, but whose cookie is still
// rejected) and longer API-side hiccups. It is a variable so tests can
// shorten it.
var deadCooldown = 10 * time.Minute

// deadAfterFailures is the number of consecutive failed probes/renewals
// tolerated before the session latches as dead.
const deadAfterFailures = 2

// latchDead marks the session dead after the configured number of
// consecutive failures. Must be called with s.mu held.
func (s *Session) latchDeadLocked() {
	// A failed post-cooldown recovery probe latches immediately: the
	// session was already latched once and the probe re-confirmed it, so
	// counting again would only add flapping between probe and failure.
	if s.recoveryProbe.Load() || s.probeFailures.Add(1) >= deadAfterFailures {
		s.recoveryProbe.Store(false)
		if s.deadSince.CompareAndSwap(0, s.now().Unix()) {
			s.log.Warn("starlink session latched as dead; a recovery probe runs in " + deadCooldown.String())
		}
		return
	}
}

// forceLatchDead latches the session dead immediately (definitive evidence
// such as an expired SSO cookie), bypassing the failure counter.
func (s *Session) forceLatchDead() {
	s.recoveryProbe.Store(false)
	if s.deadSince.CompareAndSwap(0, s.now().Unix()) {
		s.log.Warn("starlink session latched as dead; a recovery probe runs in " + deadCooldown.String())
	}
}

// clearDead resets the dead latch and failure counter after any successful
// authenticated call (probe, renewal or data request).
func (s *Session) clearDead() {
	s.deadSince.Store(0)
	s.probeFailures.Store(0)
	s.recoveryProbe.Store(false)
}

// MarkAlive clears the dead latch after a successful authenticated data
// call, so a recovered API marks the session healthy without waiting for
// the next probe.
func (s *Session) MarkAlive() {
	// A recovery probe that handed out a still-fresh token is only proven
	// good by the data call that used it; end the one-strike window here.
	if s.deadSince.Load() != 0 || s.recoveryProbe.Load() {
		s.clearDead()
		s.log.Info("starlink session recovered via a successful data call")
	}
}

// RecoveryDue reports whether a latched session is due for a recovery
// probe (the cooldown has elapsed).
func (s *Session) RecoveryDue() bool {
	ts := s.deadSince.Load()
	if ts == 0 {
		return false
	}
	return s.now().Sub(time.Unix(ts, 0)) >= deadCooldown
}

// deadBlocked reports whether token work is blocked by the dead latch.
// A latched session unblocks once the cooldown elapses; the caller then
// attempts a recovery probe.
func (s *Session) deadBlocked() bool {
	return s.deadSince.Load() != 0 && !s.RecoveryDue()
}

// AccountNumber returns the account number learned from the session probe,
// or "" before the first successful probe.
func (s *Session) AccountNumber() string {
	s.infoMu.Lock()
	defer s.infoMu.Unlock()
	return s.accountNum
}

// AccountInfo returns the raw account JSON from the session probe.
func (s *Session) AccountInfo() map[string]any {
	s.infoMu.Lock()
	defer s.infoMu.Unlock()
	return s.accountInfo
}

// currentToken returns the latest minted token without waiting for an
// in-flight mint.
func (s *Session) currentToken() string {
	s.infoMu.Lock()
	defer s.infoMu.Unlock()
	return s.token
}

// setTokenLocked must be called with s.mu held.
func (s *Session) setTokenLocked(tok string) {
	s.infoMu.Lock()
	s.token = tok
	s.infoMu.Unlock()
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
// Refresh forces a re-authentication: stale the current token and mint a
// fresh one through renewal or a session probe. Throttling is handled by
// ensureTokenLocked, which waits for the shared probe window instead of
// failing the caller.
func (s *Session) Refresh(ctx context.Context) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.deadBlocked() {
		return "", ErrSessionDead
	}
	if s.SessionDead() {
		s.log.Info("attempting session recovery probe after cooldown")
		s.deadSince.Store(0)
		s.probeFailures.Store(0)
		s.recoveryProbe.Store(true)
	}
	if s.token != "" {
		s.tokenAt = time.Time{} // stale the token so a fresh one is minted
	}
	return s.ensureTokenLocked(ctx)
}

// RefreshStale is Refresh for a caller that was rejected while using
// stale: when another caller already replaced that token with a fresh one,
// the fresh token is returned without minting again.
func (s *Session) RefreshStale(ctx context.Context, stale string) (string, error) {
	s.mu.Lock()
	if s.token != "" && s.token != stale && s.now().Sub(s.tokenAt) < tokenFreshFor {
		tok := s.token
		s.mu.Unlock()
		return tok, nil
	}
	s.mu.Unlock()
	return s.Refresh(ctx)
}

// ensureTokenLocked must be called with s.mu held. It serializes minting so
// concurrent callers share a single probe (single-flight), and enforces the
// 60s minimum probe interval.
const (
	// probeWaitPoll is the polling interval while waiting for the probe
	// rate-limit window to expire.
	probeWaitPoll = 250 * time.Millisecond
)

// probeAwaitMax bounds how long a dependent caller waits for the shared
// probe window before giving up (the caller can retry later). It is a
// variable so tests can shorten it.
var probeAwaitMax = 20 * time.Second

// ensureTokenLocked returns a usable access token, minting one if needed.
//
// The probe rate limit (at most one probe per 60s) must not fail dependent
// callers such as the roster poll: callers that arrive while the window is
// still closed wait here — bounded by probeAwaitMax and the caller's
// context — while the caller holding the mutex performs the single probe.
// That makes every waiter converge on one shared probe.
func (s *Session) ensureTokenLocked(ctx context.Context) (string, error) {
	if s.deadBlocked() {
		return "", ErrSessionDead
	}
	if s.SessionDead() {
		// Cooldown elapsed: attempt a recovery probe instead of
		// failing every dependent caller forever.
		s.log.Info("attempting session recovery probe after cooldown")
		s.deadSince.Store(0)
		s.probeFailures.Store(0)
		s.recoveryProbe.Store(true)
	}
	now := s.now()
	if s.token != "" && now.Sub(s.tokenAt) < tokenFreshFor {
		return s.token, nil
	}
	if s.token != "" {
		// Token stale (aged out or staled by Refresh): try the cheap
		// renewal first; the renewal endpoint is not rate-limited the way
		// the probe is.
		tok, err := s.renewTokenLocked(ctx)
		if err == nil {
			s.clearDead()
			return tok, nil
		}
		if errors.Is(err, errRenewRejected) {
			s.setTokenLocked("")
		} else {
			s.log.Warn("token renewal failed; keeping previous token", "err", err)
			// Inside the probe rate-limit window keep the old token;
			// otherwise fall through to the probe below.
			if since := now.Sub(s.lastProbe); !s.lastProbe.IsZero() && since < probeMinInterval {
				return s.token, nil
			}
		}
	}
	// No token and possibly inside the probe rate-limit window: wait for
	// the window (or the in-flight probe holding the mutex) instead of
	// erroring out the caller.
	waited := time.Duration(0)
	for {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		since := s.now().Sub(s.lastProbe)
		if s.lastProbe.IsZero() || since >= probeMinInterval {
			if err := ctx.Err(); err != nil {
				return "", err
			}
			return s.probeLocked(ctx)
		}
		if waited >= probeAwaitMax {
			return "", fmt.Errorf("%w (waited %s)", ErrProbeThrottled, waited.Round(time.Second))
		}
		// Release the mutex so the in-flight probe can run, then poll.
		s.mu.Unlock()
		sleep := probeWaitPoll
		if rem := probeMinInterval - since; rem < sleep {
			sleep = rem
		}
		if rem := probeAwaitMax - waited; rem < sleep {
			sleep = rem
		}
		time.Sleep(sleep)
		waited += sleep
		s.mu.Lock()
		if s.deadBlocked() {
			return "", ErrSessionDead
		}
		if s.SessionDead() {
			s.deadSince.Store(0)
			s.probeFailures.Store(0)
			s.recoveryProbe.Store(true)
		}
		// Another caller's probe may have minted a fresh token while we
		// waited; reuse it instead of probing again.
		if s.token != "" && s.now().Sub(s.tokenAt) < tokenFreshFor {
			return s.token, nil
		}
	}
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
// gateway calls that require the cookie receive it. The cookie is scoped to
// the API host (both api.starlink.com and the starlink.com /api proxy answer
// data endpoints with Bearer-only; cookies on those hosts would trigger
// 401 token_expired responses).
func (s *Session) applyTokenLocked(tok string, at time.Time) {
	s.setTokenLocked(tok)
	s.tokenAt = at
	if acc := accountFromJWT(tok); acc != "" {
		s.infoMu.Lock()
		if s.accountNum == "" {
			s.accountNum = acc
		}
		s.infoMu.Unlock()
	}
	u := &url.URL{Scheme: "https", Host: hostOf(s.apiBase), Path: "/"}
	s.jar.SetCookies(u, []*http.Cookie{{
		Name:   accessCookieName,
		Value:  tok,
		Path:   "/",
		Domain: hostOf(s.apiBase),
		Secure: true,
	}})
}

// WithPresetToken seeds the session with an externally minted access token
// (STARLINK_SESSION_TOKEN). Renewal still happens through the normal flow
// once the preset token ages out.
func (s *Session) WithPresetToken(tok string) *Session {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.applyTokenLocked(tok, s.now())
	return s
}

// WithAccountNumber supplies the account number for deployments that run
// with a pre-minted token and never execute a session probe (which is where
// the account number is otherwise learned).
func (s *Session) WithAccountNumber(acc string) *Session {
	s.infoMu.Lock()
	defer s.infoMu.Unlock()
	if acc != "" {
		s.accountNum = acc
	}
	return s
}

// WithRefreshCookies installs the browser session cookie trio (Access.V1,
// V1C1, V1C2) required by the refresh-token endpoint. They are optional:
// without them the session obtains them through the silent login.
// Values are parsed from a "Name=Value; Name2=Value2" cookie header.
func (s *Session) WithRefreshCookies(header string) *Session {
	s.mu.Lock()
	defer s.mu.Unlock()
	u := &url.URL{Scheme: "https", Host: hostOf(s.webBase), Path: "/"}
	for _, part := range strings.Split(header, ";") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		name, value, ok := strings.Cut(part, "=")
		if !ok {
			continue
		}
		name = strings.TrimSpace(name)
		value = strings.TrimSpace(value)
		switch name {
		case "Starlink.Com.Access.V1", "Starlink.Com.Access.V1C1", "Starlink.Com.Access.V1C2":
			s.jar.SetCookies(u, []*http.Cookie{{
				Name:   name,
				Value:  value,
				Path:   "/",
				Domain: hostOf(s.webBase),
				Secure: true,
			}})
		case ssoCookieName:
			s.jar.SetCookies(u, []*http.Cookie{{
				Name:   ssoCookieName,
				Value:  value,
				Path:   "/",
				Domain: hostOf(s.webBase),
				Secure: true,
			}})
		}
	}
	return s
}

// probeLocked performs the session probe: GET /auth-rp/auth/user with the
// SSO cookie. The account API answers 200, often with an empty body, and
// does not reliably mint an access token cookie. When it does not, the
// session falls back to renewal and then to the silent web login, which is
// how the Starlink app/site turns the SSO cookie into an access token.
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
		s.latchDeadLocked()
		s.setTokenLocked("")
		return "", fmt.Errorf("%w (probe answered HTTP %d)", ErrSessionDead, resp.StatusCode)
	default:
		return "", fmt.Errorf("session probe returned unexpected status %d", resp.StatusCode)
	}

	if len(bytes.TrimSpace(body)) > 0 {
		var account map[string]any
		if err := json.Unmarshal(body, &account); err != nil {
			s.log.Warn("session probe returned a non-JSON body; ignoring it",
				"err", err, "snippet", truncateForLog(body))
		} else {
			s.infoMu.Lock()
			s.accountInfo = account
			if acc := findAccountNumber(account); acc != "" {
				s.accountNum = acc
			}
			s.infoMu.Unlock()
		}
	}

	// Pick up the minted access token cookie, if the probe set one.
	tok := ""
	for _, c := range resp.Cookies() {
		if strings.EqualFold(c.Name, accessCookieName) && c.Value != "" {
			tok = c.Value
		}
	}
	if tok != "" {
		s.applyTokenLocked(tok, s.now())
		s.clearDead()
		s.log.Info("minted a fresh Starlink access token via session probe")
		return tok, nil
	}
	// Probe answered but did not mint a cookie token; try renewal with
	// whatever web session cookies the jar holds.
	tok2, err := s.renewTokenLocked(ctx)
	if err == nil {
		s.clearDead()
		return tok2, nil
	}
	if !errors.Is(err, errRenewRejected) {
		return "", fmt.Errorf("session probe succeeded but token renewal failed: %w", err)
	}
	// No web session yet: run the silent login with the SSO cookie, which
	// installs the web session cookies, then renew again.
	if err := s.silentLoginLocked(ctx); err != nil {
		if errors.Is(err, ErrSessionDead) {
			// Reaching the interactive login page without an
			// authorization code is definitive evidence that the SSO
			// cookie is no longer accepted; latch immediately.
			s.forceLatchDead()
			s.setTokenLocked("")
		}
		return "", err
	}
	tok3, err := s.renewTokenLocked(ctx)
	if err != nil {
		return "", fmt.Errorf("token renewal after silent login failed: %w", err)
	}
	s.clearDead()
	s.log.Info("minted a fresh Starlink access token via silent login")
	return tok3, nil
}

// maxLoginHops bounds the redirect chain of the silent login.
const maxLoginHops = 10

var (
	formTagRe  = regexp.MustCompile(`(?is)<form\b[^>]*>`)
	inputTagRe = regexp.MustCompile(`(?is)<input\b[^>]*>`)
	attrRe     = regexp.MustCompile(`(?is)\b([a-z_:-]+)\s*=\s*(?:"([^"]*)"|'([^']*)')`)
)

// silentLoginLocked turns the SSO cookie into a starlink.com web session:
// it starts the web login, follows the authorization redirect and submits
// the auto-post form carrying the authorization code to the login callback,
// which sets the web session cookies the refresh-token endpoint needs. An
// SSO cookie that is no longer valid ends on the interactive login page
// without a code form, which marks the session dead.
func (s *Session) silentLoginLocked(ctx context.Context) error {
	client := &http.Client{
		Transport: s.http.Transport,
		Jar:       s.jar,
		Timeout:   s.http.Timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	returnURL := s.webBase + "/account/home"
	start := s.webBase + "/api/auth/auth/login?returnUrl=" + url.QueryEscape(returnURL)
	page, pageURL, err := s.loginFollow(ctx, client, http.MethodGet, start, nil)
	if err != nil {
		return err
	}
	action, fields := parseAutoPostForm(page)
	if action == "" || fields.Get("code") == "" {
		return fmt.Errorf("%w (silent login reached %s without an authorization code)", ErrSessionDead, pageURL.Path)
	}
	target, err := pageURL.Parse(action)
	if err != nil {
		return fmt.Errorf("silent login: bad form action: %w", err)
	}
	if !strings.EqualFold(target.Hostname(), hostOf(s.webBase)) {
		return fmt.Errorf("silent login: refusing to post the authorization code to foreign host %q", target.Hostname())
	}
	if _, _, err := s.loginFollow(ctx, client, http.MethodPost, target.String(), fields); err != nil {
		return err
	}
	return nil
}

// loginFollow performs a request and follows redirects inside the
// /api/auth/ login flow by hand, stopping at the first non-redirect answer
// or at a redirect that leaves the login flow (the return URL). It returns
// the final body and URL.
func (s *Session) loginFollow(ctx context.Context, client *http.Client, method, rawURL string, form url.Values) ([]byte, *url.URL, error) {
	cur, err := url.Parse(rawURL)
	if err != nil {
		return nil, nil, err
	}
	for hop := 0; hop < maxLoginHops; hop++ {
		var body io.Reader
		if form != nil {
			body = strings.NewReader(form.Encode())
		}
		req, err := http.NewRequestWithContext(ctx, method, cur.String(), body)
		if err != nil {
			return nil, nil, err
		}
		req.Header.Set("User-Agent", browserUA)
		req.Header.Set("Accept", "text/html,application/xhtml+xml")
		if form != nil {
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			req.Header.Set("Origin", s.webBase)
		}
		resp, err := client.Do(req)
		if err != nil {
			return nil, nil, err
		}
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		if resp.StatusCode < 300 || resp.StatusCode >= 400 {
			if resp.StatusCode != http.StatusOK {
				return nil, nil, fmt.Errorf("silent login: %s %s answered HTTP %d", method, cur.Path, resp.StatusCode)
			}
			return data, cur, nil
		}
		loc := resp.Header.Get("Location")
		if loc == "" {
			return nil, nil, fmt.Errorf("silent login: redirect without Location from %s", cur.Path)
		}
		next, err := cur.Parse(loc)
		if err != nil {
			return nil, nil, fmt.Errorf("silent login: bad redirect: %w", err)
		}
		if !strings.EqualFold(next.Hostname(), hostOf(s.webBase)) || !strings.HasPrefix(next.Path, "/api/auth/") {
			// Left the login flow (return URL or interactive login page).
			return nil, next, nil
		}
		cur, method, form = next, http.MethodGet, nil
	}
	return nil, nil, errors.New("silent login: too many redirects")
}

// parseAutoPostForm extracts the action and the named input values of the
// first form in an HTML page.
func parseAutoPostForm(page []byte) (string, url.Values) {
	ft := formTagRe.Find(page)
	if ft == nil {
		return "", nil
	}
	action := ""
	for _, m := range attrRe.FindAllSubmatch(ft, -1) {
		if strings.EqualFold(string(m[1]), "action") {
			action = html.UnescapeString(string(m[2]) + string(m[3]))
		}
	}
	fields := url.Values{}
	for _, tag := range inputTagRe.FindAll(page, -1) {
		var name, value string
		for _, m := range attrRe.FindAllSubmatch(tag, -1) {
			v := html.UnescapeString(string(m[2]) + string(m[3]))
			switch strings.ToLower(string(m[1])) {
			case "name":
				name = v
			case "value":
				value = v
			}
		}
		if name != "" {
			fields.Set(name, value)
		}
	}
	return action, fields
}

// accountFromJWT returns the account_id claim of an access token, without
// verifying the token (it is only used to label the account in the UI and
// address the telemetry cache).
func accountFromJWT(tok string) string {
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		return ""
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return ""
	}
	var claims struct {
		AccountID string `json:"account_id"`
	}
	if json.Unmarshal(raw, &claims) != nil {
		return ""
	}
	return claims.AccountID
}

// findAccountNumber searches the account JSON for an account number value.
func findAccountNumber(m map[string]any) string {
	candidates := []string{"accountNumber", "accountId", "account_number", "accNumber", "account"}
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
	if tok := s.currentToken(); tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	// attempt uses the data client: data endpoints must be Bearer-only
	// (cookies would trigger 401 token_expired).
	resp, err := s.dataClient.Do(req)
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
