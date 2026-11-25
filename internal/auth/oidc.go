package auth

import (
	"context"
	"crypto/subtle"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

const (
	oidcStateCookie = "slmgr_oidc_state"
)

// OIDCAuth implements the Keycloak/OIDC authorization-code flow with a
// signed session cookie as the post-login session.
type OIDCAuth struct {
	provider *oidc.Provider
	verifier *oidc.IDTokenVerifier
	oauth    *oauth2.Config
	claim    string
	admin    AdminMatcher
	cookies  cookieConfig
	log      *slog.Logger
}

// NewOIDCAuth initializes the OIDC provider from the issuer URL. Discovery
// failures surface here so startup fails fast with a clear message.
func NewOIDCAuth(ctx context.Context, issuerURL, clientID, clientSecret, redirectURL, adminClaim string, adminGroups []string, cookieSecret []byte, cookieSecure bool, log *slog.Logger) (*OIDCAuth, error) {
	if log == nil {
		log = slog.Default()
	}
	provider, err := oidc.NewProvider(ctx, issuerURL)
	if err != nil {
		return nil, err
	}
	return &OIDCAuth{
		provider: provider,
		verifier: provider.Verifier(&oidc.Config{ClientID: clientID}),
		oauth: &oauth2.Config{
			ClientID:     clientID,
			ClientSecret: clientSecret,
			RedirectURL:  redirectURL,
			Endpoint:     provider.Endpoint(),
			Scopes:       []string{oidc.ScopeOpenID, "profile", "email"},
		},
		claim:   adminClaim,
		admin:   AdminMatcher{ClaimName: adminClaim, Groups: adminGroups},
		cookies: cookieConfig{secret: cookieSecret, secure: cookieSecure},
		log:     log,
	}, nil
}

func (a *OIDCAuth) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cookie, err := r.Cookie(SessionCookieName)
		if err == nil && cookie.Value != "" {
			if u, err := newSessionCodec(a.cookies.secret).decode(cookie.Value); err == nil {
				next.ServeHTTP(w, r.WithContext(withUser(r.Context(), u)))
				return
			}
		}
		// Unauthenticated: redirect browsers to the login flow, deny API calls.
		if wantsJSON(r) {
			http.Error(w, "authentication required", http.StatusUnauthorized)
			return
		}
		http.Redirect(w, r, "/login", http.StatusSeeOther)
	})
}

func wantsJSON(r *http.Request) bool {
	return strings.HasPrefix(r.URL.Path, "/api/") || strings.HasPrefix(r.Header.Get("Accept"), "application/json")
}

func (a *OIDCAuth) Routes(mux *http.ServeMux) {
	mux.HandleFunc("GET /login", a.handleLogin)
	mux.HandleFunc("GET /callback", a.handleCallback)
	mux.HandleFunc("GET /logout", a.handleLogout)
}

func (a *OIDCAuth) LogoutURL() string { return "/logout" }

// handleLogin starts the authorization-code flow with a CSRF state cookie.
func (a *OIDCAuth) handleLogin(w http.ResponseWriter, r *http.Request) {
	state := randomState()
	http.SetCookie(w, &http.Cookie{
		Name:     oidcStateCookie,
		Value:    state,
		Path:     "/",
		Expires:  time.Now().Add(10 * time.Minute),
		HttpOnly: true,
		Secure:   a.cookies.secure,
		SameSite: http.SameSiteLaxMode,
	})
	http.Redirect(w, r, a.oauth.AuthCodeURL(state), http.StatusSeeOther)
}

// handleCallback exchanges the code, verifies the ID token and issues the
// session cookie.
func (a *OIDCAuth) handleCallback(w http.ResponseWriter, r *http.Request) {
	stateCookie, err := r.Cookie(oidcStateCookie)
	if err != nil || stateCookie.Value == "" {
		http.Error(w, "missing login state; restart the login flow", http.StatusBadRequest)
		return
	}
	if subtle.ConstantTimeCompare([]byte(stateCookie.Value), []byte(r.URL.Query().Get("state"))) != 1 {
		http.Error(w, "login state mismatch", http.StatusBadRequest)
		return
	}
	code := r.URL.Query().Get("code")
	if code == "" {
		http.Error(w, "missing authorization code", http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	token, err := a.oauth.Exchange(ctx, code)
	if err != nil {
		a.log.Error("OIDC code exchange failed", "err", err)
		http.Error(w, "login failed (code exchange)", http.StatusBadGateway)
		return
	}
	rawIDToken, ok := token.Extra("id_token").(string)
	if !ok || rawIDToken == "" {
		http.Error(w, "login failed (no ID token)", http.StatusBadGateway)
		return
	}
	idToken, err := a.verifier.Verify(ctx, rawIDToken)
	if err != nil {
		a.log.Error("ID token verification failed", "err", err)
		http.Error(w, "login failed (ID token verification)", http.StatusUnauthorized)
		return
	}
	var claims map[string]any
	if err := idToken.Claims(&claims); err != nil {
		http.Error(w, "login failed (claims parsing)", http.StatusInternalServerError)
		return
	}
	groups := flattenClaim(claims[a.claim])
	u := &User{
		Sub:    idToken.Subject,
		Email:  stringClaim(claims, "email"),
		Name:   stringClaim(claims, "name"),
		Groups: groups,
		Admin:  a.admin.IsAdmin(claims[a.claim]),
		Exp:    idToken.Expiry.Unix(),
	}
	if err := setSessionCookie(w, a.cookies, u); err != nil {
		http.Error(w, "login failed (session issue)", http.StatusInternalServerError)
		return
	}
	// Clear the state cookie.
	http.SetCookie(w, &http.Cookie{Name: oidcStateCookie, Value: "", Path: "/", Expires: time.Unix(1, 0), HttpOnly: true})
	a.log.Info("user logged in", "user", u.Email, "admin", u.Admin)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (a *OIDCAuth) handleLogout(w http.ResponseWriter, r *http.Request) {
	clearSessionCookie(w, a.cookies)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func stringClaim(claims map[string]any, key string) string {
	if v, ok := claims[key].(string); ok {
		return v
	}
	return ""
}
