package web

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/awlx/starlink-manager/internal/starlink"
)

func TestEmptyAndAllOfflineFleet(t *testing.T) {
	srv, ts := newTestServer(t)
	read := func() string {
		t.Helper()
		r, err := http.Get(ts.URL)
		if err != nil {
			t.Fatal(err)
		}
		defer r.Body.Close()
		return readAll(t, r)
	}
	if !strings.Contains(read(), "No devices found") {
		t.Fatal("missing empty fleet state")
	}
	for _, id := range []string{"offline1", "offline2"} {
		srv.store.UpsertDevice(&starlink.Device{ID: id, Kind: starlink.KindDish, Name: id})
	}
	body := read()
	for _, want := range []string{`data-count="online">0`, `data-count="offline">2`, "Snapshot ·", "unreachable"} {
		if !strings.Contains(body, want) {
			t.Errorf("all-offline fleet missing %q", want)
		}
	}
}

func TestPresentationRoutesAndRouterlessAccount(t *testing.T) {
	srv, ts := newTestServer(t)
	seedDevice(t, srv.store)
	for _, tc := range []struct {
		path   string
		status int
		text   string
	}{
		{"/clients", 200, "No Starlink routers found"},
		{"/missing", 404, "<title>404 · Page not found"},
		{"/metrics", 404, "separate private metrics listener"},
		{"/device/unknown", 404, "<title>404 · Unknown device"},
		{"/api/device/ut1234/clients", 400, "not a router"},
	} {
		t.Run(tc.path, func(t *testing.T) {
			r, err := http.Get(ts.URL + tc.path)
			if err != nil {
				t.Fatal(err)
			}
			defer r.Body.Close()
			if r.StatusCode != tc.status {
				t.Fatalf("status = %d", r.StatusCode)
			}
			if body := readAll(t, r); !strings.Contains(body, tc.text) {
				t.Fatalf("missing %q", tc.text)
			}
		})
	}
}

func TestCloudOnlyAndMissingGatewayFields(t *testing.T) {
	srv, _ := newTestServer(t)
	d := &starlink.Device{ID: "cloud-only", Kind: starlink.KindDish, Telemetry: map[string]any{
		"softwareVersion": "cloud-v2", "signalQuality": float64(0),
		"uptimeSeconds": float64(42), "activeAlerts": []string{"no_ethernet_link", "thermal_shutdown"},
	}}
	rows := map[string]string{}
	for _, row := range srv.statusRows(d) {
		rows[row.Field] = row.Value
	}
	for field, want := range map[string]string{"versions": "cloud-v2", "signalQuality": "0.00", "uptime": "42s"} {
		if rows[field] != want {
			t.Errorf("%s = %q, want %q", field, rows[field], want)
		}
	}
	if !strings.Contains(rows["alerts"], "no Ethernet link") {
		t.Error("cloud-only alert lost")
	}
	if len(srv.deviceView(d, time.Now()).Alerts) != 2 {
		t.Error("dashboard lost cloud-only alerts")
	}
	d.Status = &starlink.DeviceStatus{Reported: map[string]string{"obstruction_stats.last_24h_obstructed_s": "0", "device_state.uptime_s": "0"}}
	rows = map[string]string{}
	for _, row := range srv.statusRows(d) {
		rows[row.Field] = row.Value
	}
	if rows["obstruction24h"] != "0 s" || rows["uptime"] != "0s" {
		t.Fatalf("reported zeros lost: %v", rows)
	}
	if rows["ping"] != "not reported" || rows["scheduledFlag"] != "no recent history" {
		t.Fatal("missing measurements presented as zero")
	}
	view := srv.deviceView(d, time.Now())
	if view.PingText != "—" || view.ObstructionText != "not reported" {
		t.Fatal("missing fields shown as measurements")
	}
}

func TestRetiredDeviceAndSourceDetails(t *testing.T) {
	srv, ts := newTestServer(t)
	now := time.Now()
	d := &starlink.Device{ID: "retired", Name: "old kit", Kind: starlink.KindDish, LastSeen: now, Retired: &now,
		RosterDetails: map[string]string{"Location nickname": "<long nickname>"},
		Status:        &starlink.DeviceStatus{Reported: map[string]string{"device_info.country_code": "DE", "stow_requested": "false"}},
		Telemetry:     map[string]any{"downlinkThroughputMbps": float64(1.25), "timestamp": now.UnixNano()},
	}
	srv.store.UpsertDevice(d)
	v := srv.deviceView(d, now)
	if v.HealthLabel != "retired" || v.OnlineLabel != "online" {
		t.Fatal("retirement must not change API reachability")
	}
	r, err := http.Get(ts.URL + "/device/retired")
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	body := readAll(t, r)
	for _, want := range []string{"history retained for 24 hours", "&lt;long nickname&gt;", "device_info.country_code", "Downlink (cloud, Mbps)", "Cloud snapshot (UTC)", `data-chart="snr"`} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q", want)
		}
	}
}

func TestRouterDiagnosticsDoNotShowDishFields(t *testing.T) {
	srv, ts := newTestServer(t)
	d := &starlink.Device{ID: "router", Name: "router", Kind: starlink.KindRouter,
		Status: &starlink.DeviceStatus{UptimeS: 15, Wifi: &starlink.WifiStatusExtra{SerialNumber: "SERIAL", SKU: "SKU", CaptivePortal: true}},
	}
	srv.store.UpsertDevice(d)
	srv.store.SetClients(d.ID, nil)
	r, err := http.Get(ts.URL + "/device/router")
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	body := readAll(t, r)
	for _, want := range []string{"SERIAL", "SKU", "Captive portal enabled", "15s", "Full client diagnostics"} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q", want)
		}
	}
	if strings.Contains(body, "Stow requested") || strings.Contains(body, "Obstruction validity") {
		t.Error("dish measurements shown on router")
	}
}
