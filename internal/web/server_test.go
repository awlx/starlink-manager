package web

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/awlx/starlink-manager/api/device"
	"github.com/awlx/starlink-manager/internal/auth"
	"github.com/awlx/starlink-manager/internal/config"
	"github.com/awlx/starlink-manager/internal/starlink"
	"google.golang.org/protobuf/proto"
)

func testConfig(t *testing.T) *config.Config {
	t.Helper()
	return &config.Config{
		ListenAddr:          ":0",
		StarlinkAPIBaseURL:  "https://api.example.test",
		StarlinkWebBaseURL:  "https://www.example.test",
		StarlinkSSOCookie:   "test-sso",
		PollInterval:        30 * time.Second,
		HistoryWindow:       24 * time.Hour,
		CallTimeout:         5 * time.Second,
		DataDir:             t.TempDir(),
		RosterCacheTTL:      5 * time.Minute,
		TelemetryTTLOnline:  5 * time.Minute,
		AuthMode:            config.AuthModeLocalDev,
		LocalDevUser:        true,
		OIDCAdminGroups:     []string{"ffmuc-admin"},
		OIDCAdminClaim:      "groups",
		SessionCookieSecret: []byte(strings.Repeat("s", 32)),
	}
}

func newTestServer(t *testing.T) (*Server, *httptest.Server) {
	t.Helper()
	cfg := testConfig(t)
	store := starlink.NewStore(cfg.DataDir, cfg.HistoryWindow, nil)
	authN := auth.LocalDev{}
	gw := starlink.NewGateway(nil, cfg.StarlinkWebBaseURL, &http.Client{}, nil)
	srv, err := New(cfg, authN, gw, nil, store, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return srv, ts
}

func seedDevice(t *testing.T, store *starlink.Store) *starlink.Device {
	t.Helper()
	d := &starlink.Device{
		ID:          "ut1234",
		Kind:        starlink.KindDish,
		Name:        "STARLINK ab12cd",
		Serial:      "ut1234",
		ServiceLine: "SL-1",
		Active:      true,
		Status: &starlink.DeviceStatus{
			FetchedAt:           time.Now(),
			State:               "CONNECTED",
			SoftwareVersion:     "2024.05.01",
			HardwareVersion:     "rev3",
			SNR:                 proto.Float64(8.5),
			PopPingLatencyMs:    42,
			PopPingDropRate:     0.01,
			DownlinkThroughput:  1.2e8,
			UplinkThroughput:    2.1e7,
			ObstructionFraction: 0.003,
			CurrentlyObstructed: false,
		},
		LastSeen: time.Now(),
	}
	store.UpsertDevice(d)
	return d
}

func TestHealthzIsUnauthenticated(t *testing.T) {
	_, ts := newTestServer(t)
	resp, err := http.Get(ts.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("healthz status = %d", resp.StatusCode)
	}
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body["status"] != "ok" {
		t.Fatalf("healthz body = %v", body)
	}
}

func TestStaticAssetsServed(t *testing.T) {
	_, ts := newTestServer(t)
	for _, asset := range []string{"/static/style.css", "/static/app.js", "/static/theme.js"} {
		resp, err := http.Get(ts.URL + asset)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("%s status = %d, want 200", asset, resp.StatusCode)
		}
		if len(body) == 0 {
			t.Errorf("%s served empty body", asset)
		}
	}
}

func TestDashboardRendersDevices(t *testing.T) {
	srv, ts := newTestServer(t)
	seedDevice(t, srv.store)
	resp, err := http.Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("dashboard status = %d", resp.StatusCode)
	}
	page := readAll(t, resp)
	for _, want := range []string{"STARLINK ab12cd", "CONNECTED", "rev3", "online", "42 ms", "120.0 Mbps", "21.0 Mbps", "1.0 %", `data-count="online">1`, `data-count="offline">0`} {
		if !strings.Contains(page, want) {
			t.Errorf("dashboard missing %q", want)
		}
	}
}

