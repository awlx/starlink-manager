package auth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestSessionCodecRoundtrip(t *testing.T) {
	secret := []byte(strings.Repeat("k", 32))
	c := newSessionCodec(secret)
	u := &User{Sub: "abc", Email: "a@b.c", Name: "Alice", Groups: []string{"ffmuc-admin"}, Admin: true}
	cookie, err := c.encode(u)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	got, err := c.decode(cookie)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Sub != "abc" || got.Email != "a@b.c" || !got.Admin || len(got.Groups) != 1 {
		t.Fatalf("decoded user wrong: %+v", got)
	}
}

func TestSessionCodecTamperRejected(t *testing.T) {
	secret := []byte(strings.Repeat("k", 32))
	c := newSessionCodec(secret)
	u := &User{Sub: "abc"}
	cookie, _ := c.encode(u)
	// Flip the payload.
	parts := strings.Split(cookie, ".")
	tampered := strings.Replace(parts[0], "abc", "xyz", 1) + "." + parts[1]
	if tampered != cookie { // only meaningful if payload contained abc
		if _, err := c.decode(tampered); err == nil {
			t.Fatal("tampered cookie accepted")
		}
	}
	// Wrong signature.
	if _, err := c.decode(cookie + "x"); err == nil {
		t.Fatal("bad signature accepted")
	}
	// Different secret.
	c2 := newSessionCodec([]byte(strings.Repeat("z", 32)))
	if _, err := c2.decode(cookie); err == nil {
		t.Fatal("cookie from another secret accepted")
	}
}

func TestSessionCodecExpiry(t *testing.T) {
	c := newSessionCodec([]byte(strings.Repeat("k", 32)))
	cookie, _ := c.encode(&User{Sub: "abc", Exp: time.Now().Add(-time.Minute).Unix()})
	if _, err := c.decode(cookie); err == nil || !strings.Contains(err.Error(), "expired") {
		t.Fatalf("expected expiry error, got %v", err)
	}
}

func TestIsAdminClaim(t *testing.T) {
	cases := []struct {
		name   string
		claim  any
		groups []string
		want   bool
	}{
		{"string match", "ffmuc-admin", []string{"ffmuc-admin"}, true},
		{"case-insensitive", "FFMUC-Admin", []string{"ffmuc-admin"}, true},
		{"string list", []any{"users", "ffmuc-admin"}, []string{"ffmuc-admin"}, true},
		{"no match", []any{"users"}, []string{"ffmuc-admin"}, false},
		{"empty groups", "ffmuc-admin", nil, false},
		{"nil claim", nil, []string{"ffmuc-admin"}, false},
		{"comma string", "users,ffmuc-admin", []string{"ffmuc-admin"}, true},
		{"keycloak role objects", []any{map[string]any{"name": "ffmuc-admin"}}, []string{"ffmuc-admin"}, true},
		{"whitespace trimmed", " ffmuc-admin ", []string{"ffmuc-admin"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsAdminClaim(tc.claim, tc.groups); got != tc.want {
				t.Errorf("IsAdminClaim(%v, %v) = %v, want %v", tc.claim, tc.groups, got, tc.want)
			}
		})
	}
}

func TestForwardAuthMiddleware(t *testing.T) {
	f := &ForwardAuth{
		Headers: HeaderNames{User: "X-Forwarded-User", Email: "X-Forwarded-Email", Groups: "X-Forwarded-Groups"},
		Admin:   AdminMatcher{ClaimName: "groups", Groups: []string{"ffmuc-admin"}},
	}
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, ok := UserFromContext(r.Context())
		if !ok {
			w.WriteHeader(http.StatusTeapot)
			return
		}
		if u.Admin {
			w.WriteHeader(http.StatusOK)
		} else {
			w.WriteHeader(http.StatusNoContent)
		}
	})
	srv := httptest.NewServer(f.Middleware(inner))
	defer srv.Close()

	// No header → 401.
	resp, _ := http.Get(srv.URL)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("missing header status = %d, want 401", resp.StatusCode)
	}
	resp.Body.Close()

	// Non-admin user → 204.
	req, _ := http.NewRequest("GET", srv.URL, nil)
	req.Header.Set("X-Forwarded-User", "someone")
	req.Header.Set("X-Forwarded-Email", "someone@ffmuc.net")
	resp, _ = http.DefaultClient.Do(req)
	if resp.StatusCode != http.StatusNoContent {
		t.Errorf("non-admin status = %d, want 204", resp.StatusCode)
	}
	resp.Body.Close()

	// Admin user → 200.
	req, _ = http.NewRequest("GET", srv.URL, nil)
	req.Header.Set("X-Forwarded-User", "admin1")
	req.Header.Set("X-Forwarded-Groups", "ffmuc-admin,orga")
	resp, _ = http.DefaultClient.Do(req)
	if resp.StatusCode != http.StatusOK {
		t.Errorf("admin status = %d, want 200", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestLocalDevMiddleware(t *testing.T) {
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, ok := UserFromContext(r.Context())
		if !ok || !u.Admin {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	srv := httptest.NewServer(LocalDev{}.Middleware(inner))
	defer srv.Close()
	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("localdev status = %d, want 200", resp.StatusCode)
	}
}

func TestRequireAdminGate(t *testing.T) {
	handler := RequireAdmin(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	mk := func(u *User) *http.Request {
		r := httptest.NewRequest("POST", "/x", nil)
		return r.WithContext(withUser(r.Context(), u))
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, mk(&User{Sub: "u", Admin: false}))
	if rec.Code != http.StatusForbidden {
		t.Errorf("non-admin = %d, want 403", rec.Code)
	}
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, mk(&User{Sub: "u", Admin: true}))
	if rec.Code != http.StatusOK {
		t.Errorf("admin = %d, want 200", rec.Code)
	}
	// No user in context at all.
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest("GET", "/x", nil))
	if rec.Code != http.StatusForbidden {
		t.Errorf("anonymous = %d, want 403", rec.Code)
	}
}

func TestRequireUserGate(t *testing.T) {
	handler := RequireUser(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest("GET", "/x", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("anonymous = %d, want 401", rec.Code)
	}
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest("GET", "/x", nil).WithContext(withUser(context.Background(), &User{Sub: "u"})))
	if rec.Code != http.StatusOK {
		t.Errorf("authenticated = %d, want 200", rec.Code)
	}
}
