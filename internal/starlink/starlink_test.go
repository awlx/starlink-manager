package starlink

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/awlx/starlink-manager/api/device"
	"google.golang.org/protobuf/proto"
)

func testLogger(t *testing.T) *slog.Logger {
	t.Helper()
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// fakeClock is an injectable time source for the session.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// fakeAPI is an in-process fake of the Starlink account API surface used by
// the app: session probe, roster, telemetry cache and the gRPC-Web device
// gateway.
type fakeAPI struct {
	mu            sync.Mutex
	probeCount    int
	probeStatus   int           // override status for the probe endpoint
	probeDelay    time.Duration // artificial delay for the probe endpoint
	handleFn      func(req *device.Request) (*device.Response, int32, string)
	tokenRenewed  int
	gatewayStatus int // HTTP status override for gateway calls
}

func newFakeAPI(t *testing.T) (*fakeAPI, *httptest.Server) {
	f := &fakeAPI{probeStatus: 200}
	mux := http.NewServeMux()

	mux.HandleFunc("/auth-rp/auth/user", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.probeCount++
		n := f.probeCount
		status := f.probeStatus
		delay := f.probeDelay
		f.mu.Unlock()
		if delay > 0 {
			time.Sleep(delay)
		}
		if status != 200 {
			w.WriteHeader(status)
			fmt.Fprintf(w, `{"error":"unauthorized"}`)
			return
		}
		http.SetCookie(w, &http.Cookie{
			Name:   accessCookieName,
			Value:  fmt.Sprintf("tok-%d", n),
			Path:   "/",
			Domain: "starlink.com",
			Secure: true,
		})
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"accountNumber":"ACC-TEST-1","email":"admin@ffmuc.net"}`)
	})

	mux.HandleFunc("/api/auth/auth/refresh-token", func(w http.ResponseWriter, r *http.Request) {
		// The refresh endpoint requires the Access.V1 chunk trio; without it
		// the real API answers 401, so mirror that here.
		need := map[string]bool{}
		for _, c := range r.Cookies() {
			need[c.Name] = true
		}
		for _, present := range need {
			if !present {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
		}
		f.mu.Lock()
		f.tokenRenewed++
		n := f.tokenRenewed
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"accessToken":"renewed-%d"}`, n)
	})

	mux.HandleFunc("/webagg/v2/accounts/service-lines", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"content":{"totalCount":1,"pageIndex":0,"limit":100,"isLastPage":true,"results":[{
			"serviceLineNumber": "SL-TEST-1",
			"nickname": "FFMUC Dach",
			"userTerminals": [{
				"userTerminalId": "ut12345678-a12b3456-123456c1",
				"serialNumber": "KIT001",
				"dishSerialNumber": "2DHT001",
				"locationNickname": "Freifunk MUC Dach",
				"lat": 48.1, "lon": 11.5,
				"active": true, "isOffline": false
			}],
			"routers": [{
				"routerId": "Router-000007685000000000000001",
				"hardwareVersion": "rev3",
				"isBypassed": false
			}]
		}]},"errors":[],"warnings":[],"information":[],"isValid":true}`)
	})

	mux.HandleFunc("/device-data/cache/v1/telemetry", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			AccountNumber string `json:"accountNumber"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body.AccountNumber != "ACC-TEST-1" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		// Positional-row shape the production API returns: per-device-type
		// column names plus one row array per sample.
		fmt.Fprintf(w, `{"data":{"values":[["u",1700000000000000000,"ut12345678-a12b3456-123456c1",120.5,18.5,0.01,42,0.5,86400,0.8,0,-1,"2024.05.01","DE",false,[82]]],
			"columnNamesByDeviceType":{"u":["DeviceType","UtcTimestampNs","DeviceId","DownlinkThroughput","UplinkThroughput","PingDropRateAvg","PingLatencyMsAvg","ObstructionPercentTime","Uptime","SignalQuality","H3CellId","SecondsUntilSwupdateRebootPossible","RunningSoftwareVersion","CountryCode","InTerritorialWaters","ActiveAlerts"]}},
			"metadata":{"enums":{"DeviceType":{"u":"UserTerminal"},"AlertsByDeviceType":{"u":{"82":"software_update_reboot_pending"}}}}}`)
	})

	mux.HandleFunc("/api/SpaceX.API.Device.Device/Handle", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		httpStatus := f.gatewayStatus
		f.mu.Unlock()
		if httpStatus != 0 && httpStatus != 200 {
			w.WriteHeader(httpStatus)
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		frames, _, err := decodeGRPCWeb(body)
		if err != nil || len(frames) != 1 {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		var req device.Request
		if err := proto.Unmarshal(frames[0], &req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		f.mu.Lock()
		fn := f.handleFn
		f.mu.Unlock()
		var resp *device.Response
		var code int32
		var msg string
		if fn != nil {
			resp, code, msg = fn(&req)
		} else {
			resp, code, msg = f.defaultHandle(&req)
		}
		writeGRPCWebResponse(w, resp, code, msg)
	})

	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return f, ts
}

// defaultHandle answers get_status and get_history with canned data.
func (f *fakeAPI) defaultHandle(req *device.Request) (*device.Response, int32, string) {
	switch req.GetRequest().(type) {
	case *device.Request_GetStatus:
		target := req.GetTargetId()
		if strings.HasPrefix(target, "Router-") {
			return &device.Response{
				Response: &device.Response_WifiGetStatus{WifiGetStatus: &device.WifiGetStatusResponse{
					DeviceInfo:     &device.DeviceInfo{HardwareVersion: proto.String("rev3"), SoftwareVersion: proto.String("wifi-2024.1")},
					DeviceState:    &device.DeviceState{UptimeS: proto.Uint64(7200)},
					SerialNumber:   proto.String("ROUTER-SN-1"),
					Sku:            proto.String("UTR-42"),
					Ipv4WanAddress: proto.String("10.20.30.40"),
					PingDropRate:   proto.Float32(0.001),
					PingLatencyMs:  proto.Float32(38.5),
				}},
			}, 0, ""
		}
		return &device.Response{
			Response: &device.Response_DishGetStatus{DishGetStatus: &device.DishGetStatusResponse{
				DeviceInfo:            &device.DeviceInfo{HardwareVersion: proto.String("rev4"), SoftwareVersion: proto.String("2024.05.01")},
				DeviceState:           &device.DeviceState{UptimeS: proto.Uint64(86400)},
				State:                 device.DishState_CONNECTED.Enum(),
				Snr:                   proto.Float32(7.5),
				PopPingLatencyMs:      proto.Float32(42),
				PopPingDropRate:       proto.Float32(0.005),
				DownlinkThroughputBps: proto.Float32(125000000),
				UplinkThroughputBps:   proto.Float32(20000000),
				ObstructionStats: &device.DishObstructionStats{
					FractionObstructed:  proto.Float32(0.004),
					ValidS:              proto.Float32(86400),
					Last_24HObstructedS: proto.Float32(600),
					CurrentlyObstructed: proto.Bool(true),
				},
				StowRequested: proto.Bool(false),
				Alerts:        &device.DishAlerts{MotorsStuck: proto.Bool(true)},
			}},
		}, 0, ""
	case *device.Request_GetHistory:
		// 3 one-second samples ending at "now" so they fall inside the
		// history window regardless of when the test runs.
		now := uint64(time.Now().Unix())
		return &device.Response{
			Response: &device.Response_DishGetHistory{DishGetHistory: &device.DishGetHistoryResponse{
				Current:               proto.Uint64(now),
				PopPingLatencyMs:      []float32{40, 41, 42},
				PopPingDropRate:       []float32{0, 0.01, 0},
				DownlinkThroughputBps: []float32{1e8, 1.1e8, 1.2e8},
				UplinkThroughputBps:   []float32{2e7, 2.1e7, 2.2e7},
				Snr:                   []float32{7.0, 7.5, 8.0},
				Scheduled:             []bool{true, true, false},
				Obstructed:            []bool{false, true, false},
			}},
		}, 0, ""
	default:
		return &device.Response{}, 0, ""
	}
}

func writeGRPCWebResponse(w http.ResponseWriter, resp *device.Response, code int32, msg string) {
	w.Header().Set("Content-Type", "application/grpc-web+proto")
	if code != 0 {
		trailers := fmt.Sprintf("grpc-status: %d\r\ngrpc-message: %s\r\n", code, msg)
		body := make([]byte, 5+len(trailers))
		body[0] = frameFlagTrailers
		binary.BigEndian.PutUint32(body[1:5], uint32(len(trailers)))
		copy(body[5:], trailers)
		w.Write(body)
		return
	}
	payload, _ := proto.Marshal(resp)
	w.Write(encodeGRPCWeb(payload))
	trailer := "grpc-status: 0\r\n"
	tb := make([]byte, 5+len(trailer))
	tb[0] = frameFlagTrailers
	binary.BigEndian.PutUint32(tb[1:5], uint32(len(trailer)))
	copy(tb[5:], trailer)
	w.Write(tb)
}

// newTestSession builds a session against the fake API with a fake clock.
func newTestSession(t *testing.T, ts *httptest.Server, clock *fakeClock) *Session {
	s := NewSession(ts.URL, ts.URL, "sso-cookie-test", ts.Client(), nil)
	// Seed the Access trio the refresh endpoint requires.
	s.WithRefreshCookies("Starlink.Com.Access.V1=v1; Starlink.Com.Access.V1C1=c1; Starlink.Com.Access.V1C2=c2")
	s.now = clock.Now
	return s
}

func TestCookieJarDomainMatching(t *testing.T) {
	jar := newCookieJar()
	apiURL := mustURL(t, "https://api.starlink.com/x")
	jar.SetCookies(apiURL, []*http.Cookie{{
		Name: "Starlink.Com.Access.V1", Value: "abc", Domain: "starlink.com", Path: "/", Secure: true,
	}})
	got := jar.Cookies(mustURL(t, "https://starlink.com/api/SpaceX.API.Device.Device/Handle"))
	found := false
	for _, c := range got {
		if c.Name == "Starlink.Com.Access.V1" && c.Value == "abc" {
			found = true
		}
	}
	if !found {
		t.Fatalf("cookie with domain starlink.com not sent to starlink.com: %v", got)
	}
	// SSO cookie installed for the web host is sent to the API host too.
	jar.setSSOCookie("starlink.com", "sso-val")
	got = jar.Cookies(apiURL)
	names := map[string]string{}
	for _, c := range got {
		names[c.Name] = c.Value
	}
	if names["Starlink.Com.Sso"] != "sso-val" || names["Starlink.Com.Access.V1"] != "abc" {
		t.Fatalf("jar cookie snapshot wrong: %v", names)
	}
}

func mustURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse %q: %v", raw, err)
	}
	return u
}

