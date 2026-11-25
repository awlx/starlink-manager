package auth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
)

// TestOIDCSessionOutlivesIDToken logs in against a fake provider whose ID
// token expires in five minutes and checks the session uses SessionTTL.
func TestOIDCSessionOutlivesIDToken(t *testing.T) {
	var issuer string
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer":                                issuer,
			"authorization_endpoint":                issuer + "/auth",
			"token_endpoint":                        issuer + "/token",
			"jwks_uri":                              issuer + "/jwks",
			"id_token_signing_alg_values_supported": []string{"RS256"},
		})
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		enc := func(v any) string { b, _ := json.Marshal(v); return base64.RawURLEncoding.EncodeToString(b) }
		idt := enc(map[string]any{"alg": "RS256"}) + "." + enc(map[string]any{
			"iss": issuer, "aud": "client", "sub": "u1", "email": "u@example.test",
			"exp": time.Now().Add(5 * time.Minute).Unix(), "iat": time.Now().Unix(),
			"groups": []string{"ffmuc-admin"},
		}) + ".c2ln"
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "at", "token_type": "Bearer", "id_token": idt, "expires_in": 300})
	})
	idp := httptest.NewServer(mux)
	defer idp.Close()
	issuer = idp.URL

	a, err := NewOIDCAuth(context.Background(), issuer, "client", "secret", "http://app/callback",
		"groups", []string{"ffmuc-admin"}, []byte(strings.Repeat("k", 32)), false, nil)
	if err != nil {
		t.Fatal(err)
	}
	// The fake token is unsigned; signature checks are go-oidc's concern.
	a.verifier = a.provider.Verifier(&oidc.Config{ClientID: "client", InsecureSkipSignatureCheck: true})

	req := httptest.NewRequest("GET", "/callback?"+url.Values{"state": {"st"}, "code": {"c"}}.Encode(), nil)
	req.AddCookie(&http.Cookie{Name: oidcStateCookie, Value: "st"})
	rec := httptest.NewRecorder()
	a.handleCallback(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("callback status %d: %s", rec.Code, rec.Body.String())
	}
	var session string
	for _, c := range rec.Result().Cookies() {
		if c.Name == SessionCookieName {
			session = c.Value
		}
	}
	u, err := newSessionCodec([]byte(strings.Repeat("k", 32))).decode(session)
	if err != nil {
		t.Fatal(err)
	}
	if left := time.Until(time.Unix(u.Exp, 0)); left < SessionTTL-time.Minute {
		t.Fatalf("session expires in %s, want about %s", left, SessionTTL)
	}
	if !u.Admin {
		t.Error("admin group claim not honoured")
	}
}
