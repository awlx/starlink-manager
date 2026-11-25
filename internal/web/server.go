// Package web serves the server-rendered UI and the JSON API of the
// FFMUC Starlink manager. Templates and static assets are embedded so the
// app ships as a single static binary.
package web

import (
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/awlx/starlink-manager/api/device"
	"github.com/awlx/starlink-manager/internal/auth"
	"github.com/awlx/starlink-manager/internal/config"
	"github.com/awlx/starlink-manager/internal/starlink"
)

//go:embed templates/*.html static/*
var embedded embed.FS

// Server wires the Starlink clients, the auth middleware and the templates.
type Server struct {
	cfg     *config.Config
	auth    auth.Authenticator
	gateway *starlink.Gateway
	roster  *starlink.Roster
	store   *starlink.Store
	session *starlink.Session
	pages   map[string]*template.Template
}

// New builds the web server and parses all templates eagerly so template
// errors surface at startup instead of on the first request.
func New(cfg *config.Config, authN auth.Authenticator, gateway *starlink.Gateway,
	roster *starlink.Roster, store *starlink.Store, session *starlink.Session) (*Server, error) {
	s := &Server{
		cfg:     cfg,
		auth:    authN,
		gateway: gateway,
		roster:  roster,
		store:   store,
		session: session,
		pages:   map[string]*template.Template{},
	}
	base, err := template.ParseFS(embedded, "templates/base.html")
	if err != nil {
		return nil, fmt.Errorf("parse base template: %w", err)
	}
	for _, page := range []string{"dashboard", "device", "router", "error"} {
		t, err := base.Clone()
		if err != nil {
			return nil, err
		}
		t, err = t.ParseFS(embedded, "templates/"+page+".html")
		if err != nil {
			return nil, fmt.Errorf("parse template %s: %w", page, err)
		}
		s.pages[page] = t
	}
	return s, nil
}

// Handler builds the full HTTP handler tree. /healthz is intentionally
// unauthenticated so the container health check works without Keycloak.
func (s *Server) Handler() http.Handler {
	// The embedded FS root contains templates/ and static/; serve the
	// static/ subtree directly so /static/style.css resolves.
	staticFS, err := fs.Sub(embedded, "static")
	if err != nil {
		panic(fmt.Sprintf("embed static/: %v", err))
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.handleHealthz)
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServer(http.FS(staticFS))))
	// Browsers request /favicon.ico even for SVG icons; serve the
	// embedded SVG there with the right content type.
	mux.HandleFunc("GET /favicon.ico", func(w http.ResponseWriter, r *http.Request) {
		data, err := staticFS.Open("favicon.svg")
		if err != nil {
			http.NotFound(w, r)
			return
		}
		defer data.Close()
		w.Header().Set("Content-Type", "image/svg+xml")
		w.Header().Set("Cache-Control", "public, max-age=86400")
		if rs, ok := data.(io.ReadSeeker); ok {
			_, _ = rs.Seek(0, io.SeekStart)
		}
		_, _ = io.Copy(w, data)
	})
	mux.Handle("GET /api/device/{$}", http.NotFoundHandler())

	s.auth.Routes(mux)

	protected := http.NewServeMux()
	protected.HandleFunc("GET /{$}", s.handleDashboard)
	protected.HandleFunc("GET /device/{id}", s.handleDevicePage)
	protected.HandleFunc("GET /api/summary", s.handleSummary)
	protected.HandleFunc("GET /api/device/{id}/history", s.handleHistory)
	protected.HandleFunc("GET /api/device/{id}/clients", s.handleClients)
	protected.Handle("POST /api/device/{id}/action", auth.RequireAdmin(http.HandlerFunc(s.handleAction)))

	mux.Handle("/", s.auth.Middleware(protected))
	return mux
}

// ---------- template data ----------

type pageInfo struct {
	Title         string
	User          *auth.User
	LogoutURL     string
	SessionDead   bool
	AccountNumber string
}