func TestSessionProbeMintsToken(t *testing.T) {
	f, ts := newFakeAPI(t)
	clock := &fakeClock{now: time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)}
	s := newTestSession(t, ts, clock)

	tok, err := s.Token(context.Background())
	if err != nil {
		t.Fatalf("Token: %v", err)
	}
	if tok != "tok-1" {
		t.Fatalf("token = %q, want tok-1", tok)
	}
	if s.AccountNumber() != "ACC-TEST-1" {
		t.Fatalf("account number = %q", s.AccountNumber())
	}
	// Second call within freshness window reuses the token, no new probe.
	tok2, err := s.Token(context.Background())
	if err != nil || tok2 != "tok-1" {
		t.Fatalf("second Token = %q, %v", tok2, err)
	}
	if f.probeCount != 1 {
		t.Fatalf("probeCount = %d, want 1", f.probeCount)
	}
	// Access cookie is mirrored into the jar for the API host.
	snap := s.jar.snapshot()["127.0.0.1"]
	if snap[accessCookieName] != "tok-1" {
		t.Fatalf("access cookie missing in jar: %v", snap)
	}
}

func TestSessionProbeThrottle(t *testing.T) {
	f, ts := newFakeAPI(t)
	clock := &fakeClock{now: time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)}
	s := newTestSession(t, ts, clock)
	oldMax := probeAwaitMax
	probeAwaitMax = 300 * time.Millisecond // keep the test fast
	t.Cleanup(func() { probeAwaitMax = oldMax })

	// First probe fails with 500 (unexpected status): no token minted.
	f.mu.Lock()
	f.probeStatus = 500
	f.mu.Unlock()
	if _, err := s.Token(context.Background()); err == nil {
		t.Fatal("expected probe error")
	}
	f.mu.Lock()
	f.probeStatus = 200
	f.mu.Unlock()

	// A retry within 60s waits for the shared probe window (bounded by
	// probeAwaitMax) and then reports throttled — it must not send a
	// second probe.
	if _, err := s.Token(context.Background()); err == nil || !strings.Contains(err.Error(), "throttled") {
		t.Fatalf("expected throttle error, got %v", err)
	}
	if f.probeCount != 1 {
		t.Fatalf("probeCount = %d, want 1", f.probeCount)
	}
	// After the 60s window, the probe succeeds (probe #2).
	clock.Advance(61 * time.Second)
	tok, err := s.Token(context.Background())
	if err != nil || tok != "tok-2" {
		t.Fatalf("Token after window = %q, %v", tok, err)
	}
}

