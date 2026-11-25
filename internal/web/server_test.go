package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/awlx/starlink-manager/api/device"
	"github.com/awlx/starlink-manager/internal/auth"
	"github.com/awlx/starlink-manager/internal/config"
	"github.com/awlx/starlink-manager/internal/starlink"
)

func testConfig(t *testing.T) *config.Config {
	t.Helper()
	return &config.Config{
		ListenAddr:            ":0",
		StarlinkAPIBaseURL:    "https://api.example.test",
		StarlinkWebBaseURL:    "https://www.example.test",
		StarlinkSSOCookie:     "test-sso",
		PollInterval:          30 * time.Second,
		HistoryWindow:         24 * time.Hour,
		CallTimeout:           5 * time.Second,
		DataDir:               t.TempDir(),
		RosterCacheTTL:        5 * time.Minute,
		TelemetryTTLOnline:    5 * time.Minute,
		AuthMode:              config.AuthModeLocalDev,
		LocalDevUser:          true,
		OIDCAdminGroups:       []string{"ffmuc-admin"},
		OIDCAdminClaim:        "groups",
		SessionCookieSecret:   []byte(strings.Repeat("s", 32)),
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
			SNR:                 8.5,
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
	for _, want := range []string{"STARLINK ab12cd", "CONNECTED", "rev3", "online"} {
		if !strings.Contains(page, want) {
			t.Errorf("dashboard missing %q", want)
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

func readAll(t *testing.T, resp *http.Response) string {
	t.Helper()
	buf := make([]byte, 1<<20)
	n, _ := resp.Body.Read(buf)
	return string(buf[:n])
}

var _ = context.Background
