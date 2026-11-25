package auth

import (
	"context"
	"net/http"
	"strings"
)

// ctxUserKey is the context key for the resolved user.
type ctxUserKey struct{}

// UserFromContext returns the authenticated user, if any.
func UserFromContext(ctx context.Context) (*User, bool) {
	u, ok := ctx.Value(ctxUserKey{}).(*User)
	return u, ok
}

func withUser(ctx context.Context, u *User) context.Context {
	return context.WithValue(ctx, ctxUserKey{}, u)
}

// WithUser stores a resolved user in the context. It is exported for tests
// and for middleware composition outside this package.
func WithUser(ctx context.Context, u *User) context.Context { return withUser(ctx, u) }

// Authenticator resolves the current user for a request. Implementations:
// OIDCAuth (Keycloak), ForwardAuth (proxy headers), LocalDev.
type Authenticator interface {
	// Middleware resolves the user and stores it in the request context.
	// It may redirect unauthenticated browser requests to a login flow.
	Middleware(next http.Handler) http.Handler
	// Routes registers mode-specific endpoints (e.g. /login, /callback).
	Routes(mux *http.ServeMux)
	// LogoutURL returns where to send users that log out.
	LogoutURL() string
}

// AdminMatcher decides the admin flag from a claim value and group list.
type AdminMatcher struct {
	ClaimName string
	Groups    []string
}

// IsAdmin reports whether the claim value grants admin rights. The claim
// may be a string, a list of strings, or a list of nested objects with a
// name/role field (as Keycloak realm roles sometimes arrive).
func (m *AdminMatcher) IsAdmin(claimValue any) bool {
	return IsAdminClaim(claimValue, m.Groups)
}

// IsAdminClaim is the pure claim-matching logic used by all modes.
func IsAdminClaim(claimValue any, adminGroups []string) bool {
	if len(adminGroups) == 0 {
		return false
	}
	values := flattenClaim(claimValue)
	for _, v := range values {
		for _, g := range adminGroups {
			if strings.EqualFold(strings.TrimSpace(v), g) {
				return true
			}
		}
	}
	return false
}

// flattenClaim converts arbitrary claim JSON shapes into a list of strings.
func flattenClaim(v any) []string {
	switch val := v.(type) {
	case nil:
		return nil
	case string:
		// Comma-separated claim strings are common in proxy headers.
		parts := strings.Split(val, ",")
		out := make([]string, 0, len(parts))
		for _, p := range parts {
			p = strings.TrimSpace(p)
			if p != "" {
				out = append(out, p)
			}
		}
		return out
	case []string:
		return val
	case []any:
		var out []string
		for _, item := range val {
			out = append(out, flattenClaim(item)...)
		}
		return out
	case map[string]any:
		// Keycloak role objects: {"name": "..."} or {"role": "..."}.
		for _, key := range []string{"name", "role", "group"} {
			if inner, ok := val[key]; ok {
				return flattenClaim(inner)
			}
		}
		return nil
	default:
		return nil
	}
}

// AdminFromHeaders computes the admin flag from forward-auth headers.
func AdminFromHeaders(groups string, adminGroups []string) bool {
	return IsAdminClaim(groups, adminGroups)
}

// LocalDev treats every request as an authenticated admin. Only for local
// development; production deployments must never enable it.
type LocalDev struct{}

func (LocalDev) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u := &User{Sub: "localdev", Email: "dev@localhost", Name: "Local Dev", Admin: true}
		next.ServeHTTP(w, r.WithContext(withUser(r.Context(), u)))
	})
}

func (LocalDev) Routes(mux *http.ServeMux) {}

func (LocalDev) LogoutURL() string { return "/" }

// ForwardAuth trusts headers injected by an authenticated reverse proxy.
// The app must only be reachable through that proxy.
type ForwardAuth struct {
	Headers HeaderNames
	Admin   AdminMatcher
}

// HeaderNames holds the configured forward-auth header names.
type HeaderNames struct {
	User   string
	Email  string
	Groups string
}

func (f *ForwardAuth) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sub := strings.TrimSpace(r.Header.Get(f.Headers.User))
		if sub == "" {
			// Not authenticated by the proxy: deny.
			http.Error(w, "authentication required (forward-auth header missing)", http.StatusUnauthorized)
			return
		}
		email := strings.TrimSpace(r.Header.Get(f.Headers.Email))
		groups := r.Header.Get(f.Headers.Groups)
		u := &User{
			Sub:    sub,
			Email:  email,
			Name:   sub,
			Groups: flattenClaim(groups),
			Admin:  AdminFromHeaders(groups, f.Admin.Groups),
		}
		next.ServeHTTP(w, r.WithContext(withUser(r.Context(), u)))
	})
}

func (*ForwardAuth) Routes(mux *http.ServeMux) {}

func (*ForwardAuth) LogoutURL() string { return "/" }

// RequireAdmin allows the request through only for admins.
func RequireAdmin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, ok := UserFromContext(r.Context())
		if !ok || !u.Admin {
			http.Error(w, "admin privileges required", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// RequireUser allows the request through for any authenticated user.
func RequireUser(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := UserFromContext(r.Context()); !ok {
			http.Error(w, "authentication required", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}