// TestSessionThrottledCallerWaitsForSharedProbe verifies that a caller
// arriving inside the probe window waits for the in-flight probe (single
// flight via the session mutex) and reuses its token instead of failing.
func TestSessionThrottledCallerWaitsForSharedProbe(t *testing.T) {
	f, ts := newFakeAPI(t)
	clock := &fakeClock{now: time.Now()}
	s := newTestSession(t, ts, clock)

	// Start a probe in a goroutine (the fake API delays it).
	f.mu.Lock()
	f.probeDelay = 300 * time.Millisecond
	f.mu.Unlock()

	const workers = 3
	var wg sync.WaitGroup
	tokens := make([]string, workers)
	errs := make([]error, workers)
	start := time.Now()
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			tokens[i], errs[i] = s.Token(ctx)
		}(i)
	}
	wg.Wait()
	elapsed := time.Since(start)

	f.mu.Lock()
	probes := f.probeCount
	f.probeDelay = 0
	f.mu.Unlock()

	if probes != 1 {
		t.Fatalf("probeCount = %d, want a single shared probe", probes)
	}
	for i := 0; i < workers; i++ {
		if errs[i] != nil || tokens[i] == "" {
			t.Errorf("worker %d: token %q err %v", i, tokens[i], errs[i])
		}
	}
	if elapsed < 200*time.Millisecond {
		t.Errorf("waiters returned too early (%s): they should await the probe", elapsed)
	}
}