type DeviceView struct {
	ID                string
	Kind              string
	KindLabel         string
	DisplayName       string
	Nickname          string
	SerialNumber      string
	ServiceLineName   string
	OnlineClass       string
	OnlineLabel       string
	LastSeenText      string
	StateLabel        string
	SNRText           string
	ObstructionText   string
	ObstructionClass  string
	ObstructionBarPct int
	VersionsText      string
	Alerts            []string
	HealthClass       string
	HealthLabel       string
	PingText          string
	DropText          string
	DownlinkText      string
	UplinkText        string
}

type statusRow struct {
	Label string
	Field string
	Value string
}

type windowOpt struct {
	Label    string
	Value    string
	Selected bool
}

type chartOpt struct {
	Key   string
	Title string
}

type devicePageData struct {
	pageInfo
	Device     DeviceView
	AdminUser  bool
	Windows    []windowOpt
	Charts     []chartOpt
	StatusRows []statusRow
}

type dashboardData struct {
	pageInfo
	Devices         []DeviceView
	LastRosterError string
	OnlineCount     int
	DegradedCount   int
	OfflineCount    int
}

type errorData struct {
	pageInfo
	ErrorCode    string
	ErrorMessage string
}

func (s *Server) render(w http.ResponseWriter, page string, data any) {
	t, ok := s.pages[page]
	if !ok {
		http.Error(w, "template missing", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := t.ExecuteTemplate(w, page, data); err != nil {
		s.logf("render %s failed: %v", page, err)
	}
}

func (s *Server) logf(format string, args ...any) {
	slog.Warn(fmt.Sprintf(format, args...))
}

func (s *Server) baseInfo(r *http.Request) pageInfo {
	u, _ := auth.UserFromContext(r.Context())
	return pageInfo{
		Title:         "Starlink Manager",
		User:          u,
		LogoutURL:     s.auth.LogoutURL(),
		SessionDead:   s.sessionDead(),
		AccountNumber: s.accountNumber(),
	}
}

// sessionDead is nil-safe so handlers work before the session is wired
// (e.g. unit tests with no live Starlink session).
func (s *Server) sessionDead() bool {
	if s.session == nil {
		return false
	}
	return s.session.SessionDead()
}

func (s *Server) accountNumber() string {
	if s.session == nil {
		return ""
	}
	return s.session.AccountNumber()
}

// ---------- device view assembly ----------

// defaultOnlineTTL applies when STARLINK_TELEMETRY_ONLINE_TTL is unset.
const defaultOnlineTTL = 5 * time.Minute

// isOnline reports whether the device answered a poll within the
// configured TTL (STARLINK_TELEMETRY_ONLINE_TTL).
func (s *Server) isOnline(d *starlink.Device, now time.Time) bool {
	ttl := defaultOnlineTTL
	if s.cfg != nil && s.cfg.TelemetryTTLOnline > 0 {
		ttl = s.cfg.TelemetryTTLOnline
	}
	return !d.LastSeen.IsZero() && now.Sub(d.LastSeen) < ttl
}

func (s *Server) deviceView(d *starlink.Device, now time.Time) DeviceView {
	v := DeviceView{
		ID:           d.ID,
		Kind:         string(d.Kind),
		DisplayName:  d.Name,
		Nickname:     nicknameOf(d),
		SerialNumber: d.Serial,
		PingText:     "—",
		DropText:     "—",
		DownlinkText: "—",
		UplinkText:   "—",
	}
	v.KindLabel = "Dish"
	v.ServiceLineName = d.ServiceLine
	if d.Kind == starlink.KindRouter {
		v.KindLabel = "Router"
	}
	if d.DishSerial != "" && d.Serial == "" {
		v.SerialNumber = d.DishSerial
	}

	if s.isOnline(d, now) {
		v.OnlineClass = "pill-ok"
		v.OnlineLabel = "online"
	} else {
		v.OnlineClass = "pill-error"
		v.OnlineLabel = "unreachable"
	}
	if d.LastSeen.IsZero() {
		v.LastSeenText = "never"
	} else {
		v.LastSeenText = now.Sub(d.LastSeen).Round(time.Second).String() + " ago"
	}

	if st := d.Status; st != nil {
		v.StateLabel = st.State
		if v.StateLabel == "" {
			v.StateLabel = "not reported"
		}
		v.SNRText = snrText(st.SNR)
		if st.ObstructionFraction > 0 || st.CurrentlyObstructed {
			v.ObstructionText = fmt.Sprintf("%.1f%%", st.ObstructionFraction*100)
			if st.CurrentlyObstructed {
				v.ObstructionText += " (obstructed now)"
			}
		} else {
			v.ObstructionText = "clear"
		}
		v.ObstructionClass = obstructionClass(st)
		v.ObstructionBarPct = obstructionBarPct(st.ObstructionFraction)
		v.VersionsText = strings.TrimSpace(st.SoftwareVersion + " / " + st.HardwareVersion)
		if v.VersionsText == "/" {
			// The dish firmware sometimes omits the software version;
			// fall back to the cloud telemetry enrichment.
			if sv := telemetryString(d.Telemetry, "softwareVersion"); sv != "" {
				v.VersionsText = sv
			}
		}
		v.Alerts = activeAlerts(st.Alerts)
		if d.Kind == starlink.KindRouter {
			if st.Wifi != nil {
				v.PingText = msText(st.Wifi.PingLatencyMs)
				v.DropText = pctText(st.Wifi.PingDropRate)
			}
		} else {
			v.PingText = msText(st.PopPingLatencyMs)
			v.DropText = pctText(st.PopPingDropRate)
			v.DownlinkText = compactBpsText(st.DownlinkThroughput)
			v.UplinkText = compactBpsText(st.UplinkThroughput)
		}
	}
	if v.StateLabel == "" {
		v.StateLabel = "unknown"
	}
	if v.SNRText == "" {
		v.SNRText = "—"
	}
	if v.ObstructionText == "" {
		v.ObstructionText = "—"
	}
	if v.VersionsText == "" {
		v.VersionsText = "—"
	}
	// Health is presentation-only; API reachability semantics stay unchanged.
	v.HealthClass, v.HealthLabel = v.OnlineClass, v.OnlineLabel
	if v.OnlineClass == "pill-ok" && (len(v.Alerts) > 0 || v.ObstructionClass == "pill-warn" || v.ObstructionClass == "pill-error") {
		v.HealthClass, v.HealthLabel = "pill-warn", "degraded"
	}
	return v
}

func compactBpsText(v float64) string {
	units := []string{"bps", "Kbps", "Mbps", "Gbps"}
	i := 0
	for v >= 1000 && i < len(units)-1 {
		v /= 1000
		i++
	}
	if i == 0 {
		return fmt.Sprintf("%.0f %s", v, units[i])
	}
	return fmt.Sprintf("%.1f %s", v, units[i])
}

func nicknameOf(d *starlink.Device) string {
	// The store's Name already includes the roster nickname override when
	// one is configured; the raw roster value is kept for the detail page.
	return ""
}

func alertLabel(key string) string {
	switch key {
	case "motors_stuck":
		return "motors stuck"
	case "thermal_shutdown":
		return "thermal shutdown"
	case "thermal_throttle":
		return "thermal throttling"
	case "unexpected_location":
		return "unexpected location"
	case "mast_not_near_vertical":
		return "mast not near vertical"
	case "slow_ethernet_speeds":
		return "slow ethernet speeds"
	case "obstructed":
		return "obstructed"
	default:
		return strings.ReplaceAll(strings.TrimPrefix(key, "is"), "_", " ")
	}
}

// ---------- pages ----------

func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	now := time.Now()
	var devs []DeviceView
	for _, d := range sortedDevices(s.store) {
		devs = append(devs, s.deviceView(d, now))
	}
	data := dashboardData{
		pageInfo:        s.baseInfo(r),
		Devices:         devs,
		LastRosterError: s.roster.LastError(),
	}
	data.Title = "Fleet overview"
	for _, d := range devs {
		switch d.HealthClass {
		case "pill-ok":
			data.OnlineCount++
		case "pill-warn":
			data.DegradedCount++
		default:
			data.OfflineCount++
		}
	}
	s.render(w, "dashboard", data)
}

func sortedDevices(store *starlink.Store) []*starlink.Device {
	devs := store.Devices()
	// deterministic order: dishes before routers, then by name, then by ID
	sort.Slice(devs, func(i, j int) bool {
		a, b := devs[i], devs[j]
		if a.Kind != b.Kind {
			return a.Kind == starlink.KindDish
		}
		if a.Name != b.Name {
			return a.Name < b.Name
		}
		return a.ID < b.ID
	})
	return devs
}

var historyWindows = []struct {
	Label string
	Value time.Duration
}{
	{"15m", 15 * time.Minute},
	{"1h", time.Hour},
	{"6h", 6 * time.Hour},
	{"24h", 24 * time.Hour},
	{"3d", 72 * time.Hour},
}

func availableWindows(cfgWindow time.Duration) []windowOpt {
	var out []windowOpt
	for _, w := range historyWindows {
		if w.Value <= cfgWindow {
			out = append(out, windowOpt{Label: w.Label, Value: w.Value.String()})
		}
	}
	if len(out) > 0 {
		out[len(out)-1].Selected = true
	}
	return out
}

func (s *Server) deviceOr404(w http.ResponseWriter, r *http.Request) (*starlink.Device, bool) {
	id := r.PathValue("id")
	d, ok := s.store.Device(id)
	if !ok {
		s.renderError(w, r, http.StatusNotFound, "Unknown device", "No device with ID "+id+" is configured on this account.")
		return nil, false
	}
	return d, true
}

// deviceForAPI looks up the {id} device for JSON endpoints and answers a
// JSON 404 itself; deviceOr404 renders an HTML page and must not be mixed
// with writeJSON.
func (s *Server) deviceForAPI(w http.ResponseWriter, r *http.Request) (*starlink.Device, bool) {
	d, ok := s.store.Device(r.PathValue("id"))
	if !ok {
		writeJSON(w, http.StatusNotFound, apiError{"unknown device"})
		return nil, false
	}
	return d, true
}

func (s *Server) renderError(w http.ResponseWriter, r *http.Request, code int, title, msg string) {
	w.WriteHeader(code)
	s.render(w, "error", errorData{
		pageInfo:     s.baseInfo(r),
		ErrorCode:    title,
		ErrorMessage: msg,
	})
}

func (s *Server) handleDevicePage(w http.ResponseWriter, r *http.Request) {
	d, ok := s.deviceOr404(w, r)
	if !ok {
		return
	}
	now := time.Now()
	u, _ := auth.UserFromContext(r.Context())
	data := devicePageData{
		pageInfo:  s.baseInfo(r),
		Device:    s.deviceView(d, now),
		AdminUser: u != nil && u.Admin,
		Windows:   availableWindows(s.cfg.HistoryWindow),
		Charts: []chartOpt{
			{Key: "ping", Title: "Ping latency (ms)"},
			{Key: "drop", Title: "Ping drop rate (%)"},
			{Key: "downlink", Title: "Downlink throughput"},
			{Key: "uplink", Title: "Uplink throughput"},
			{Key: "obstruction", Title: "Obstruction"},
			{Key: "scheduled", Title: "Scheduled (boresight) windows"},
		},
		StatusRows: s.statusRows(d),
	}
	data.Title = d.Name
	page := "device"
	if d.Kind == starlink.KindRouter {
		page = "router"
	}
	s.render(w, page, data)
}

func (s *Server) statusRows(d *starlink.Device) []statusRow {
	var rows []statusRow
	st := d.Status
	if st == nil {
		return []statusRow{{Label: "Status", Field: "state", Value: "unreachable — no data yet"}}
	}
	add := func(label, field, value string) { rows = append(rows, statusRow{label, field, value}) }

	if d.Kind == starlink.KindRouter {
		if st.Wifi != nil {
			add("State", "state", orDash(st.State))
			add("Software version", "versions", orDash(st.SoftwareVersion))
			add("Hardware version", "hw", orDash(st.HardwareVersion))
			add("WAN address", "wan", orDash(st.Wifi.IPv4WANAddress))
			add("Ping latency", "ping", orDash(msText(st.Wifi.PingLatencyMs)))
			add("Ping drop rate", "drop", orDash(pctText(st.Wifi.PingDropRate)))
		}
	} else {
		add("State", "state", orDash(st.State))
		add("Software version", "versions",
			orDash(firstNonEmpty(st.SoftwareVersion, telemetryString(d.Telemetry, "softwareVersion"))))
		add("Hardware version", "hw", orDash(st.HardwareVersion))
		add("SNR", "snr", snrText(st.SNR))
		add("Signal quality", "signalQuality", orDash(telemetryQualityText(d.Telemetry)))
		add("Ping latency", "ping", orDash(msText(st.PopPingLatencyMs)))
		add("Ping drop rate", "drop", orDash(pctText(st.PopPingDropRate)))
		add("Downlink", "down", orDash(bpsText(st.DownlinkThroughput)))
		add("Uplink", "up", orDash(bpsText(st.UplinkThroughput)))
		add("Currently obstructed", "obstructedNow", yesNo(st.CurrentlyObstructed))
		add("Obstruction validity", "obstructionValid", orDash(secondsText(st.ObstructionValidS)))
		add("Obstructed (last 24h)", "obstruction24h", orDash(secondsText(st.ObstructionLast24hS)))
		add("Scheduled (last sample)", "scheduledFlag", yesNo(s.lastScheduled(d)))
		add("Stow requested", "stow", yesNo(st.StowRequested))
		add("Uptime", "uptime", orDash(uptimeText(st, d.Telemetry)))
		if alerts := activeAlerts(st.Alerts); len(alerts) > 0 {
			add("Alerts", "alerts", strings.Join(alerts, ", "))
		}
	}
	add("Last seen", "updated", lastSeenText(d))
	return rows
}

func orDash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "—"
	}
	return s
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// telemetryString reads a string field from the cloud telemetry enrichment
// map stored per device.
func telemetryString(tm map[string]any, key string) string {
	if tm == nil {
		return ""
	}
	if v, ok := tm[key].(string); ok {
		return v
	}
	return ""
}

