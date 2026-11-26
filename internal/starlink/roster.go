package starlink

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// ServiceLine is one service line from the account roster with its nested
// user terminals and routers.
type ServiceLine struct {
	Number    string
	Nickname  string
	Terminals []*TerminalInfo
	Routers   []*RouterInfo

	Subscription    *Subscription
	CanPauseService bool
}

// TerminalInfo describes a dish (user terminal) as reported by the roster.
type TerminalInfo struct {
	UserTerminalID   string
	SerialNumber     string
	DishSerialNumber string
	LocationNickname string
	Lat, Lon         float64
	Active           bool
	IsOffline        bool
	LastConnected    time.Time
	LastDisconnected time.Time
	ServiceLine      string
	Present          map[string]bool
}

// RouterInfo describes a router as reported by the roster.
type RouterInfo struct {
	RouterID        string
	HardwareVersion string
	IsBypassed      bool
	ServiceLine     string
	Present         map[string]bool
}

// Roster fetches and caches the account's service lines. The cached roster
// expires after ttl (default ~5 minutes).
type Roster struct {
	session *Session
	apiBase string
	log     *slog.Logger
	ttl     time.Duration

	mu      sync.Mutex
	lines   []*ServiceLine
	fetched time.Time
	err     error

	// lastErr is readable without r.mu, which is held across fetches.
	lastErr atomic.Pointer[string]
}

// LastError returns the most recent roster refresh error, or "" once a
// refresh has succeeded since.
func (r *Roster) LastError() string {
	if r == nil {
		return ""
	}
	if p := r.lastErr.Load(); p != nil {
		return *p
	}
	return ""
}

// NewRoster creates a roster client.
func NewRoster(session *Session, apiBase string, ttl time.Duration, log *slog.Logger) *Roster {
	if ttl <= 0 {
		ttl = 5 * time.Minute
	}
	return &Roster{
		session: session,
		apiBase: strings.TrimRight(apiBase, "/"),
		log:     log,
		ttl:     ttl,
	}
}

// Invalidate forces the next Get to refetch (e.g. after a plan change).
func (r *Roster) Invalidate() {
	r.mu.Lock()
	r.fetched = time.Time{}
	r.mu.Unlock()
}

// Get returns the cached roster, refreshing it when stale. Pagination walks
// pages of 100 service lines until isLastPage.
func (r *Roster) Get(ctx context.Context) ([]*ServiceLine, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.lines != nil && time.Since(r.fetched) < r.ttl && r.err == nil {
		return r.lines, nil
	}
	lines, err := r.fetch(ctx)
	if err != nil {
		msg := err.Error()
		r.lastErr.Store(&msg)
		if r.lines != nil {
			// Serve stale data rather than failing the whole tick.
			r.log.Warn("roster refresh failed; serving cached roster", "err", err)
			return r.lines, nil
		}
		return nil, err
	}
	r.lines = lines
	r.fetched = time.Now()
	r.err = nil
	r.lastErr.Store(nil)
	// A successful authenticated roster fetch proves the session is alive.
	if r.session != nil {
		r.session.MarkAlive()
	}
	return lines, nil
}

func (r *Roster) fetch(ctx context.Context) ([]*ServiceLine, error) {
	var out []*ServiceLine
	for page := 0; page < 50; page++ { // hard cap: 5000 service lines
		u := fmt.Sprintf("%s/webagg/v2/accounts/service-lines?limit=100&page=%d&isConverting=false&onlyActive=false&searchString=&onlyNoUts=false", r.apiBase, page)
		_, data, err := r.session.request(ctx, "GET", u, nil, "")
		if err != nil {
			return nil, fmt.Errorf("fetching service lines page %d: %w", page, err)
		}
		// Response envelope: {"content":{"totalCount":..,"results":[...]},"errors":..,"isValid":..}.
		// Older raw-array shapes are still accepted for robustness.
		var env struct {
			Content struct {
				TotalCount int              `json:"totalCount"`
				IsLastPage bool             `json:"isLastPage"`
				Results    []map[string]any `json:"results"`
			} `json:"content"`
		}
		var raw []map[string]any
		if err := json.Unmarshal(data, &env); err == nil {
			raw = env.Content.Results
		} else {
			if err := json.Unmarshal(data, &raw); err != nil {
				return nil, fmt.Errorf("service lines page %d: %w", page, err)
			}
		}
		for _, entry := range raw {
			out = append(out, parseServiceLine(entry))
		}
		if env.Content.IsLastPage || len(raw) < 100 {
			break
		}
	}
	return out, nil
}