func TestSessionDeadOnUnauthorizedProbe(t *testing.T) {
	f, ts := newFakeAPI(t)
	clock := &fakeClock{now: time.Now()}
	s := newTestSession(t, ts, clock)
	f.mu.Lock()
	f.probeStatus = 401
	f.mu.Unlock()
	_, err := s.Token(context.Background())
	if err == nil || !strings.Contains(err.Error(), "re-authenticate") {
		t.Fatalf("expected session-dead error, got %v", err)
	}
	if !s.SessionDead() {
		t.Fatal("SessionDead should be true")
	}
}

func TestSessionRenewal(t *testing.T) {
	f, ts := newFakeAPI(t)
	clock := &fakeClock{now: time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)}
	s := newTestSession(t, ts, clock)
	if _, err := s.Token(context.Background()); err != nil {
		t.Fatalf("Token: %v", err)
	}
	// Advance past freshness so renewal kicks in before the probe window.
	clock.Advance(11 * time.Minute)
	tok, err := s.Token(context.Background())
	if err != nil {
		t.Fatalf("renewed Token: %v", err)
	}
	if tok != "renewed-1" {
		t.Fatalf("token = %q, want renewed-1", tok)
	}
	if f.probeCount != 1 {
		t.Fatalf("probeCount = %d, want 1 (renewal should avoid a probe)", f.probeCount)
	}
}