func TestHealthPresentationPreservesReachability(t *testing.T) {
	srv, ts := newTestServer(t)
	d := seedDevice(t, srv.store)
	for _, tc := range []struct {
		name       string
		obstructed bool
		fraction   float64
		alert      bool
		stale      bool
		want       string
	}{
		{name: "clear", want: "online"},
		{name: "active obstruction", obstructed: true, want: "degraded"},
		{name: "elevated fraction", fraction: 0.05, want: "degraded"},
		{name: "active alert", alert: true, want: "degraded"},
		{name: "stale warning stays unreachable", alert: true, stale: true, want: "unreachable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d.LastSeen = time.Now()
			if tc.stale {
				d.LastSeen = time.Now().Add(-time.Hour)
			}
			d.Status.CurrentlyObstructed = tc.obstructed
			d.Status.ObstructionFraction = tc.fraction
			d.Status.Alerts = map[string]bool{"thermal_throttle": tc.alert}
			v := srv.deviceView(d, time.Now())
			if v.HealthLabel != tc.want {
				t.Fatalf("health = %q, want %q", v.HealthLabel, tc.want)
			}
			if !tc.stale && v.OnlineLabel != "online" {
				t.Fatalf("presentation changed API reachability: %q", v.OnlineLabel)
			}
		})
	}
	d.LastSeen = time.Now()
	d.Status.CurrentlyObstructed = true
	srv.store.UpsertDevice(d)
	resp, err := http.Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	page := readAll(t, resp)
	for _, want := range []string{`data-count="degraded">1`, `data-count="online">0`, `class="pill pill-warn" data-field="online">degraded`} {
		if !strings.Contains(page, want) {
			t.Errorf("degraded dashboard missing %q", want)
		}
	}
}

func TestDashboardMetricsMissingAndRouter(t *testing.T) {
	srv, _ := newTestServer(t)
	d := seedDevice(t, srv.store)
	d.Status = nil
	v := srv.deviceView(d, time.Now())
	if v.PingText != "—" || v.DropText != "—" || v.DownlinkText != "—" || v.UplinkText != "—" {
		t.Fatalf("missing status must not look like zero traffic: %+v", v)
	}
	d.Kind = starlink.KindRouter
	d.Status = &starlink.DeviceStatus{Wifi: &starlink.WifiStatusExtra{PingLatencyMs: 12, PingDropRate: 0.02}}
	v = srv.deviceView(d, time.Now())
	if v.PingText != "12 ms" || v.DropText != "2.0 %" || v.DownlinkText != "—" || v.UplinkText != "—" {
		t.Fatalf("router metrics must use WiFi status: %+v", v)
	}
}

func TestRosterErrorBannerRendersSafely(t *testing.T) {
	srv, _ := newTestServer(t)
	w := httptest.NewRecorder()
	srv.render(w, "dashboard", dashboardData{
		LastRosterError: "upstream unavailable <retry>",
	})
	body := w.Body.String()
	for _, want := range []string{`role="alert"`, "Roster refresh failed.", "upstream unavailable &lt;retry&gt;", "Showing the last known devices.", "</html>"} {
		if !strings.Contains(body, want) {
			t.Errorf("roster error page missing %q", want)
		}
	}
}

func TestRedesignedPagesPreserveViewerGating(t *testing.T) {
	srv, _ := newTestServer(t)
	d := seedDevice(t, srv.store)
	srv.cfg.HistoryWindow = 72 * time.Hour
	for _, kind := range []starlink.DeviceKind{starlink.KindDish, starlink.KindRouter} {
		d.Kind = kind
		srv.store.UpsertDevice(d)
		for _, admin := range []bool{false, true} {
			req := httptest.NewRequest(http.MethodGet, "/device/"+d.ID, nil)
			req.SetPathValue("id", d.ID)
			req = req.WithContext(auth.WithUser(req.Context(), &auth.User{Name: "test", Admin: admin}))
			w := httptest.NewRecorder()
			srv.handleDevicePage(w, req)
			body := w.Body.String()
			if strings.Contains(body, `data-action="reboot"`) != admin {
				t.Errorf("%s admin=%v: action gating changed", kind, admin)
			}
			if !strings.Contains(body, "</html>") || !strings.Contains(body, "Current status") {
				t.Errorf("%s: incomplete rendered page", kind)
			}
			if kind == starlink.KindDish {
				for _, want := range []string{"Show 3d of history", `aria-pressed="true"`, `data-chart="scheduled"`, `data-chart="obstruction"`, `tabindex="0"`} {
					if !strings.Contains(body, want) {
						t.Errorf("dish missing %q", want)
					}
				}
			} else if !strings.Contains(body, `id="clients-table"`) || !strings.Contains(body, `data-action="refresh_clients"`) {
				t.Error("router clients UI missing")
			}
		}
	}
}

