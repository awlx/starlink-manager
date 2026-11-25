package starlink

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// newSilentLoginAPI fakes the starlink.com web login: the probe answers 200
// with an empty body and no token cookie (as the production API does), the
// refresh endpoint needs the web session cookie, and the login flow issues
// it only when the SSO cookie is valid.
func newSilentLoginAPI(t *testing.T, validSSO string) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var callbacks atomic.Int32
	claims := base64.RawURLEncoding.EncodeToString([]byte(`{"account_id":"ACC-JWT-1"}`))
	jwt := "h." + claims + ".s"
	mux := http.NewServeMux()
	mux.HandleFunc("/auth-rp/auth/user", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/api/auth/auth/login", func(w http.ResponseWriter, r *http.Request) {
		http.SetCookie(w, &http.Cookie{Name: ".AspNetCore.Correlation", Value: "corr", Path: "/"})
		http.Redirect(w, r, "/api/auth/connect/authorize?client_id=x", http.StatusFound)
	})
	mux.HandleFunc("/api/auth/connect/authorize", func(w http.ResponseWriter, r *http.Request) {
		sso, err := r.Cookie(ssoCookieName)
		if err != nil || sso.Value != validSSO {
			http.Redirect(w, r, "/auth/login", http.StatusFound)
			return
		}
		fmt.Fprint(w, `<html><body onload="document.forms[0].submit()">
<form method='post' action='/api/auth/auth/callback'>
<input type='hidden' name='code' value='abc&amp;123' />
<input type="hidden" value="st" name="state"/>
</form></body></html>`)
	})
	mux.HandleFunc("/auth/login", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `<html><form action="/auth/password"><input name="email"></form></html>`)
	})
	mux.HandleFunc("/api/auth/auth/callback", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.FormValue("code") != "abc&123" || r.FormValue("state") != "st" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if _, err := r.Cookie(".AspNetCore.Correlation"); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		callbacks.Add(1)
		http.SetCookie(w, &http.Cookie{Name: "Starlink.Com.Access.V1C1", Value: "web-session", Path: "/", Secure: true})
		http.Redirect(w, r, "/account/home", http.StatusFound)
	})
	mux.HandleFunc("/account/home", func(w http.ResponseWriter, r *http.Request) {
		t.Error("silent login must not fetch the return URL")
	})
	mux.HandleFunc("/api/auth/auth/refresh-token", func(w http.ResponseWriter, r *http.Request) {
		if c, err := r.Cookie("Starlink.Com.Access.V1C1"); err != nil || c.Value != "web-session" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		fmt.Fprintf(w, `{"accessToken":%q,"expiresIn":900}`, jwt)
	})
	ts := httptest.NewTLSServer(mux)
	t.Cleanup(ts.Close)
	return ts, &callbacks
}

func TestSessionSilentLoginMintsToken(t *testing.T) {
	ts, callbacks := newSilentLoginAPI(t, "good-sso")
	s := NewSession(ts.URL, ts.URL, "good-sso", ts.Client(), testLogger(t))
	tok, err := s.Token(context.Background())
	if err != nil {
		t.Fatalf("Token: %v", err)
	}
	if accountFromJWT(tok) != "ACC-JWT-1" {
		t.Fatalf("unexpected token %q", tok)
	}
	if callbacks.Load() != 1 {
		t.Fatalf("callbacks = %d, want 1", callbacks.Load())
	}
	if s.AccountNumber() != "ACC-JWT-1" {
		t.Fatalf("account number = %q, want it learned from the token", s.AccountNumber())
	}
	if s.SessionDead() {
		t.Fatal("session should be alive")
	}
}

func TestSessionSilentLoginDeadSSO(t *testing.T) {
	ts, callbacks := newSilentLoginAPI(t, "good-sso")
	s := NewSession(ts.URL, ts.URL, "expired-sso", ts.Client(), testLogger(t))
	_, err := s.Token(context.Background())
	if !errors.Is(err, ErrSessionDead) {
		t.Fatalf("err = %v, want ErrSessionDead", err)
	}
	if !s.SessionDead() || callbacks.Load() != 0 {
		t.Fatalf("dead=%v callbacks=%d", s.SessionDead(), callbacks.Load())
	}
}

func TestParseAutoPostFormRejectsForeignHost(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/auth/auth/login", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `<form action="https://evil.example/cb"><input name="code" value="c"></form>`)
	})
	ts := httptest.NewTLSServer(mux)
	defer ts.Close()
	s := NewSession(ts.URL, ts.URL, "sso", ts.Client(), testLogger(t))
	s.mu.Lock()
	err := s.silentLoginLocked(context.Background())
	s.mu.Unlock()
	if err == nil || errors.Is(err, ErrSessionDead) {
		t.Fatalf("err = %v, want a foreign-host refusal", err)
	}
}

// TestSessionAccessorsDoNotBlockOnMint guards the page handlers: reading the
// session state must not wait for an in-flight probe.
func TestSessionAccessorsDoNotBlockOnMint(t *testing.T) {
	f, ts := newFakeAPI(t)
	s := newTestSession(t, ts, &fakeClock{now: time.Now()})
	f.mu.Lock()
	f.probeDelay = 500 * time.Millisecond
	f.mu.Unlock()
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = s.Token(context.Background())
	}()
	time.Sleep(100 * time.Millisecond) // let the probe take the mutex
	start := time.Now()
	_ = s.SessionDead()
	_ = s.AccountNumber()
	if d := time.Since(start); d > 100*time.Millisecond {
		t.Fatalf("accessors blocked for %s behind the probe", d)
	}
	<-done
}
