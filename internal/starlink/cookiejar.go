package starlink

import (
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// jarEntry is one stored cookie.
type jarEntry struct {
	value    string
	domain   string // host or ".host" form
	path     string
	secure   bool
	httpOnly bool
	expires  time.Time // zero = session cookie
}

// cookieJar is a small thread-safe http.CookieJar. Unlike net/http/cookiejar
// it stores cookies with their raw domain attribute so that cookies set for
// ".starlink.com" are also sent to "api.starlink.com" and vice versa.
type cookieJar struct {
	mu      sync.Mutex
	cookies map[string]map[string]jarEntry // host -> name -> entry
}

func newCookieJar() *cookieJar {
	return &cookieJar{cookies: map[string]map[string]jarEntry{}}
}

// domainMatches reports whether cookie domain d applies to host h
// (RFC 6265 style suffix matching).
func domainMatches(d, h string) bool {
	d = strings.ToLower(strings.TrimPrefix(d, "."))
	h = strings.ToLower(h)
	return h == d || strings.HasSuffix(h, "."+d)
}

func (j *cookieJar) SetCookies(u *url.URL, cookies []*http.Cookie) {
	j.mu.Lock()
	defer j.mu.Unlock()
	host := strings.ToLower(u.Hostname())
	for _, c := range cookies {
		if c.Value == "" {
			continue
		}
		domain := c.Domain
		if domain == "" {
			domain = host
		}
		if !domainMatches(domain, host) {
			continue
		}
		path := c.Path
		if path == "" {
			path = "/"
		}
		byHost, ok := j.cookies[host]
		if !ok {
			byHost = map[string]jarEntry{}
			j.cookies[host] = byHost
		}
		if c.MaxAge < 0 {
			delete(byHost, c.Name)
			continue
		}
		expires := time.Time{}
		if c.Expires.After(time.Time{}) {
			expires = c.Expires
		}
		byHost[c.Name] = jarEntry{
			value:    c.Value,
			domain:   domain,
			path:     path,
			secure:   c.Secure,
			httpOnly: c.HttpOnly,
			expires:  expires,
		}
	}
}

func (j *cookieJar) Cookies(u *url.URL) []*http.Cookie {
	j.mu.Lock()
	defer j.mu.Unlock()
	host := strings.ToLower(u.Hostname())
	path := u.Path
	if path == "" {
		path = "/"
	}
	secure := u.Scheme == "https"
	now := time.Now()

	// Collect cookies across all hosts whose domain matches.
	var out []*http.Cookie
	for _, byName := range j.cookies {
		names := make([]string, 0, len(byName))
		for name := range byName {
			names = append(names, name)
		}
		sortStrings(names)
		for _, name := range names {
			e := byName[name]
			if !e.expires.IsZero() && now.After(e.expires) {
				continue
			}
			if !domainMatches(e.domain, host) {
				continue
			}
			if !strings.HasPrefix(path, e.path) {
				continue
			}
			if e.secure && !secure {
				continue
			}
			out = append(out, &http.Cookie{Name: name, Value: e.value})
		}
	}
	return out
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

// setSSOCookie installs the durable Starlink.Com.Sso cookie into the jar for
// the starlink.com domain, mirroring the browser cookie.
func (j *cookieJar) setSSOCookie(host, value string) {
	j.mu.Lock()
	defer j.mu.Unlock()
	byHost, ok := j.cookies[host]
	if !ok {
		byHost = map[string]jarEntry{}
		j.cookies[host] = byHost
	}
	byHost["Starlink.Com.Sso"] = jarEntry{
		value:  value,
		domain: host,
		path:   "/",
		secure: true,
	}
}

// snapshot returns a copy of all stored cookies (for tests and debugging).
func (j *cookieJar) snapshot() map[string]map[string]string {
	j.mu.Lock()
	defer j.mu.Unlock()
	out := map[string]map[string]string{}
	for host, byName := range j.cookies {
		m := map[string]string{}
		for name, e := range byName {
			m[name] = e.value
		}
		out[host] = m
	}
	return out
}