func TestDashboardToleratesUnreachableDevice(t *testing.T) {
	srv, ts := newTestServer(t)
	d := seedDevice(t, srv.store)
	d.LastSeen = time.Time{} // never seen
	srv.store.UpsertDevice(d)
	resp, err := http.Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	page := readAll(t, resp)
	if !strings.Contains(page, "unreachable") {
		t.Errorf("dashboard should show 'unreachable' for stale device")
	}
}

func TestDevicePageAndHistory(t *testing.T) {
	srv, ts := newTestServer(t)
	d := seedDevice(t, srv.store)
	// seed two history samples via the store's cursor-merge path
	cur := uint64(time.Now().Unix())
	n := srv.store.AppendHistory(d.ID, &device.DishGetHistoryResponse{
		Current:               &cur,
		PopPingLatencyMs:      []float32{40, 45},
		PopPingDropRate:       []float32{0, 0.02},
		DownlinkThroughputBps: []float32{1e8, 1.2e8},
		UplinkThroughputBps:   []float32{2e7, 2.1e7},
		Snr:                   []float32{8, 8.5},
	})
	_ = n

	resp, err := http.Get(ts.URL + "/device/ut1234")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("device page status = %d", resp.StatusCode)
	}
	page := readAll(t, resp)
	if !strings.Contains(page, "Ping latency") || !strings.Contains(page, "Reboot") {
		t.Errorf("device page missing expected sections")
	}

	resp2, err := http.Get(ts.URL + "/api/device/ut1234/history?window=1h")
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	var hist struct {
		Samples []map[string]any `json:"samples"`
		Count   int              `json:"count"`
	}
	if err := json.NewDecoder(resp2.Body).Decode(&hist); err != nil {
		t.Fatal(err)
	}
	if hist.Count != 2 || len(hist.Samples) != 2 {
		t.Errorf("expected 2 history samples, got %d", hist.Count)
	}
	if hist.Samples[0]["pingMs"] == nil {
		t.Errorf("sample missing pingMs: %v", hist.Samples[0])
	}

	resp3, err := http.Get(ts.URL + "/device/unknown")
	if err != nil {
		t.Fatal(err)
	}
	resp3.Body.Close()
	if resp3.StatusCode != http.StatusNotFound {
		t.Errorf("unknown device status = %d, want 404", resp3.StatusCode)
	}
}

func TestActionRequiresAdmin(t *testing.T) {
	srv, _ := newTestServer(t)
	seedDevice(t, srv.store)
	// LocalDev auth grants admin to everyone; swap in a middleware that
	// authenticates a non-admin user to verify RequireAdmin blocks it.
	nonAdmin := middlewareFunc(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := auth.WithUser(r.Context(), &auth.User{Sub: "u1", Name: "viewer", Admin: false})
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	})
	srv.auth = nonAdmin
	// rebuild handler tree with the replaced authenticator
	ts2 := httptest.NewServer(srv.Handler())
	defer ts2.Close()

	req, _ := http.NewRequest("POST", ts2.URL+"/api/device/ut1234/action",
		strings.NewReader(`{"action":"reboot"}`))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("non-admin action status = %d, want 403", resp.StatusCode)
	}

	// unknown action with admin should 400
	adminTs := httptest.NewServer(func() http.Handler {
		srv.auth = middlewareFunc(func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				ctx := auth.WithUser(r.Context(), &auth.User{Sub: "u2", Admin: true})
				next.ServeHTTP(w, r.WithContext(ctx))
			})
		})
		return srv.Handler()
	}())
	defer adminTs.Close()

	req2, _ := http.NewRequest("POST", adminTs.URL+"/api/device/ut1234/action",
		strings.NewReader(`{"action":"explode"}`))
	req2.Header.Set("Content-Type", "application/json")
	resp2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusBadRequest {
		t.Errorf("unknown action status = %d, want 400", resp2.StatusCode)
	}
}