func parseServiceLine(m map[string]any) *ServiceLine {
	sl := &ServiceLine{
		Number:          mapString(m, "serviceLineNumber", "serviceLine"),
		Nickname:        mapString(m, "nickname", "name"),
		CanPauseService: mapBool(m, "canPauseService"),
	}
	if sub, ok := lookup(m, "subscription"); ok {
		if sm, ok := sub.(map[string]any); ok {
			sl.Subscription = parseSubscription(sm)
		}
	}
	if uts, ok := mapSlice(m, "userTerminals", "user_terminals"); ok {
		for _, ut := range uts {
			sl.Terminals = append(sl.Terminals, parseTerminal(ut, sl.Number))
		}
	}
	if rs, ok := mapSlice(m, "routers"); ok {
		for _, rt := range rs {
			sl.Routers = append(sl.Routers, parseRouter(rt, sl.Number))
		}
	}
	return sl
}

func parseTerminal(m map[string]any, serviceLine string) *TerminalInfo {
	t := &TerminalInfo{
		UserTerminalID:   mapString(m, "userTerminalId", "id"),
		SerialNumber:     mapString(m, "serialNumber", "kitSerialNumber"),
		DishSerialNumber: mapString(m, "dishSerialNumber"),
		LocationNickname: mapString(m, "locationNickname"),
		Active:           mapBool(m, "active"),
		IsOffline:        mapBool(m, "isOffline"),
		ServiceLine:      serviceLine,
		Present:          map[string]bool{},
	}
	t.Lat = mapFloat(m, "lat", "latitude")
	t.Lon = mapFloat(m, "lon", "longitude")
	t.LastConnected = mapTime(m, "lastConnected")
	t.LastDisconnected = mapTime(m, "lastDisconnected")
	for key, aliases := range map[string][]string{"active": {"active"}, "isOffline": {"isOffline"}, "lat": {"lat", "latitude"}, "lon": {"lon", "longitude"}} {
		value, ok := lookup(m, aliases...)
		t.Present[key] = ok && value != nil
	}
	return t
}

func parseRouter(m map[string]any, serviceLine string) *RouterInfo {
	value, bypassed := lookup(m, "isBypassed")
	return &RouterInfo{
		RouterID:        mapString(m, "routerId", "id"),
		HardwareVersion: mapString(m, "hardwareVersion"),
		IsBypassed:      mapBool(m, "isBypassed"),
		ServiceLine:     serviceLine,
		Present:         map[string]bool{"isBypassed": bypassed && value != nil},
	}
}

// Lenient map helpers for the webagg payloads (field casing varies between
// API generations; lookups are tolerant and case-insensitive).

func lookup(m map[string]any, keys ...string) (any, bool) {
	for _, key := range keys {
		if v, ok := m[key]; ok {
			return v, true
		}
	}
	// Case-insensitive fallback.
	lower := map[string]any{}
	for k, v := range m {
		lower[strings.ToLower(k)] = v
	}
	for _, key := range keys {
		if v, ok := lower[strings.ToLower(key)]; ok {
			return v, true
		}
	}
	return nil, false
}

func mapString(m map[string]any, keys ...string) string {
	v, ok := lookup(m, keys...)
	if !ok {
		return ""
	}
	switch val := v.(type) {
	case string:
		return val
	case nil:
		return ""
	default:
		return fmt.Sprintf("%v", val)
	}
}

func mapBool(m map[string]any, keys ...string) bool {
	v, ok := lookup(m, keys...)
	if !ok {
		return false
	}
	switch val := v.(type) {
	case bool:
		return val
	case string:
		b, _ := strconv.ParseBool(val)
		return b
	case float64:
		return val != 0
	default:
		return false
	}
}

func mapFloat(m map[string]any, keys ...string) float64 {
	v, ok := lookup(m, keys...)
	if !ok {
		return 0
	}
	switch val := v.(type) {
	case float64:
		return finite64(val)
	case int64:
		return float64(val)
	case int:
		return float64(val)
	case string:
		f, err := strconv.ParseFloat(val, 64)
		if err != nil {
			return 0
		}
		return finite64(f)
	default:
		return 0
	}
}

// finite64 returns v unless it is NaN or ±Inf (which json.Marshal rejects
// and which would make every store save fail); those become 0.
func finite64(v float64) float64 {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return 0
	}
	return v
}

func mapTime(m map[string]any, keys ...string) time.Time {
	v, ok := lookup(m, keys...)
	if !ok {
		return time.Time{}
	}
	switch val := v.(type) {
	case string:
		if val == "" {
			return time.Time{}
		}
		if t, err := time.Parse(time.RFC3339, val); err == nil {
			return t
		}
		return time.Time{}
	case float64:
		return time.Unix(int64(val), 0)
	default:
		return time.Time{}
	}
}

func mapSlice(m map[string]any, keys ...string) ([]map[string]any, bool) {
	v, ok := lookup(m, keys...)
	if !ok {
		return nil, false
	}
	arr, ok := v.([]any)
	if !ok {
		return nil, false
	}
	out := make([]map[string]any, 0, len(arr))
	for _, item := range arr {
		if m, ok := item.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out, true
}