// telemetryQualityText renders the signal quality from the telemetry
// enrichment (0..1 scale).
func telemetryQualityText(tm map[string]any) string {
	if tm == nil {
		return ""
	}
	v, ok := tm["signalQuality"].(float64)
	if !ok || v <= 0 {
		return ""
	}
	return fmt.Sprintf("%.2f", v)
}

// uptimeText prefers the gateway-reported uptime and falls back to the
// telemetry enrichment value (cloud-only devices).
func uptimeText(st *starlink.DeviceStatus, tm map[string]any) string {
	if st.UptimeS > 0 {
		return durationText(st.UptimeS)
	}
	if tm == nil {
		return ""
	}
	if v, ok := tm["uptimeSeconds"].(float64); ok && v > 0 {
		return durationText(uint64(v))
	}
	return ""
}
func msText(v float64) string  { return fmt.Sprintf("%.0f ms", v) }
func pctText(v float64) string { return fmt.Sprintf("%.1f %%", v*100) }
func bpsText(v float64) string { return fmt.Sprintf("%.0f bps", v) }
func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}
func durationText(s uint64) string {
	d := time.Duration(s) * time.Second
	return d.Round(time.Second).String()
}

// lastScheduled reports whether the most recent history sample was taken
// during a scheduled (boresight) maintenance window. Device snapshots from
// the store carry no samples, so read the history directly.
func (s *Server) lastScheduled(d *starlink.Device) bool {
	samples := s.store.History(d.ID, 15*time.Minute)
	if len(samples) == 0 {
		return false
	}
	return samples[len(samples)-1].Scheduled
}