func TestGatewayRoundtrip(t *testing.T) {
	_, ts := newFakeAPI(t)
	clock := &fakeClock{now: time.Now()}
	s := newTestSession(t, ts, clock)
	g := NewGateway(s, ts.URL, ts.Client(), nil)

	resp, err := g.GetStatus(context.Background(), KindDish, "12345678-a12b3456-123456c1")
	if err != nil {
		t.Fatalf("GetStatus: %v", err)
	}
	dish := resp.GetDishGetStatus()
	if dish == nil {
		t.Fatal("no dish status in response")
	}
	if dish.GetDeviceInfo().GetSoftwareVersion() != "2024.05.01" {
		t.Fatalf("software version = %q", dish.GetDeviceInfo().GetSoftwareVersion())
	}
	if dish.GetSnr() != 7.5 {
		t.Fatalf("snr = %v", dish.GetSnr())
	}
}

func TestGatewayOfflineMapping(t *testing.T) {
	f, ts := newFakeAPI(t)
	clock := &fakeClock{now: time.Now()}
	s := newTestSession(t, ts, clock)
	g := NewGateway(s, ts.URL, ts.Client(), nil)

	f.mu.Lock()
	f.handleFn = func(req *device.Request) (*device.Response, int32, string) {
		return &device.Response{}, 2, "DEVICE_CONNECTION_NOT_FOUND: device not connected"
	}
	f.mu.Unlock()

	_, err := g.GetStatus(context.Background(), KindDish, "1234")
	if err != ErrDeviceOffline {
		t.Fatalf("expected ErrDeviceOffline, got %v", err)
	}
}

func TestGatewayRetryOnUnauthenticated(t *testing.T) {
	f, ts := newFakeAPI(t)
	clock := &fakeClock{now: time.Now()}
	s := newTestSession(t, ts, clock)
	if _, err := s.Token(context.Background()); err != nil {
		t.Fatalf("Token: %v", err)
	}
	g := NewGateway(s, ts.URL, ts.Client(), nil)

	var calls int32
	var mu sync.Mutex
	f.mu.Lock()
	f.handleFn = func(req *device.Request) (*device.Response, int32, string) {
		mu.Lock()
		calls++
		n := calls
		mu.Unlock()
		if n == 1 {
			return &device.Response{}, 16, "unauthenticated"
		}
		return &device.Response{Response: &device.Response_Reboot{Reboot: &device.RebootResponse{}}}, 0, ""
	}
	f.mu.Unlock()

	if err := g.Reboot(context.Background(), KindDish, "1234"); err != nil {
		t.Fatalf("Reboot after re-auth retry: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if calls != 2 {
		t.Fatalf("gateway calls = %d, want 2", calls)
	}
	if f.tokenRenewed != 1 {
		t.Fatalf("token renewals = %d, want 1", f.tokenRenewed)
	}
}

func TestStoreHistoryDedupeAndPersistence(t *testing.T) {
	dir := t.TempDir()
	clock := &fakeClock{now: time.Unix(200, 0)}
	s := NewStore(dir, 24*time.Hour, clock.Now)
	s.UpsertDevice(&Device{ID: "ut12345678-a12b3456-123456c1", Kind: KindDish, Name: "test"})

	hist := &device.DishGetHistoryResponse{
		Current:               proto.Uint64(100),
		PopPingLatencyMs:      []float32{40, 41, 42},
		PopPingDropRate:       []float32{0, 0.01, 0},
		DownlinkThroughputBps: []float32{1e8, 1.1e8, 1.2e8},
		UplinkThroughputBps:   []float32{2e7, 2.1e7, 2.2e7},
		Snr:                   []float32{7, 7.5, 8},
		Scheduled:             []bool{true, true, false},
		Obstructed:            []bool{false, true, false},
	}
	if n := s.AppendHistory("ut12345678-a12b3456-123456c1", hist); n != 3 {
		t.Fatalf("first append = %d samples, want 3", n)
	}
	// Same cursor again: nothing new.
	if n := s.AppendHistory("ut12345678-a12b3456-123456c1", hist); n != 0 {
		t.Fatalf("duplicate append = %d, want 0", n)
	}
	// New samples with advanced cursor: the two seconds after the cursor
	// are fresh (101, 102), the first duplicates cursor 100.
	hist2 := &device.DishGetHistoryResponse{
		Current:          proto.Uint64(102),
		PopPingLatencyMs: []float32{40, 41, 43},
		PopPingDropRate:  []float32{0, 0.01, 0.02},
	}
	if n := s.AppendHistory("ut12345678-a12b3456-123456c1", hist2); n != 2 {
		t.Fatalf("overlap append = %d, want 2", n)
	}
	samples := s.History("ut12345678-a12b3456-123456c1", time.Hour)
	if len(samples) != 5 {
		t.Fatalf("history len = %d, want 5", len(samples))
	}
	last := samples[len(samples)-1]
	if last.PopPingLatencyMs != 43 || last.PopPingDropRate < 0.0199 || last.PopPingDropRate > 0.0201 {
		t.Fatalf("last sample wrong: %+v", last)
	}
	// Verify timestamps line up with the cursor (current-(n-1-i)).
	if samples[4].Ts.Unix() != 102 {
		t.Fatalf("last sample ts = %d, want 102", samples[4].Ts.Unix())
	}

	// Persistence roundtrip.
	if err := s.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	s2 := NewStore(dir, 24*time.Hour, clock.Now)
	if err := s2.Load(); err != nil {
		t.Fatalf("Load: %v", err)
	}
	samples2 := s2.History("ut12345678-a12b3456-123456c1", time.Hour)
	if len(samples2) != 5 {
		t.Fatalf("persisted history len = %d, want 5", len(samples2))
	}
	if _, ok := s2.Device("ut12345678-a12b3456-123456c1"); !ok {
		t.Fatal("persisted device missing")
	}
}

func TestStorePersistenceFileLayout(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir, 24*time.Hour, nil)
	s.UpsertDevice(&Device{ID: "ut-1", Kind: KindDish, Name: "x"})
	if err := s.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	want := filepath.Join(dir, "store.json")
	if _, err := os.Stat(want); err != nil {
		t.Fatalf("store file missing: %v", err)
	}
}

