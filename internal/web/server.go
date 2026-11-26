// Package web serves the server-rendered UI and the JSON API of the
// FFMUC Starlink manager. Templates and static assets are embedded so the
// app ships as a single static binary.
package web

import (
	"bytes"
	"context"
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
	"sync"
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
	billing *starlink.Billing
	pages   map[string]*template.Template
	metrics MetricsSink

	billingMu   sync.Mutex
	billingBusy map[string]bool
}

// MetricsSink collects operational metrics (satisfied by metrics.Registry).
type MetricsSink interface {
	SetGauge(name, labels string, value float64)
	IncCounter(name, labels string)
}

// SetMetrics attaches the metrics registry; passing nil disables updates.
func (s *Server) SetMetrics(m MetricsSink) {
	s.metrics = m
}

// SetBilling attaches the billing client; passing nil disables billing
// endpoints (they answer 503).
func (s *Server) SetBilling(b *starlink.Billing) {
	s.billing = b
}

// CollectMetrics refreshes the fleet-level gauges; it is registered as a
// scrape-time collector so the values are current even when nobody opens
// the dashboard.
func (s *Server) CollectMetrics() {
	if s.metrics == nil {
		return
	}
	now := time.Now()
	total, online := 0, 0
	for _, d := range sortedDevices(s.store) {
		total++
		if s.isOnline(d, now) {
			online++
		}
	}
	s.metrics.SetGauge("starlink_devices_total", "", float64(total))
	s.metrics.SetGauge("starlink_devices_online", "", float64(online))
	s.metrics.SetGauge("starlink_session_dead", "", boolGauge(s.sessionDead()))
	s.metrics.SetGauge("starlink_store_size_bytes", "", float64(s.store.FileSize()))
}

func boolGauge(b bool) float64 {
	if b {
		return 1
	}
	return 0
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
	for _, page := range []string{"dashboard", "device", "router", "clients", "billing", "error"} {
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
	protected.HandleFunc("GET /clients", s.handleClientsPage)
	protected.HandleFunc("GET /billing", s.handleBillingPage)
	protected.Handle("GET /api/billing/line/{sl}/options", auth.RequireAdmin(http.HandlerFunc(s.handleBillingOptions)))
	protected.Handle("GET /api/billing/{$}", http.NotFoundHandler())
	protected.Handle("POST /api/billing/line/{sl}/change", auth.RequireAdmin(http.HandlerFunc(s.handleBillingChange)))
	protected.Handle("POST /api/billing/line/{sl}/resume", auth.RequireAdmin(http.HandlerFunc(s.handleBillingResume)))
	protected.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api" || strings.HasPrefix(r.URL.Path, "/api/") {
			http.NotFound(w, r)
			return
		}
		msg := "This page does not exist. Return to the fleet overview to find your devices."
		if r.URL.Path == "/metrics" {
			msg = "Prometheus metrics are served on the separate private metrics listener (default port 9101), not this web app."
		}
		s.renderError(w, r, http.StatusNotFound, "Page not found", msg)
	})
	protected.HandleFunc("GET /api/summary", s.handleSummary)
	protected.HandleFunc("GET /api/device/{id}/status", s.handleDeviceStatus)
	protected.HandleFunc("GET /api/device/{id}/history", s.handleHistory)
	protected.HandleFunc("GET /api/device/{id}/clients", s.handleClients)
	protected.Handle("POST /api/device/{id}/action", auth.RequireAdmin(http.HandlerFunc(s.handleAction)))

	mux.Handle("/", s.auth.Middleware(protected))
	return mux
}

// ---------- template data ----------

type pageInfo struct {
	Title           string
	User            *auth.User
	LogoutURL       string
	SessionDead     bool
	AccountNumber   string
	LastRosterError string
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
	Retired           bool
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
	Key    string
	Title  string
	Series string
}

type devicePageData struct {
	pageInfo
	Device       DeviceView
	AdminUser    bool
	Windows      []windowOpt
	Charts       []chartOpt
	StatusRows   []statusRow
	CloudRows    []statusRow
	RosterRows   []statusRow
	ReportedRows []statusRow
	ClientRows   []statusRow
}

type dashboardData struct {
	pageInfo
	Devices       []DeviceView
	OnlineCount   int
	DegradedCount int
	OfflineCount  int
}

type errorData struct {
	pageInfo
	ErrorCode    string
	ErrorMessage string
}