func snrText(v *float64) string {
	if v == nil {
		return "—"
	}
	return fmt.Sprintf("%.1f", *v)
}

// activeAlerts returns the labels of the active alerts in a stable order.
func activeAlerts(m map[string]bool) []string {
	var out []string
	for name, active := range m {
		if active {
			out = append(out, alertLabel(name))
		}
	}
	sort.Strings(out)
	return out
}
func obstructionText(st *starlink.DeviceStatus) string {
	if st.ObstructionFraction <= 0 && !st.CurrentlyObstructed {
		return "clear"
	}
	t := fmt.Sprintf("%.1f %%", st.ObstructionFraction*100)
	if st.CurrentlyObstructed {
		t += " (now)"
	}
	return t
}

// obstructionClass picks the severity colour for the obstruction badge:
// green below 2%, amber between 2% and 10%, red above 10% or while an
// obstruction is active right now.
func obstructionClass(st *starlink.DeviceStatus) string {
	switch {
	case st.CurrentlyObstructed:
		return "pill-error"
	case st.ObstructionFraction*100 < 2:
		return "pill-ok"
	case st.ObstructionFraction*100 <= 10:
		return "pill-warn"
	default:
		return "pill-error"
	}
}

// obstructionBarPct returns the fill percentage for the obstruction bar,
// capped at 100.
func obstructionBarPct(frac float64) int {
	pct := int(frac * 100)
	if pct > 100 {
		pct = 100
	}
	return pct
}

