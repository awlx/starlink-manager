// Package auth implements user authentication for the web UI.
//
// Three modes are supported (selected via AUTH_MODE):
//   - oidc:       Keycloak (or any OIDC provider) authorization-code flow
//   - forwardauth: trust X-Forwarded-* headers injected by a reverse-proxy
//     forward-auth service (e.g. Traefik + noc-auth)
//   - localdev:   every request is treated as an authenticated admin
//
// In all modes a signed session cookie carries the resolved user identity
// (subject, email, name, groups) plus an admin flag computed from the
// configured admin claim/groups.
package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// SessionTTL is how long a session cookie stays valid.
const SessionTTL = 12 * time.Hour

// SessionCookieName is the cookie used for authenticated sessions.
const SessionCookieName = "slmgr_session"

// User is the identity carried in the session cookie.
type User struct {
	Sub    string   `json:"sub"`
	Email  string   `json:"email,omitempty"`
	Name   string   `json:"name,omitempty"`
	Groups []string `json:"groups,omitempty"`
	Admin  bool     `json:"admin"`
	Exp    int64    `json:"exp"` // unix seconds
}

// sessionCodec signs session payloads with HMAC-SHA256:
//
//	cookie := base64url(json(payload)) + "." + base64url(hmac)
type sessionCodec struct {
	secret []byte
}

func newSessionCodec(secret []byte) *sessionCodec {
	return &sessionCodec{secret: secret}
}

func (c *sessionCodec) encode(u *User) (string, error) {
	if u.Exp == 0 {
		u = &User{Sub: u.Sub, Email: u.Email, Name: u.Name, Groups: u.Groups, Admin: u.Admin, Exp: time.Now().Add(SessionTTL).Unix()}
	}
	payload, err := json.Marshal(u)
	if err != nil {
		return "", err
	}
	b64 := base64.RawURLEncoding.EncodeToString(payload)
	mac := c.mac([]byte(b64))
	return b64 + "." + base64.RawURLEncoding.EncodeToString(mac), nil
}

func (c *sessionCodec) decode(cookie string) (*User, error) {
	b64, macB64, ok := strings.Cut(cookie, ".")
	if !ok {
		return nil, errors.New("malformed session cookie")
	}
	mac, err := base64.RawURLEncoding.DecodeString(macB64)
	if err != nil {
		return nil, errors.New("malformed session cookie signature")
	}
	if !hmac.Equal(mac, c.mac([]byte(b64))) {
		return nil, errors.New("session cookie signature mismatch")
	}
	payload, err := base64.RawURLEncoding.DecodeString(b64)
	if err != nil {
		return nil, errors.New("malformed session cookie payload")
	}
	var u User
	if err := json.Unmarshal(payload, &u); err != nil {
		return nil, errors.New("malformed session cookie payload")
	}
	if u.Exp > 0 && time.Now().Unix() > u.Exp {
		return nil, errors.New("session expired")
	}
	if u.Sub == "" {
		return nil, errors.New("session payload missing subject")
	}
	return &u, nil
}

func (c *sessionCodec) mac(data []byte) []byte {
	h := hmac.New(sha256.New, c.secret)
	h.Write(data)
	return h.Sum(nil)
}

// setSessionCookie writes the signed session cookie.
func setSessionCookie(w http.ResponseWriter, cfg cookieConfig, u *User) error {
	value, err := newSessionCodec(cfg.secret).encode(u)
	if err != nil {
		return err
	}
	http.SetCookie(w, &http.Cookie{
		Name:     SessionCookieName,
		Value:    value,
		Path:     "/",
		Expires:  time.Now().Add(SessionTTL),
		HttpOnly: true,
		Secure:   cfg.secure,
		SameSite: http.SameSiteLaxMode,
	})
	return nil
}

// clearSessionCookie removes the session cookie.
func clearSessionCookie(w http.ResponseWriter, cfg cookieConfig) {
	http.SetCookie(w, &http.Cookie{
		Name:     SessionCookieName,
		Value:    "",
		Path:     "/",
		Expires:  time.Unix(1, 0),
		HttpOnly: true,
		Secure:   cfg.secure,
		SameSite: http.SameSiteLaxMode,
	})
}

type cookieConfig struct {
	secret []byte
	secure bool
}

// randomState returns a fresh URL-safe random string for OAuth state.
func randomState() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(fmt.Sprintf("reading random bytes: %v", err))
	}
	return base64.RawURLEncoding.EncodeToString(b)
}