func (s *Server) render(w http.ResponseWriter, page string, data any) {
	s.renderStatus(w, http.StatusOK, page, data)
}

// renderStatus executes the page into a buffer first so headers (status,
// Content-Type) are set before anything is written and a template error
// yields a clean 500 instead of a truncated page.
func (s *Server) renderStatus(w http.ResponseWriter, code int, page string, data any) {
	t, ok := s.pages[page]
	if !ok {
		http.Error(w, "template missing", http.StatusInternalServerError)
		return
	}
	var buf bytes.Buffer
	if err := t.ExecuteTemplate(&buf, page, data); err != nil {
		s.logf("render %s failed: %v", page, err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(code)
	_, _ = buf.WriteTo(w)
}

func (s *Server) logf(format string, args ...any) {
	slog.Warn(fmt.Sprintf(format, args...))
}

func (s *Server) baseInfo(r *http.Request) pageInfo {
	u, _ := auth.UserFromContext(r.Context())
	return pageInfo{
		Title:           "Starlink Manager",
		User:            u,
		LogoutURL:       s.auth.LogoutURL(),
		SessionDead:     s.sessionDead(),
		AccountNumber:   s.accountNumber(),
		LastRosterError: s.roster.LastError(),
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
		DisplayName:  firstNonEmpty(d.Name, d.ID),
		Nickname:     nicknameOf(d),
		SerialNumber: d.Serial,
		PingText:     "—",
		DropText:     "—",
		DownlinkText: "—",
		UplinkText:   "—",
		Retired:      d.Retired != nil,
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
		// The dish firmware sometimes omits the software version; fall
		// back to the cloud telemetry enrichment.
		v.VersionsText = versionsText(
			firstNonEmpty(st.SoftwareVersion, telemetryString(d.Telemetry, "softwareVersion")),
			st.HardwareVersion)
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
		v.VersionsText = orDash(telemetryString(d.Telemetry, "softwareVersion"))
	}
	if st := d.Status; st != nil && st.Reported != nil {
		ping, drop := "pop_ping_latency_ms", "pop_ping_drop_rate"
		if d.Kind == starlink.KindRouter {
			ping, drop = "ping_latency_ms", "ping_drop_rate"
		}
		for path, target := range map[string]*string{
			ping: &v.PingText, drop: &v.DropText,
			"downlink_throughput_bps": &v.DownlinkText, "uplink_throughput_bps": &v.UplinkText,
		} {
			if _, present := st.Reported[path]; !present {
				*target = "—"
			}
		}
		_, fraction := st.Reported["obstruction_stats.fraction_obstructed"]
		_, current := st.Reported["obstruction_stats.currently_obstructed"]
		if !fraction && !current {
			v.ObstructionText, v.ObstructionClass = "not reported", "pill-muted"
		} else if !fraction && !st.CurrentlyObstructed {
			v.ObstructionText, v.ObstructionClass = "not currently obstructed", "pill-muted"
		} else if !fraction {
			v.ObstructionText, v.ObstructionClass = "obstructed now (fraction not reported)", "pill-error"
		}
	}
	v.Alerts = deviceAlerts(d)
	if v.ObstructionClass == "" {
		v.ObstructionClass = "pill-muted"
	}
	// Health is presentation-only; API reachability semantics stay unchanged.
	v.HealthClass, v.HealthLabel = v.OnlineClass, v.OnlineLabel
	if v.OnlineClass == "pill-ok" && (len(v.Alerts) > 0 || v.ObstructionClass == "pill-warn" || v.ObstructionClass == "pill-error") {
		v.HealthClass, v.HealthLabel = "pill-warn", "degraded"
	}
	if v.Retired {
		v.HealthClass, v.HealthLabel = "pill-muted", "retired"
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
		return "Ethernet link speeds low (may limit throughput)"
	case "slow_ethernet_speeds_100", "ethernet_slow_100mbps":
		return "Ethernet limited to 100 Mbps"
	case "slow_ethernet_speeds_10", "ethernet_slow_10mbps":
		return "Ethernet limited to 10 Mbps"
	case "no_ethernet_link":
		return "no Ethernet link"
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
		pageInfo: s.baseInfo(r),
		Devices:  devs,
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
	// Every window is offered so users can always see the full choice;
	// requests beyond the configured history window are clamped by the
	// server, which is honest about what data exists.
	out := make([]windowOpt, 0, len(historyWindows))
	// The largest window actually backed by stored data is the default
	// selection.
	sel := ""
	for _, w := range historyWindows {
		if w.Value <= cfgWindow {
			sel = w.Value.String()
		}
	}
	for _, w := range historyWindows {
		out = append(out, windowOpt{Label: w.Label, Value: w.Value.String(), Selected: w.Value.String() == sel})
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
	info := s.baseInfo(r)
	info.Title = fmt.Sprintf("%d · %s", code, title)
	s.renderStatus(w, code, "error", errorData{
		pageInfo:     info,
		ErrorCode:    title,
		ErrorMessage: msg,
	})
}

func (s *Server) handleClientsPage(w http.ResponseWriter, r *http.Request) {
	data := dashboardData{pageInfo: s.baseInfo(r)}
	data.Title = "WiFi clients"
	for _, d := range sortedDevices(s.store) {
		if d.Kind == starlink.KindRouter {
			data.Devices = append(data.Devices, s.deviceView(d, time.Now()))
		}
	}
	s.render(w, "clients", data)
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
			{Key: "scheduled", Title: "Scheduled (reported flag)"},
		},
		StatusRows: s.statusRows(d),
		CloudRows:  cloudRows(d.Telemetry),
		RosterRows: detailRows(d.RosterDetails),
		ClientRows: detailRows(d.ClientDetails),
	}
	if d.Status != nil {
		data.ReportedRows = detailRows(d.Status.Reported)
	}
	// Supplemental SNR points stay in HTML so the established history JSON
	// contract remains unchanged.
	type point struct {
		T int64   `json:"t"`
		V float64 `json:"v"`
	}
	data.Charts = append(data.Charts, chartOpt{Key: "snr", Title: "SNR (reported samples)"})
	history := downsampleSamples(s.store.History(d.ID, s.cfg.HistoryWindow), maxChartBuckets)
	for i := range data.Charts {
		key := data.Charts[i].Key
		if key != "snr" && key != "scheduled" && key != "obstruction" {
			continue
		}
		var points []point
		for _, sample := range history {
			switch {
			case key == "snr" && sample.SNRReported:
				points = append(points, point{sample.Ts.Unix(), sample.SNR})
			case key == "scheduled" && sample.ScheduledReported:
				points = append(points, point{sample.Ts.Unix(), boolGauge(sample.Scheduled)})
			case key == "obstruction" && sample.ObstructedReported:
				points = append(points, point{sample.Ts.Unix(), boolGauge(sample.Obstructed)})
			}
		}
		series, _ := json.Marshal(points)
		data.Charts[i].Series = string(series)
	}
	data.Title = data.Device.DisplayName
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
		st = &starlink.DeviceStatus{Reported: map[string]string{}}
		rows = append(rows, statusRow{Label: "Status", Field: "gatewayStatus", Value: "unreachable — no gateway data yet"})
	}
	add := func(label, field, value string) {
		paths := map[string]string{
			"state": "state", "snr": "snr", "ping": "pop_ping_latency_ms", "drop": "pop_ping_drop_rate",
			"down": "downlink_throughput_bps", "up": "uplink_throughput_bps", "stow": "stow_requested",
			"obstructedNow": "obstruction_stats.currently_obstructed", "obstructionValid": "obstruction_stats.valid_s",
			"obstruction24h": "obstruction_stats.last_24h_obstructed_s",
			"captivePortal":  "captive_portal_enabled",
		}
		if d.Kind == starlink.KindRouter {
			paths["ping"], paths["drop"] = "ping_latency_ms", "ping_drop_rate"
		}
		if path, ok := paths[field]; ok && st.Reported != nil {
			if _, present := st.Reported[path]; !present {
				value = "not reported"
			}
		}
		rows = append(rows, statusRow{label, field, value})
	}
	add("Device ID", "deviceID", d.ID)
	add("Service active (roster)", "active", firstNonEmpty(d.RosterDetails["Active service (roster)"], "not reported"))
	if d.Retired != nil {
		add("Retired at (UTC)", "retired", d.Retired.UTC().Format(time.RFC3339))
	}
	if d.Status == nil {
		add("Software version (cloud)", "versions", orDash(telemetryString(d.Telemetry, "softwareVersion")))
		add("Signal quality (cloud, 0–1)", "signalQuality", orDash(telemetryQualityText(d.Telemetry)))
		add("Uptime (cloud)", "uptime", orDash(uptimeText(st, d.Telemetry)))
		if alerts := deviceAlerts(d); len(alerts) > 0 {
			add("Alerts", "alerts", strings.Join(alerts, ", "))
		}
		add("Last seen", "updated", lastSeenText(d))
		return rows
	}

	if d.Kind == starlink.KindRouter {
		add("Software version", "versions", orDash(firstNonEmpty(st.SoftwareVersion, telemetryString(d.Telemetry, "softwareVersion"))))
		add("Hardware version", "hw", orDash(st.HardwareVersion))
		if st.Wifi != nil {
			add("Router serial", "serial", orDash(st.Wifi.SerialNumber))
			add("Router SKU", "sku", orDash(st.Wifi.SKU))
			add("Captive portal enabled", "captivePortal", yesNo(st.Wifi.CaptivePortal))
			add("WAN address", "wan", orDash(st.Wifi.IPv4WANAddress))
			add("Ping latency", "ping", orDash(msText(st.Wifi.PingLatencyMs)))
			add("Ping drop rate", "drop", orDash(pctText(st.Wifi.PingDropRate)))
		}
	} else {
		add("History sample counter", "historyCounter", fmt.Sprint(d.Cursor))
		add("State", "state", firstNonEmpty(st.State, "not reported"))
		add("Software version", "versions",
			orDash(firstNonEmpty(st.SoftwareVersion, telemetryString(d.Telemetry, "softwareVersion"))))
		add("Hardware version", "hw", orDash(st.HardwareVersion))
		add("SNR", "snr", snrText(st.SNR))
		add("Signal quality (cloud, 0–1)", "signalQuality", orDash(telemetryQualityText(d.Telemetry)))
		add("Ping latency", "ping", orDash(msText(st.PopPingLatencyMs)))
		add("Ping drop rate", "drop", orDash(pctText(st.PopPingDropRate)))
		add("Downlink", "down", orDash(bpsText(st.DownlinkThroughput)))
		add("Uplink", "up", orDash(bpsText(st.UplinkThroughput)))
		add("Currently obstructed", "obstructedNow", yesNo(st.CurrentlyObstructed))
		add("Obstruction validity", "obstructionValid", orDash(secondsText(st.ObstructionValidS)))
		add("Obstructed (last 24h)", "obstruction24h", orDash(secondsText(st.ObstructionLast24hS)))
		scheduled := "no recent history"
		if samples := s.store.History(d.ID, 15*time.Minute); len(samples) > 0 && samples[len(samples)-1].ScheduledReported {
			scheduled = yesNo(samples[len(samples)-1].Scheduled)
		}
		add("Scheduled (last sample)", "scheduledFlag", scheduled)
		add("Stow requested", "stow", yesNo(st.StowRequested))
	}
	add("Uptime", "uptime", orDash(uptimeText(st, d.Telemetry)))
	if alerts := deviceAlerts(d); len(alerts) > 0 {
		add("Alerts", "alerts", strings.Join(alerts, ", "))
	}
	if !st.FetchedAt.IsZero() {
		add("Gateway snapshot (UTC)", "fetchedAt", st.FetchedAt.UTC().Format(time.RFC3339))
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

// versionsText joins software and hardware versions, leaving out the
// separator when either is unknown.
func versionsText(sw, hw string) string {
	sw, hw = strings.TrimSpace(sw), strings.TrimSpace(hw)
	if sw != "" && hw != "" {
		return sw + " / " + hw
	}
	return sw + hw
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
	if !ok || v < 0 {
		return ""
	}
	return fmt.Sprintf("%.2f", v)
}

// uptimeText prefers the gateway-reported uptime and falls back to the
// telemetry enrichment value (cloud-only devices).
func uptimeText(st *starlink.DeviceStatus, tm map[string]any) string {
	if st.Reported != nil {
		if _, ok := st.Reported["device_state.uptime_s"]; ok {
			return durationText(st.UptimeS)
		}
	}
	if st.UptimeS > 0 {
		return durationText(st.UptimeS)
	}
	if tm == nil {
		return ""
	}
	if v, ok := tm["uptimeSeconds"].(float64); ok && v >= 0 {
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

func snrText(v *float64) string {
	if v == nil {
		return "—"
	}
	return fmt.Sprintf("%.1f", *v)
}

// activeAlerts returns the labels of the active alerts in a stable order.
// Gateway alert flags and cloud-telemetry alert names are merged so
// alerts that only appear in the telemetry cache still surface.
func deviceAlerts(d *starlink.Device) []string {
	merged := map[string]bool{}
	if st := d.Status; st != nil {
		for name, active := range st.Alerts {
			merged[name] = active
		}
	}
	if tm := d.Telemetry; tm != nil {
		switch raw := tm["activeAlerts"].(type) {
		case []any:
			for _, v := range raw {
				if s, ok := v.(string); ok && s != "" {
					merged[s] = true
				}
			}
		case []string:
			for _, s := range raw {
				if s != "" {
					merged[s] = true
				}
			}
		}
	}
	var out []string
	for name, active := range merged {
		if active {
			out = append(out, alertLabel(name))
		}
	}
	sort.Strings(out)
	return out
}

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
	if v < 0 {
		return ""
	}
	if v < 1 {
		return fmt.Sprintf("%g s", v)
	}
	return (time.Duration(v * float64(time.Second))).Round(time.Millisecond).String()
}

func detailRows(fields map[string]string) []statusRow {
	keys := make([]string, 0, len(fields))
	for key := range fields {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var rows []statusRow
	for _, key := range keys {
		if fields[key] != "" {
			rows = append(rows, statusRow{Label: key, Value: fields[key]})
		}
	}
	return rows
}

func cloudRows(tm map[string]any) []statusRow {
	var rows []statusRow
	for _, field := range []struct{ key, label string }{
		{"timestamp", "Cloud snapshot (UTC)"},
		{"softwareVersion", "Cloud software version"},
		{"countryCode", "Country code (cloud)"},
		{"downlinkThroughputMbps", "Downlink (cloud, Mbps)"},
		{"uplinkThroughputMbps", "Uplink (cloud, Mbps)"},
		{"pingLatencyMsAvg", "Average ping (cloud, ms)"},
		{"pingDropRateAvg", "Average ping drop (cloud, 0–1)"},
		{"obstructionPercentTime", "Obstruction time (cloud, reported value)"},
		{"signalQuality", "Signal quality (cloud, 0–1)"},
		{"uptimeSeconds", "Uptime (cloud, seconds)"},
	} {
		if v, ok := tm[field.key]; ok {
			text := fmt.Sprint(v)
			if field.key == "timestamp" {
				var ns int64
				switch value := v.(type) {
				case int64:
					ns = value
				case float64:
					ns = int64(value)
				}
				if ns <= 0 {
					continue
				}
				text = time.Unix(0, ns).UTC().Format(time.RFC3339)
			}
			if text != "" {
				rows = append(rows, statusRow{Label: field.label, Value: text})
			}
		}
	}
	return rows
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

// handleDeviceStatus returns one device's full status rows as JSON so the
// 30s summary poll can also refresh device- and router-page pills without
// a manual reload.
func (s *Server) handleDeviceStatus(w http.ResponseWriter, r *http.Request) {
	d, ok := s.deviceForAPI(w, r)
	if !ok {
		return
	}
	v := s.deviceView(d, time.Now())
	rows := s.statusRows(d)
	m := map[string]string{}
	for _, row := range rows {
		m[row.Field] = row.Value
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id":                v.ID,
		"onlineLabel":       v.OnlineLabel,
		"onlineClass":       v.OnlineClass,
		"obstruction":       v.ObstructionText,
		"obstructionClass":  v.ObstructionClass,
		"obstructionBarPct": v.ObstructionBarPct,
		"rows":              m,
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
			agg.SNRReported = agg.SNRReported && smp.SNRReported
			agg.ScheduledReported = agg.ScheduledReported && smp.ScheduledReported
			agg.ObstructedReported = agg.ObstructedReported && smp.ObstructedReported
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
	s.store.SetClients(d.ID, clients)
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
		signal := "—"
		if c.SignalStrength != nil && c.GetIface() != device.WifiClient_ETH {
			signal = fmt.Sprintf("%.0f dBm", c.GetSignalStrength())
		}
		out = append(out, apiClient{
			Name: c.GetName(),
			MAC:  c.GetMacAddress(),
			Band: band,
			IP:   c.GetIpAddress(),
			DBm:  signal,
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

// ---------- billing ----------

// billingLine is one service line as shown on the billing page.
type billingLine struct {
	ServiceLine string
	Nickname    string
	Plan        string
	PlanClass   string
	SinceText   string
	UntilText   string
	StatusLabel string
	StatusClass string
	PendingText string
	CanPause    bool
	CanResume   bool
	CanChange   bool
	Terminals   int
}

type billingData struct {
	pageInfo
	Lines     []billingLine
	AdminUser bool
}

// handleBillingPage renders the subscriptions overview. Data comes from the
// cached roster; no billing API call happens on page load.
func (s *Server) handleBillingPage(w http.ResponseWriter, r *http.Request) {
	data := billingData{pageInfo: s.baseInfo(r)}
	data.Title = "Billing & plans"
	u, _ := auth.UserFromContext(r.Context())
	if u != nil {
		data.AdminUser = u.Admin
	}
	if s.roster == nil {
		s.render(w, "billing", data)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	lines, err := s.roster.Get(ctx)
	if err != nil {
		data.LastRosterError = err.Error()
	}
	for _, line := range lines {
		data.Lines = append(data.Lines, billingLineFor(line))
	}
	s.render(w, "billing", data)
}

// billingLineFor derives display fields from a roster service line.
func billingLineFor(line *starlink.ServiceLine) billingLine {
	bl := billingLine{
		ServiceLine: line.Number,
		Nickname:    line.Nickname,
		Terminals:   len(line.Terminals),
	}
	if sub := line.Subscription; sub != nil {
		bl.Plan = sub.ProductDescription
		bl.PlanClass = sub.SubscriptionClass
		bl.CanResume = sub.CanResume
		bl.CanChange = sub.CanChange
		if !sub.StartDate.IsZero() {
			bl.SinceText = sub.StartDate.UTC().Format("2006-01-02")
		}
		if !sub.EndDate.IsZero() {
			bl.UntilText = sub.EndDate.UTC().Format("2006-01-02")
		}
		switch {
		case sub.PendingProductID != "":
			bl.PendingText = "Scheduled change to " + sub.PendingProductID + " at the next billing cycle"
		case sub.StandbyPending:
			bl.PendingText = "Switch to standby scheduled for the next billing cycle"
		}
		// Specific states win over the generic active flag: a paused or
		// suspended line may also report active=false.
		switch {
		case sub.IsSuspended:
			bl.StatusLabel, bl.StatusClass = "Suspended", "pill-error"
		case sub.IsPaused:
			bl.StatusLabel, bl.StatusClass = "Paused", "pill-warn"
		case sub.PendingActivation:
			bl.StatusLabel, bl.StatusClass = "Pending activation", "pill-warn"
		case !sub.Active:
			bl.StatusLabel, bl.StatusClass = "Inactive", "pill-error"
		case sub.IsStandby:
			bl.StatusLabel, bl.StatusClass = "Standby", "pill-warn"
		default:
			bl.StatusLabel, bl.StatusClass = "Active", "pill-ok"
		}
	} else {
		bl.StatusLabel, bl.StatusClass = "Unknown", "pill-warn"
	}
	bl.CanPause = line.CanPauseService
	return bl
}

// billingClientOK answers a JSON error when the billing client is absent.
func (s *Server) billingClientOK(w http.ResponseWriter) bool {
	if s.billing == nil {
		writeJSON(w, http.StatusServiceUnavailable, apiError{"billing is not configured"})
		return false
	}
	return true
}

// handleBillingOptions returns the available plan change options for a
// service line (admin only, read-only, cached ~5 minutes). Only service
// lines on the account roster are looked up.
func (s *Server) handleBillingOptions(w http.ResponseWriter, r *http.Request) {
	if !s.billingClientOK(w) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	sl := r.PathValue("sl")
	if _, err := s.serviceLineFor(ctx, sl); err != nil {
		writeJSON(w, http.StatusNotFound, apiError{err.Error()})
		return
	}
	opts, err := s.billing.GetChangeOptions(ctx, sl)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, apiError{err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, opts)
}

type billingChangeRequest struct {
	ProductID string `json:"productId"`
	Confirm   string `json:"confirm"`
}

// decodeBillingRequest enforces same-origin, a JSON content type (so a
// cross-site HTML form cannot submit it) and a bounded JSON body.
func decodeBillingRequest(w http.ResponseWriter, r *http.Request) (billingChangeRequest, bool) {
	var req billingChangeRequest
	if !sameSiteOK(r) {
		writeJSON(w, http.StatusForbidden, apiError{"cross-site request rejected"})
		return req, false
	}
	if ct := r.Header.Get("Content-Type"); !strings.HasPrefix(strings.ToLower(strings.TrimSpace(ct)), "application/json") {
		writeJSON(w, http.StatusUnsupportedMediaType, apiError{"content type must be application/json"})
		return req, false
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, apiError{"invalid body"})
		return req, false
	}
	return req, true
}

// billingMutationTimeout bounds a billing mutation. The mutation context is
// detached from the request so a client disconnect cannot cancel a POST
// that the account API may already be applying (which would log a false
// failure).
const billingMutationTimeout = 60 * time.Second

// handleBillingChange performs a plan change after strict server-side
// validation: the line must allow changes, the product must be among the
// live (uncached) options and not the current plan, and the typed
// confirmation must equal the target plan's description exactly (only
// surrounding whitespace is ignored). Options with an immediate variant
// apply now (prorated); scheduled-only options (e.g. Standby Mode) apply at
// the next billing cycle.
func (s *Server) handleBillingChange(w http.ResponseWriter, r *http.Request) {
	if !s.billingClientOK(w) {
		return
	}
	req, ok := decodeBillingRequest(w, r)
	if !ok {
		return
	}
	sl := r.PathValue("sl")
	user := auditUser(r)
	reject := func(status int, msg string) {
		s.logAudit("billing plan change rejected", sl, req.ProductID, user, msg)
		writeJSON(w, status, apiError{msg})
	}
	if strings.TrimSpace(req.ProductID) == "" {
		reject(http.StatusBadRequest, "productId is required")
		return
	}
	unlock, ok := s.lockBillingLine(sl)
	if !ok {
		reject(http.StatusConflict, "another billing change for this service line is in progress")
		return
	}
	defer unlock()

	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), billingMutationTimeout)
	defer cancel()

	line, err := s.serviceLineFor(ctx, sl)
	if err != nil {
		reject(http.StatusNotFound, err.Error())
		return
	}
	sub := line.Subscription
	if sub == nil || sub.ReferenceID == "" {
		reject(http.StatusConflict, "no subscription reference for "+sl)
		return
	}
	if !sub.CanChange {
		reject(http.StatusConflict, "the account does not allow plan changes for this service line")
		return
	}
	// Validate against live options, never the cache.
	opts, err := s.billing.FreshChangeOptions(ctx, sl)
	if err != nil {
		reject(http.StatusBadGateway, err.Error())
		return
	}
	var target *starlink.ChangeOption
	for i := range opts.Options {
		if opts.Options[i].Product.ProductID == req.ProductID {
			target = &opts.Options[i]
			break
		}
	}
	if target == nil {
		reject(http.StatusBadRequest, "plan is not among the offered options")
		return
	}
	if target.IsCurrent || req.ProductID == opts.Current.ProductID || req.ProductID == sub.ProductID {
		reject(http.StatusBadRequest, "plan is already the current plan of this service line")
		return
	}
	want := target.Product.Description
	if want == "" || strings.TrimSpace(req.Confirm) != want {
		reject(http.StatusBadRequest, fmt.Sprintf("confirmation must be the exact plan name %q", want))
		return
	}
	schedule, ok := target.Schedule()
	if !ok {
		reject(http.StatusBadRequest, "the account API offers no price variant for this plan")
		return
	}

	s.logAudit("billing plan change attempted", sl, req.ProductID, user, fmt.Sprintf("scheduled=%t", schedule))
	effective, err := s.billing.UpdatePlan(ctx, sub.ReferenceID, req.ProductID, schedule)
	// Whatever the outcome, the cached plan data may now be outdated.
	s.billing.Invalidate(sl)
	if s.roster != nil {
		s.roster.Invalidate()
	}
	if err != nil {
		s.logAudit("billing plan change failed", sl, req.ProductID, user, err.Error())
		writeJSON(w, http.StatusBadGateway, apiError{err.Error()})
		return
	}
	s.logAudit("billing plan change applied", sl, req.ProductID, user, fmt.Sprintf("scheduled=%t", schedule))
	msg := fmt.Sprintf("Plan change to %q applied (immediate, prorated). It may take a few minutes to show up.", want)
	if schedule {
		// Prefer the date the update response confirms; fall back to the
		// date from the option list, then to a generic phrase. The
		// response date is only available when the envelope check passed.
		when := "the next billing cycle"
		switch {
		case effective != nil:
			when = effective.UTC().Format("2006-01-02")
		case !target.ScheduledEffective.IsZero():
			when = target.ScheduledEffective.UTC().Format("2006-01-02")
		}
		msg = fmt.Sprintf("Plan change to %q scheduled for %s.", want, when)
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "scheduled": schedule, "message": msg})
}

// handleBillingResume resumes a paused subscription line after a typed
// "RESUME" confirmation (exact, case-sensitive).
func (s *Server) handleBillingResume(w http.ResponseWriter, r *http.Request) {
	if !s.billingClientOK(w) {
		return
	}
	req, ok := decodeBillingRequest(w, r)
	if !ok {
		return
	}
	sl := r.PathValue("sl")
	user := auditUser(r)
	reject := func(status int, msg string) {
		s.logAudit("billing resume rejected", sl, "", user, msg)
		writeJSON(w, status, apiError{msg})
	}
	if strings.TrimSpace(req.Confirm) != "RESUME" {
		reject(http.StatusBadRequest, `confirmation must be exactly "RESUME"`)
		return
	}
	unlock, ok := s.lockBillingLine(sl)
	if !ok {
		reject(http.StatusConflict, "another billing change for this service line is in progress")
		return
	}
	defer unlock()

	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), billingMutationTimeout)
	defer cancel()

	line, err := s.serviceLineFor(ctx, sl)
	if err != nil {
		reject(http.StatusNotFound, err.Error())
		return
	}
	sub := line.Subscription
	if sub == nil || sub.ReferenceID == "" {
		reject(http.StatusConflict, "no subscription reference for "+sl)
		return
	}
	if !sub.CanResume {
		reject(http.StatusConflict, "the account does not offer a resume for this service line")
		return
	}
	s.logAudit("billing resume attempted", sl, "", user, "")
	err = s.billing.Resume(ctx, sub.ReferenceID)
	s.billing.Invalidate(sl)
	if s.roster != nil {
		s.roster.Invalidate()
	}
	if err != nil {
		s.logAudit("billing resume failed", sl, "", user, err.Error())
		writeJSON(w, http.StatusBadGateway, apiError{err.Error()})
		return
	}
	s.logAudit("billing resume applied", sl, "", user, "")
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      true,
		"message": "Service line resumed. It may take a few minutes to become active.",
	})
}

// serviceLineFor finds a service line on the account roster; billing
// endpoints only ever address lines that belong to the account.
func (s *Server) serviceLineFor(ctx context.Context, serviceLine string) (*starlink.ServiceLine, error) {
	if s.roster == nil {
		return nil, fmt.Errorf("roster unavailable")
	}
	lines, err := s.roster.Get(ctx)
	if err != nil {
		return nil, fmt.Errorf("roster unavailable: %w", err)
	}
	for _, line := range lines {
		if serviceLine != "" && line.Number == serviceLine {
			return line, nil
		}
	}
	return nil, fmt.Errorf("unknown service line %q", serviceLine)
}

// lockBillingLine serializes mutations per service line so a double submit
// (or two admins) cannot fire two plan changes concurrently.
func (s *Server) lockBillingLine(sl string) (func(), bool) {
	s.billingMu.Lock()
	defer s.billingMu.Unlock()
	if s.billingBusy == nil {
		s.billingBusy = map[string]bool{}
	}
	if s.billingBusy[sl] {
		return nil, false
	}
	s.billingBusy[sl] = true
	return func() {
		s.billingMu.Lock()
		delete(s.billingBusy, sl)
		s.billingMu.Unlock()
	}, true
}

// auditUser identifies the acting user for the audit log (never tokens).
func auditUser(r *http.Request) string {
	u, _ := auth.UserFromContext(r.Context())
	if u == nil {
		return "<unknown>"
	}
	for _, v := range []string{u.Email, u.Name, u.Sub} {
		if v != "" {
			return v
		}
	}
	return "<unknown>"
}

// logAudit writes an auditable billing mutation event (plan, service line,
// outcome — never credentials).
func (s *Server) logAudit(event, serviceLine, plan, user, detail string) {
	args := []any{"audit", "billing", "serviceLine", serviceLine, "user", user}
	if plan != "" {
		args = append(args, "plan", plan)
	}
	if detail != "" {
		args = append(args, "detail", detail)
	}
	slog.Info(event, args...)
}