// secondsText renders a fractional-seconds duration, or "" for zero.
func secondsText(v float64) string {
	if v <= 0 {
		return ""
	}
	return durationText(uint64(v))
}
func lastSeenText(d *starlink.Device) string {
	if d.LastSeen.IsZero() {
		return "never"
	}
	return time.Since(d.LastSeen).Round(time.Second).String() + " ago"
}

// ---------- JSON API ----------

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

type apiError struct {
	Error string `json:"error"`
}

func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"status":      "ok",
		"sessionDead": s.sessionDead(),
		"time":        time.Now().UTC().Format(time.RFC3339),
	})
}

type summaryDevice struct {
	ID               string   `json:"id"`
	OnlineLabel      string   `json:"onlineLabel,omitempty"`
	OnlineClass      string   `json:"onlineClass,omitempty"`
	State            string   `json:"state,omitempty"`
	SNR              string   `json:"snr,omitempty"`
	Obstruction      string   `json:"obstruction,omitempty"`
	ObstructionClass string   `json:"obstructionClass,omitempty"`
	Versions         string   `json:"versions,omitempty"`
	Alerts           []string `json:"alerts,omitempty"`
	Updated          string   `json:"updated,omitempty"`
}

func (s *Server) handleSummary(w http.ResponseWriter, r *http.Request) {
	now := time.Now()
	devs := []summaryDevice{}
	for _, d := range sortedDevices(s.store) {
		v := s.deviceView(d, now)
		devs = append(devs, summaryDevice{
			ID:               v.ID,
			OnlineLabel:      v.OnlineLabel,
			OnlineClass:      v.OnlineClass,
			State:            v.StateLabel,
			SNR:              v.SNRText,
			Obstruction:      v.ObstructionText,
			ObstructionClass: v.ObstructionClass,
			Versions:         v.VersionsText,
			Alerts:           v.Alerts,
			Updated:          v.LastSeenText,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"sessionDead":   s.sessionDead(),
		"accountNumber": s.accountNumber(),
		"updatedAt":     now.Unix(),
		"devices":       devs,
	})
}

func (s *Server) handleHistory(w http.ResponseWriter, r *http.Request) {
	d, ok := s.deviceForAPI(w, r)
	if !ok {
		return
	}
	window := s.cfg.HistoryWindow
	if raw := r.URL.Query().Get("window"); raw != "" {
		dur, err := time.ParseDuration(raw)
		if err != nil || dur <= 0 {
			writeJSON(w, http.StatusBadRequest, apiError{"invalid window"})
			return
		}
		if dur > s.cfg.HistoryWindow {
			dur = s.cfg.HistoryWindow
		}
		window = dur
	}
	samples := s.store.History(d.ID, window)
	samples = downsampleSamples(samples, maxChartBuckets)
	out := make([]apiSample, 0, len(samples))
	var tMin int64
	for i, smp := range samples {
		out = append(out, apiSample{
			Ts:         smp.Ts.Unix(),
			PingMs:     smp.PopPingLatencyMs,
			DropRate:   smp.PopPingDropRate,
			DownBps:    smp.DownlinkThroughputBps,
			UpBps:      smp.UplinkThroughputBps,
			Obstructed: smp.Obstructed,
			Scheduled:  smp.Scheduled,
		})
		if i == 0 {
			tMin = smp.Ts.Unix()
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"samples": out,
		"tMin":    tMin,
		"count":   len(out),
	})
}

// maxChartBuckets caps the chart points a history response carries; larger
// windows are server-side averaged so responses stay small.
const maxChartBuckets = 2000

// downsampleSamples averages consecutive samples into at most maxBuckets
// buckets. Bool flags (obstructed/scheduled) stay true when any sample in
// the bucket was flagged, so short events survive averaging.
func downsampleSamples(samples []starlink.Sample, maxBuckets int) []starlink.Sample {
	if len(samples) <= maxBuckets || maxBuckets <= 0 {
		return samples
	}
	bucket := (len(samples) + maxBuckets - 1) / maxBuckets
	out := make([]starlink.Sample, 0, (len(samples)+bucket-1)/bucket)
	for start := 0; start < len(samples); start += bucket {
		end := start + bucket
		if end > len(samples) {
			end = len(samples)
		}
		agg := samples[start]
		n := end - start
		agg.PopPingLatencyMs = 0
		agg.PopPingDropRate = 0
		agg.DownlinkThroughputBps = 0
		agg.UplinkThroughputBps = 0
		agg.SNR = 0
		for _, smp := range samples[start:end] {
			agg.PopPingLatencyMs += smp.PopPingLatencyMs
			agg.PopPingDropRate += smp.PopPingDropRate
			agg.DownlinkThroughputBps += smp.DownlinkThroughputBps
			agg.UplinkThroughputBps += smp.UplinkThroughputBps
			agg.SNR += smp.SNR
			agg.Obstructed = agg.Obstructed || smp.Obstructed
			agg.Scheduled = agg.Scheduled || smp.Scheduled
		}
		agg.PopPingLatencyMs /= float64(n)
		agg.PopPingDropRate /= float64(n)
		agg.DownlinkThroughputBps /= float64(n)
		agg.UplinkThroughputBps /= float64(n)
		agg.SNR /= float64(n)
		out = append(out, agg)
	}
	return out
}

// apiSample is the JSON shape of one history point. The timestamp is unix
// seconds so the client-side chart can plot it directly.
type apiSample struct {
	Ts         int64   `json:"ts"`
	PingMs     float64 `json:"pingMs"`
	DropRate   float64 `json:"dropRate"`
	DownBps    float64 `json:"downBps"`
	UpBps      float64 `json:"upBps"`
	Obstructed bool    `json:"obstructed"`
	Scheduled  bool    `json:"scheduled"`
}

type apiClient struct {
	Name string `json:"name"`
	MAC  string `json:"mac"`
	Band string `json:"band"`
	IP   string `json:"ip"`
	DBm  string `json:"dBm"`
}

func (s *Server) handleClients(w http.ResponseWriter, r *http.Request) {
	d, ok := s.deviceForAPI(w, r)
	if !ok {
		return
	}
	if d.Kind != starlink.KindRouter {
		writeJSON(w, http.StatusBadRequest, apiError{"not a router"})
		return
	}
	ctx := r.Context()
	clients, err := s.gateway.WifiGetClients(ctx, d.ID)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, apiError{"router unreachable: " + err.Error()})
		return
	}
	out := make([]apiClient, 0, len(clients))
	for _, c := range clients {
		band := ""
		switch c.GetIface() {
		case device.WifiClient_RF_2GHZ:
			band = "2.4 GHz"
		case device.WifiClient_RF_5GHZ:
			band = "5 GHz"
		case device.WifiClient_ETH:
			band = "ethernet"
		}
		out = append(out, apiClient{
			Name: c.GetName(),
			MAC:  c.GetMacAddress(),
			Band: band,
			IP:   c.GetIpAddress(),
			DBm:  fmt.Sprintf("%.0f dBm", c.GetSignalStrength()),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"clients": out, "count": len(out)})
}