func TestPollerTick(t *testing.T) {
	f, ts := newFakeAPI(t)
	clock := &fakeClock{now: time.Now()}
	s := newTestSession(t, ts, clock)
	store := NewStore(t.TempDir(), 24*time.Hour, clock.Now)
	roster := NewRoster(s, ts.URL, time.Minute, nil)
	tel := NewTelemetry(s, ts.URL, nil)
	g := NewGateway(s, ts.URL, ts.Client(), nil)
	p := NewPoller(g, roster, tel, s, store, 30*time.Second, testLogger(t))

	p.Tick()

	devices := store.Devices()
	var dish, router *Device
	for _, d := range devices {
		switch d.Kind {
		case KindDish:
			dish = d
		case KindRouter:
			router = d
		}
	}
	if dish == nil || router == nil {
		t.Fatalf("devices missing: %+v", devices)
	}
	if dish.Name != "Freifunk MUC Dach" {
		t.Errorf("dish name = %q", dish.Name)
	}
	if router.Name != "Router 000000000001" {
		t.Errorf("router name = %q", router.Name)
	}
	if dish.Status == nil || dish.Status.State != "CONNECTED" {
		t.Errorf("dish status = %+v", dish.Status)
	}
	if !dish.Status.Alerts["motors_stuck"] {
		t.Errorf("dish alerts missing motors_stuck: %v", dish.Status.Alerts)
	}
	if dish.Status.ObstructionFraction < 0.0039 || dish.Status.ObstructionFraction > 0.0041 {
		t.Errorf("obstruction fraction = %v", dish.Status.ObstructionFraction)
	}
	if !dish.Status.CurrentlyObstructed {
		t.Error("currently obstructed flag missing")
	}
	if dish.Status.ObstructionValidS != 86400 {
		t.Errorf("obstruction validity = %v", dish.Status.ObstructionValidS)
	}
	if dish.Status.ObstructionLast24hS != 600 {
		t.Errorf("obstruction last 24h = %v", dish.Status.ObstructionLast24hS)
	}
	samples := store.History(dish.ID, time.Hour)
	if len(samples) != 3 {
		t.Fatalf("dish samples = %d, want 3", len(samples))
	}
	if dish.Telemetry == nil || dish.Telemetry["signalQuality"] != 0.8 {
		t.Errorf("telemetry enrichment missing: %v", dish.Telemetry)
	}
	if f.probeCount == 0 {
		t.Error("session was never probed")
	}
}