func TestSummaryJSON(t *testing.T) {
	srv, ts := newTestServer(t)
	seedDevice(t, srv.store)
	resp, err := http.Get(ts.URL + "/api/summary")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var body struct {
		Devices []map[string]any `json:"devices"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if len(body.Devices) != 1 {
		t.Fatalf("summary devices = %d, want 1", len(body.Devices))
	}
	if body.Devices[0]["onlineLabel"] != "online" {
		t.Errorf("onlineLabel = %v", body.Devices[0]["onlineLabel"])
	}
}

func TestClientsRejectsDish(t *testing.T) {
	srv, ts := newTestServer(t)
	seedDevice(t, srv.store)
	resp, err := http.Get(ts.URL + "/api/device/ut1234/clients")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("clients on dish status = %d, want 400", resp.StatusCode)
	}
}

type middlewareFunc func(http.Handler) http.Handler

func (f middlewareFunc) Middleware(next http.Handler) http.Handler { return f(next) }
func (middlewareFunc) Routes(*http.ServeMux)                       {}
func (middlewareFunc) LogoutURL() string                           { return "/" }

var _ auth.Authenticator = middlewareFunc(nil)

func TestObstructionClassThresholds(t *testing.T) {
	cases := []struct {
		frac      float64
		currently bool
		want      string
	}{
		{0.001, false, "pill-ok"},
		{0.019, false, "pill-ok"},
		{0.02, false, "pill-warn"},
		{0.05, false, "pill-warn"},
		{0.10, false, "pill-warn"},
		{0.101, false, "pill-error"},
		{0.004, true, "pill-error"},
	}
	for _, c := range cases {
		got := obstructionClass(&starlink.DeviceStatus{
			ObstructionFraction: c.frac,
			CurrentlyObstructed: c.currently,
		})
		if got != c.want {
			t.Errorf("obstructionClass(%v, %v) = %q, want %q", c.frac, c.currently, got, c.want)
		}
	}
}

func TestDevicePageObstructionBadge(t *testing.T) {
	srv, ts := newTestServer(t)
	seedDevice(t, srv.store)
	resp, err := http.Get(ts.URL + "/device/ut1234")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	html := string(body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("device page status = %d", resp.StatusCode)
	}
	for _, want := range []string{"obstruction-fill", "pill-ok", "0.3%"} {
		if !strings.Contains(html, want) {
			t.Errorf("device page missing %q", want)
		}
	}
	// seedDevice: 0.3% obstruction, not currently obstructed → green badge.
	if !strings.Contains(html, "obstruction-bar") {
		t.Error("device page missing obstruction bar")
	}
}

func TestHistoryIncludesObstructionFields(t *testing.T) {
	srv, ts := newTestServer(t)
	d := seedDevice(t, srv.store)
	srv.store.AppendHistory(d.ID, &device.DishGetHistoryResponse{
		Current:          proto.Uint64(2),
		PopPingLatencyMs: []float32{40, 41},
		Scheduled:        []bool{false, true},
		Obstructed:       []bool{true, false},
	})
	resp, err := http.Get(ts.URL + "/api/device/ut1234/history?window=1h")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var body struct {
		Samples []map[string]any `json:"samples"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if len(body.Samples) != 2 {
		t.Fatalf("samples = %d, want 2", len(body.Samples))
	}
	if body.Samples[0]["obstructed"] != true {
		t.Errorf("sample 0 obstructed = %v, want true", body.Samples[0]["obstructed"])
	}
	if body.Samples[1]["scheduled"] != true {
		t.Errorf("sample 1 scheduled = %v, want true", body.Samples[1]["scheduled"])
	}
}

func readAll(t *testing.T, resp *http.Response) string {
	t.Helper()
	buf := make([]byte, 1<<20)
	n, _ := resp.Body.Read(buf)
	return string(buf[:n])
}

var _ = context.Background

func TestOnlineUsesConfiguredTTL(t *testing.T) {
	srv, _ := newTestServer(t)
	now := time.Now()
	d := &starlink.Device{ID: "ut-ttl", Kind: starlink.KindDish, LastSeen: now.Add(-2 * time.Minute)}
	srv.cfg.TelemetryTTLOnline = time.Minute
	if v := srv.deviceView(d, now); v.OnlineLabel != "unreachable" {
		t.Fatalf("label = %q with a 1m TTL and a 2m old poll", v.OnlineLabel)
	}
	srv.cfg.TelemetryTTLOnline = 10 * time.Minute
	if v := srv.deviceView(d, now); v.OnlineLabel != "online" {
		t.Fatalf("label = %q with a 10m TTL", v.OnlineLabel)
	}
}

func TestAPIUnknownDeviceIsCleanJSON(t *testing.T) {
	_, ts := newTestServer(t)
	for _, req := range []struct{ method, path, body string }{
		{"GET", "/api/device/nope/history", ""},
		{"GET", "/api/device/nope/clients", ""},
		{"POST", "/api/device/nope/action", `{"action":"reboot"}`},
	} {
		r, _ := http.NewRequest(req.method, ts.URL+req.path, strings.NewReader(req.body))
		resp, err := http.DefaultClient.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		body := readAll(t, resp)
		if resp.StatusCode != http.StatusNotFound || !strings.HasPrefix(resp.Header.Get("Content-Type"), "application/json") {
			t.Errorf("%s: status %d type %q", req.path, resp.StatusCode, resp.Header.Get("Content-Type"))
		}
		var e map[string]string
		if err := json.Unmarshal([]byte(body), &e); err != nil || e["error"] != "unknown device" {
			t.Errorf("%s: body %q is not a single JSON error", req.path, body)
		}
	}
}

func TestSameSiteOK(t *testing.T) {
	cases := []struct {
		name, origin, fetchSite string
		want                    bool
	}{
		{"no origin", "", "", true},
		{"same origin with port", "http://localhost:8090", "same-origin", true},
		{"same origin case", "https://Starlink.FFMUC.net", "", true},
		{"other host", "https://evil.example", "", false},
		{"other port", "http://localhost:9999", "", false},
		{"null origin", "null", "", false},
		{"cross-site fetch metadata", "", "cross-site", false},
	}
	for _, c := range cases {
		r := httptest.NewRequest("POST", "/api/device/x/action", nil)
		r.Host = "localhost:8090"
		if c.name == "same origin case" {
			r.Host = "starlink.ffmuc.net"
		}
		if c.origin != "" {
			r.Header.Set("Origin", c.origin)
		}
		if c.fetchSite != "" {
			r.Header.Set("Sec-Fetch-Site", c.fetchSite)
		}
		if got := sameSiteOK(r); got != c.want {
			t.Errorf("%s: sameSiteOK = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestActionValidation(t *testing.T) {
	srv, ts := newTestServer(t)
	seedDevice(t, srv.store)
	srv.store.UpsertDevice(&starlink.Device{ID: "Router-1", Kind: starlink.KindRouter, Name: "r"})
	for _, c := range []struct{ id, body string }{
		{"Router-1", `{"action":"stow"}`},
		{"Router-1", `{"action":"inhibit_rf"}`},
		{"ut1234", `{"action":"power_save","enable":true,"startMinutes":1440,"durationMinutes":60}`},
		{"ut1234", `{"action":"power_save","enable":true,"startMinutes":60,"durationMinutes":0}`},
	} {
		resp, err := http.Post(ts.URL+"/api/device/"+c.id+"/action", "application/json", strings.NewReader(c.body))
		if err != nil {
			t.Fatal(err)
		}
		body := readAll(t, resp)
		if resp.StatusCode != http.StatusBadRequest || !strings.Contains(body, `"error":`) {
			t.Errorf("%s %s: status %d body %q, want a 400 JSON error", c.id, c.body, resp.StatusCode, body)
		}
	}
}

func TestDeviceViewUnreportedStateAndSNR(t *testing.T) {
	srv, ts := newTestServer(t)
	d := seedDevice(t, srv.store)
	d.Status.State = ""
	d.Status.SNR = nil
	d.Status.Alerts = map[string]bool{"thermal_throttle": true, "motors_stuck": true, "obstructed": false}
	srv.store.UpsertDevice(d)

	v := srv.deviceView(d, time.Now())
	if v.StateLabel != "not reported" || v.SNRText != "—" {
		t.Errorf("state %q snr %q, want not reported / —", v.StateLabel, v.SNRText)
	}
	if strings.Join(v.Alerts, ",") != "motors stuck,thermal throttling" {
		t.Errorf("alerts = %v, want sorted active alerts", v.Alerts)
	}
	resp, err := http.Get(ts.URL + "/device/" + d.ID)
	if err != nil {
		t.Fatal(err)
	}
	if body := readAll(t, resp); strings.Contains(body, ">0.0<") {
		t.Error("device page shows SNR 0.0 for a dish that did not report SNR")
	}
}

func TestSortedDevicesStable(t *testing.T) {
	srv, _ := newTestServer(t)
	for _, id := range []string{"utC", "Router-A", "utB", "utA"} {
		kind := starlink.KindDish
		if strings.HasPrefix(id, "Router") {
			kind = starlink.KindRouter
		}
		srv.store.UpsertDevice(&starlink.Device{ID: id, Kind: kind, Name: "same"})
	}
	for i := 0; i < 20; i++ {
		var ids []string
		for _, d := range sortedDevices(srv.store) {
			ids = append(ids, d.ID)
		}
		if got := strings.Join(ids, ","); got != "utA,utB,utC,Router-A" {
			t.Fatalf("order = %s", got)
		}
	}
}

func TestSummaryEmptyDeviceListIsArray(t *testing.T) {
	_, ts := newTestServer(t)
	resp, err := http.Get(ts.URL + "/api/summary")
	if err != nil {
		t.Fatal(err)
	}
	if body := readAll(t, resp); !strings.Contains(body, `"devices":[]`) {
		t.Errorf("summary without devices = %s, want devices:[]", body)
	}
}

func TestFaviconServed(t *testing.T) {
	_, ts := newTestServer(t)
	resp, err := http.Get(ts.URL + "/favicon.ico")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("favicon status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "image/svg+xml" {
		t.Errorf("favicon content type = %q", ct)
	}
	if body := readAll(t, resp); !strings.Contains(body, "<svg") {
		t.Error("favicon body is not an SVG")
	}
}

func TestDownsampleSamples(t *testing.T) {
	samples := make([]starlink.Sample, 5000)
	for i := range samples {
		samples[i] = starlink.Sample{Ts: time.Unix(int64(i), 0), PopPingLatencyMs: 40}
	}
	got := downsampleSamples(samples, maxChartBuckets)
	if len(got) > maxChartBuckets {
		t.Fatalf("downsampled to %d buckets, want <= %d", len(got), maxChartBuckets)
	}
	// Small inputs pass through unchanged.
	if got := downsampleSamples(samples[:10], maxChartBuckets); len(got) != 10 {
		t.Fatalf("small input changed: %d", len(got))
	}
}

func TestTelemetryFallbacksInStatusRows(t *testing.T) {
	srv, ts := newTestServer(t)
	d := seedDevice(t, srv.store)
	// Wipe the gateway-reported fields so the telemetry fallbacks show.
	if dev, ok := srv.store.Device(d.ID); ok {
		dev.Status.SoftwareVersion = ""
		dev.Status.UptimeS = 0
		srv.store.SetStatus(dev.ID, dev.Status)
		srv.store.SetTelemetry(dev.ID, map[string]any{
			"softwareVersion": "2025.01.10.cloud",
			"uptimeSeconds":   float64(7200),
			"signalQuality":   float64(0.8),
		})
	}
	resp, err := http.Get(ts.URL + "/device/ut1234")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body := readAll(t, resp)
	for _, want := range []string{"2025.01.10.cloud", "2h0m0s", "0.80"} {
		if !strings.Contains(body, want) {
			t.Errorf("device page missing telemetry fallback %q", want)
		}
	}
}