// ---------- admin actions ----------

type actionRequest struct {
	Action          string `json:"action"`
	Enable          bool   `json:"enable"`
	StartMinutes    uint32 `json:"startMinutes"`
	DurationMinutes uint32 `json:"durationMinutes"`
}

var sameSiteOK = func(r *http.Request) bool {
	// Basic CSRF defence: browsers send Origin (and Sec-Fetch-Site) on
	// cross-site POSTs; non-browser clients send neither.
	if r.Header.Get("Sec-Fetch-Site") == "cross-site" {
		return false
	}
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true // same-origin fetch from an older browser or a CLI client
	}
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" {
		return false // includes the opaque "null" origin
	}
	return strings.EqualFold(u.Host, r.Host)
}

// dishOnlyActions may not be sent to routers.
var dishOnlyActions = map[string]bool{
	"stow": true, "unstow": true, "inhibit_rf": true, "allow_rf": true, "power_save": true,
}

func (s *Server) handleAction(w http.ResponseWriter, r *http.Request) {
	if !sameSiteOK(r) {
		writeJSON(w, http.StatusForbidden, apiError{"cross-site request rejected"})
		return
	}
	d, ok := s.deviceForAPI(w, r)
	if !ok {
		return
	}
	var req actionRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, apiError{"invalid body"})
		return
	}
	if d.Kind == starlink.KindRouter && dishOnlyActions[req.Action] {
		writeJSON(w, http.StatusBadRequest, apiError{"action " + req.Action + " is not supported on routers"})
		return
	}
	if req.Action == "power_save" && req.Enable {
		if req.StartMinutes >= 24*60 || req.DurationMinutes == 0 || req.DurationMinutes > 24*60 {
			writeJSON(w, http.StatusBadRequest, apiError{"power save needs startMinutes 0-1439 and durationMinutes 1-1440"})
			return
		}
	}

	ctx := r.Context()
	var err error
	var message string
	switch req.Action {
	case "reboot":
		err = s.gateway.Reboot(ctx, d.Kind, d.ID)
		message = "reboot requested"
	case "stow":
		err = s.gateway.Stow(ctx, d.ID, false)
		message = "stow requested"
	case "unstow":
		err = s.gateway.Stow(ctx, d.ID, true)
		message = "unstow requested"
	case "inhibit_rf":
		err = s.gateway.InhibitRF(ctx, d.ID, true)
		message = "RF inhibited"
	case "allow_rf":
		err = s.gateway.InhibitRF(ctx, d.ID, false)
		message = "RF allowed"
	case "power_save":
		err = s.gateway.SetPowerSave(ctx, d.ID, req.StartMinutes, req.DurationMinutes, req.Enable)
		if req.Enable {
			message = fmt.Sprintf("power save enabled: start +%d min, %d min duration", req.StartMinutes, req.DurationMinutes)
		} else {
			message = "power save disabled"
		}
	default:
		writeJSON(w, http.StatusBadRequest, apiError{"unknown action"})
		return
	}
	if err != nil {
		writeJSON(w, http.StatusBadGateway, apiError{err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "message": message})
}