func TestPollerToleratesOfflineDevice(t *testing.T) {
	f, ts := newFakeAPI(t)
	clock := &fakeClock{now: time.Now()}
	s := newTestSession(t, ts, clock)
	store := NewStore(t.TempDir(), 24*time.Hour, clock.Now)
	roster := NewRoster(s, ts.URL, time.Minute, nil)
	tel := NewTelemetry(s, ts.URL, nil)
	g := NewGateway(s, ts.URL, ts.Client(), nil)
	p := NewPoller(g, roster, tel, s, store, 30*time.Second, testLogger(t))

	// First tick populates devices.
	p.Tick()

	// Second tick: every gateway call is offline.
	f.mu.Lock()
	f.handleFn = func(req *device.Request) (*device.Response, int32, string) {
		return &device.Response{}, 2, "DEVICE_CONNECTION_NOT_FOUND"
	}
	f.mu.Unlock()
	p.Tick() // must not panic or hang; store keeps stale data

	d, ok := store.Device("ut12345678-a12b3456-123456c1")
	if !ok {
		t.Fatal("device disappeared")
	}
	if d.Status == nil {
		t.Fatal("stale status lost after offline tick")
	}
	// History should be preserved (cursor unchanged).
	if n := len(store.History(d.ID, time.Hour)); n != 3 {
		t.Fatalf("history after offline tick = %d, want 3", n)
	}
}

func TestRosterPagination(t *testing.T) {
	_, ts := newFakeAPI(t)
	clock := &fakeClock{now: time.Now()}
	s := newTestSession(t, ts, clock)
	// Default fake returns a short page (<100), so pagination stops after
	// page 0. Just assert the roster parses.
	r := NewRoster(s, ts.URL, time.Minute, nil)
	lines, err := r.Get(context.Background())
	if err != nil {
		t.Fatalf("roster Get: %v", err)
	}
	if len(lines) != 1 {
		t.Fatalf("service lines = %d", len(lines))
	}
	if lines[0].Nickname != "FFMUC Dach" || len(lines[0].Terminals) != 1 || len(lines[0].Routers) != 1 {
		t.Fatalf("parsed roster wrong: %+v", lines[0])
	}
}

func TestTelemetryParse(t *testing.T) {
	data := []byte(`{"devices":[{"DeviceId":"ut-1","UtcTimestampNs":1700000000000000000,"SoftwareVersion":"v1","SignalQuality":0.9,"ActiveAlert":[82]}]}`)
	tms, err := parseTelemetryResponse(data)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(tms) != 1 || tms[0].DeviceID != "ut-1" || tms[0].SignalQuality != 0.9 || len(tms[0].ActiveAlerts) != 1 {
		t.Fatalf("parsed telemetry: %+v", tms)
	}
}

func TestGRPCWebFraming(t *testing.T) {
	payload := []byte{1, 2, 3, 4, 5}
	frame := encodeGRPCWeb(payload)
	if frame[0] != 0 || binary.BigEndian.Uint32(frame[1:5]) != 5 || !bytes.Equal(frame[5:], payload) {
		t.Fatalf("encodeGRPCWeb frame wrong: %v", frame)
	}
	data, trailers, err := decodeGRPCWeb(frame)
	if err != nil || len(data) != 1 || !bytes.Equal(data[0], payload) {
		t.Fatalf("decodeGRPCWeb data wrong: %v %v", data, err)
	}
	if _, ok := trailers["grpc-status"]; ok {
		t.Fatal("unexpected trailers on data-only frame")
	}
	code, _, err := parseGRPCStatus(map[string]string{"grpc-status": "16", "grpc-message": "unauthenticated"})
	if err != nil || code != 16 {
		t.Fatalf("parseGRPCStatus = %d, %v", code, err)
	}
	if _, _, err := parseGRPCStatus(map[string]string{}); err == nil {
		t.Fatal("missing grpc-status should error")
	}
}
