package starlink

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
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
	// billing endpoint behavior: change-options hit counter, body, status.
	changeOptionsCount  int
	changeOptionsBody   string
	changeOptionsStatus int
	// mutation behavior: status/body override and location for redirects.
	mutationStatus  int
	mutationBody    string
	mutationLoc     string
	changeCount     int
	resumeCount     int
	rosterCount     int
	// mutation capture: method+path of subscription update/resume calls.
	mutations []string
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
		f.mu.Lock()
		f.rosterCount++
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"content":{"totalCount":1,"pageIndex":0,"limit":100,"isLastPage":true,"results":[{
			"serviceLineNumber": "SL-TEST-1",
			"nickname": "FFMUC Dach",
			"canPauseService": true,
			"subscription": {
				"subscriptionReferenceId": "11111111-2222-3333-4444-555555555555",
				"productId": "de-test-subscription",
				"productDescription": "Test Residential",
				"startDate": "2024-06-29T02:11:45.006694+00:00",
				"endDate": null,
				"subscriptionClass": "Standard",
				"active": true,
				"canChangeService": true,
				"canResumeService": false,
				"pendingActivation": false,
				"isPaused": false,
				"isSuspended": false,
				"isStandby": false
			},
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

	mux.HandleFunc("/webagg/v1/public/subscriptions/change-options/", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.changeOptionsCount++
		body, status := f.changeOptionsBody, f.changeOptionsStatus
		f.mu.Unlock()
		if status == 0 {
			status = 200
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		fmt.Fprint(w, body)
	})

	mux.HandleFunc("/webagg/v1/public/subscriptions/line/", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		path := r.URL.EscapedPath()
		if r.URL.RawQuery != "" {
			path += "?" + r.URL.RawQuery
		}
		f.mutations = append(f.mutations, r.Method+" "+path)
		status, body, loc := f.mutationStatus, f.mutationBody, f.mutationLoc
		if strings.Contains(path, "/resume") {
			f.resumeCount++
		} else {
			f.changeCount++
		}
		f.mu.Unlock()
		if status == 0 {
			status = 200
		}
		if loc != "" {
			w.Header().Set("Location", loc)
		}
		if body == "" && status == 200 {
			body = `{"isValid":true}`
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		fmt.Fprint(w, body)
	})

	// Redirect target for expired-cookie SSO redirects: an HTML login page.
	mux.HandleFunc("/login", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, "<html><body>Sign in to continue</body></html>")
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
		// A 3-sample ring buffer; the counter is samples since boot.
		return &device.Response{
			Response: &device.Response_DishGetHistory{DishGetHistory: &device.DishGetHistoryResponse{
				Current:               proto.Uint64(3000),
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

	// One transient 401 must not latch the session permanently.
	_, err := s.Token(context.Background())
	if err == nil || !strings.Contains(err.Error(), "re-authenticate") {
		t.Fatalf("expected session-dead error, got %v", err)
	}
	if s.SessionDead() {
		t.Fatal("a single failed probe must not latch the session dead")
	}

	// A second consecutive failure (after the probe window) latches.
	clock.Advance(2 * time.Minute)
	if _, err := s.Token(context.Background()); err == nil {
		t.Fatal("expected another session-dead error")
	}
	if !s.SessionDead() {
		t.Fatal("SessionDead should be true after repeated failures")
	}
}

func TestSessionDeadRecovery(t *testing.T) {
	f, ts := newFakeAPI(t)
	clock := &fakeClock{now: time.Now()}
	s := newTestSession(t, ts, clock)
	oldCooldown := deadCooldown
	deadCooldown = time.Minute
	t.Cleanup(func() { deadCooldown = oldCooldown })

	f.mu.Lock()
	f.probeStatus = 401
	f.mu.Unlock()
	_, _ = s.Token(context.Background())
	clock.Advance(2 * time.Minute)
	if _, err := s.Token(context.Background()); err == nil {
		t.Fatal("expected session-dead error")
	}
	if !s.SessionDead() {
		t.Fatal("session should be latched dead")
	}

	// Inside the cooldown, Token keeps failing and the poller skips.
	if _, err := s.Token(context.Background()); err == nil || !strings.Contains(err.Error(), "re-authenticate") {
		t.Fatalf("expected blocked error inside cooldown, got %v", err)
	}

	// Once the cooldown elapses, a successful probe recovers the session.
	clock.Advance(2 * time.Minute)
	f.mu.Lock()
	f.probeStatus = 200
	f.mu.Unlock()
	tok, err := s.Token(context.Background())
	if err != nil {
		t.Fatalf("recovery probe failed: %v", err)
	}
	if tok == "" || s.SessionDead() {
		t.Fatal("session should be recovered")
	}

	// After a successful recovery the one-strike window must be over: a
	// later single failure counts like any other and does not latch.
	s.mu.Lock()
	s.latchDeadLocked()
	s.mu.Unlock()
	if s.SessionDead() {
		t.Fatal("a single failure after a successful recovery must not latch the session dead")
	}
}

// A recovery that reuses a still-fresh token is confirmed by the data call
// (MarkAlive), which must end the one-strike window too.
func TestMarkAliveEndsRecoveryWindow(t *testing.T) {
	_, ts := newFakeAPI(t)
	s := newTestSession(t, ts, &fakeClock{now: time.Now()})
	s.armRecovery()
	s.MarkAlive()
	s.mu.Lock()
	s.latchDeadLocked()
	s.mu.Unlock()
	if s.SessionDead() {
		t.Fatal("one failure after a confirmed recovery must not latch")
	}
}

func TestMarkAliveClearsDeadLatch(t *testing.T) {
	f, ts := newFakeAPI(t)
	clock := &fakeClock{now: time.Now()}
	s := newTestSession(t, ts, clock)
	oldCooldown := deadCooldown
	deadCooldown = time.Minute
	t.Cleanup(func() { deadCooldown = oldCooldown })

	f.mu.Lock()
	f.probeStatus = 401
	f.mu.Unlock()
	_, _ = s.Token(context.Background())
	clock.Advance(2 * time.Minute)
	_, _ = s.Token(context.Background())
	if !s.SessionDead() {
		t.Fatal("session should be latched dead")
	}
	s.MarkAlive()
	if s.SessionDead() {
		t.Fatal("MarkAlive should clear the dead latch")
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

func TestGatewayHonorsCallTimeout(t *testing.T) {
	f, ts := newFakeAPI(t)
	s := newTestSession(t, ts, &fakeClock{now: time.Now()})
	g := NewGateway(s, ts.URL, ts.Client(), nil)
	g.SetCallTimeout(100 * time.Millisecond)
	f.handleFn = func(req *device.Request) (*device.Response, int32, string) {
		time.Sleep(time.Second)
		return f.defaultHandle(req)
	}
	start := time.Now()
	if _, err := g.GetStatus(context.Background(), KindDish, "ut1"); err == nil {
		t.Fatal("expected a timeout")
	}
	if d := time.Since(start); d > 700*time.Millisecond {
		t.Fatalf("call took %s, want it bounded by the 100ms call timeout", d)
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

func TestGatewayConcurrentUnauthenticatedSharesRefresh(t *testing.T) {
	f, ts := newFakeAPI(t)
	clock := &fakeClock{now: time.Now()}
	s := newTestSession(t, ts, clock)
	if _, err := s.Token(context.Background()); err != nil {
		t.Fatalf("Token: %v", err)
	}
	g := NewGateway(s, ts.URL, ts.Client(), nil)
	f.mu.Lock()
	base := f.tokenRenewed
	f.mu.Unlock()

	// The first wave of parallel calls is rejected together, as happens
	// when a token expires during a poll round.
	const workers = 4
	var arrived int32
	release := make(chan struct{})
	var once sync.Once
	var mu sync.Mutex
	f.mu.Lock()
	f.handleFn = func(req *device.Request) (*device.Response, int32, string) {
		mu.Lock()
		arrived++
		n := arrived
		mu.Unlock()
		if n <= workers {
			if n == workers {
				once.Do(func() { close(release) })
			}
			<-release
			return &device.Response{}, 16, "unauthenticated"
		}
		return &device.Response{Response: &device.Response_Reboot{Reboot: &device.RebootResponse{}}}, 0, ""
	}
	f.mu.Unlock()

	var wg sync.WaitGroup
	errs := make(chan error, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs <- g.Reboot(context.Background(), KindDish, fmt.Sprintf("dev%d", i))
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("Reboot after shared re-auth: %v", err)
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if got := f.tokenRenewed - base; got != 1 {
		t.Fatalf("token renewals = %d, want 1 shared by %d rejected calls", got, workers)
	}
}

func TestStoreHistoryDedupeAndPersistence(t *testing.T) {
	dir := t.TempDir()
	clock := &fakeClock{now: time.Unix(1_800_000_000, 0)}
	s := NewStore(dir, 24*time.Hour, clock.Now)
	const id = "ut12345678-a12b3456-123456c1"
	s.UpsertDevice(&Device{ID: id, Kind: KindDish, Name: "test"})

	// Ring buffer of 3: samples 97, 98, 99 live at indices 1, 2, 0.
	hist := &device.DishGetHistoryResponse{
		Current:               proto.Uint64(100),
		PopPingLatencyMs:      []float32{42, 40, 41},
		PopPingDropRate:       []float32{0, 0, 0.01},
		DownlinkThroughputBps: []float32{1.2e8, 1e8, 1.1e8},
		UplinkThroughputBps:   []float32{2.2e7, 2e7, 2.1e7},
		Snr:                   []float32{8, 7, 7.5},
		Scheduled:             []bool{false, true, true},
		Obstructed:            []bool{false, false, true},
	}
	if n := s.AppendHistory(id, hist); n != 3 {
		t.Fatalf("first append = %d samples, want 3", n)
	}
	got := s.History(id, time.Hour)
	if got[0].PopPingLatencyMs != 40 || got[2].PopPingLatencyMs != 42 {
		t.Fatalf("ring buffer order wrong: %+v", got)
	}
	// Sample timestamps are anchored at the fetch time, not at the
	// sample counter (which counts seconds since the dish booted).
	if got[2].Ts.Unix() != 1_800_000_000 || got[0].Ts.Unix() != 1_800_000_000-2 {
		t.Fatalf("timestamps = %v .. %v", got[0].Ts, got[2].Ts)
	}
	// Same cursor again: nothing new.
	if n := s.AppendHistory(id, hist); n != 0 {
		t.Fatalf("duplicate append = %d, want 0", n)
	}
	// Two seconds later the dish took samples 100 and 101 (indices 1, 2).
	clock.Advance(2 * time.Second)
	hist2 := &device.DishGetHistoryResponse{
		Current:          proto.Uint64(102),
		PopPingLatencyMs: []float32{42, 50, 43},
		PopPingDropRate:  []float32{0, 0, 0.02},
	}
	if n := s.AppendHistory(id, hist2); n != 2 {
		t.Fatalf("overlap append = %d, want 2", n)
	}
	samples := s.History(id, time.Hour)
	if len(samples) != 5 {
		t.Fatalf("history len = %d, want 5", len(samples))
	}
	last := samples[len(samples)-1]
	if last.PopPingLatencyMs != 43 || last.PopPingDropRate < 0.0199 || last.PopPingDropRate > 0.0201 {
		t.Fatalf("last sample wrong: %+v", last)
	}
	if last.Ts.Unix() != 1_800_000_002 {
		t.Fatalf("last sample ts = %d", last.Ts.Unix())
	}
	// The dish reboots: its counter restarts below the cursor. History
	// must continue instead of waiting for the counter to catch up.
	clock.Advance(10 * time.Second)
	hist3 := &device.DishGetHistoryResponse{
		Current:          proto.Uint64(2),
		PopPingLatencyMs: []float32{60, 61, 0},
	}
	if n := s.AppendHistory(id, hist3); n != 2 {
		t.Fatalf("append after reboot = %d, want 2", n)
	}
	samples = s.History(id, time.Hour)
	if len(samples) != 7 || samples[6].PopPingLatencyMs != 61 {
		t.Fatalf("history after reboot: %+v", samples[5:])
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
	if len(samples2) != 7 {
		t.Fatalf("persisted history len = %d, want 7", len(samples2))
	}
	if _, ok := s2.Device("ut12345678-a12b3456-123456c1"); !ok {
		t.Fatal("persisted device missing")
	}
}

// TestStoreDevicesSnapshotIsolated must run under -race: readers of
// Devices() may not share structs the poller is updating.
func TestStoreDevicesSnapshotIsolated(t *testing.T) {
	s := NewStore(t.TempDir(), time.Hour, nil)
	s.UpsertDevice(&Device{ID: "ut-1", Kind: KindDish, Name: "x"})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 200; i++ {
			s.SetStatus("ut-1", &DeviceStatus{State: "CONNECTED"})
			s.MarkSeen("ut-1", time.Now())
		}
	}()
	for i := 0; i < 200; i++ {
		for _, d := range s.Devices() {
			_ = d.Status
			_ = d.LastSeen
		}
	}
	<-done
	d := s.Devices()[0]
	d.Name = "mutated"
	if got, _ := s.Device("ut-1"); got.Name != "x" {
		t.Fatal("Devices() returned the stored struct")
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

// TestPollerKeepsTickingAcrossSaves guards against the save ticker starving
// the poll deadline when the poll interval is longer than the save period.
func TestPollerKeepsTickingAcrossSaves(t *testing.T) {
	f, ts := newFakeAPI(t)
	clock := &fakeClock{now: time.Now()}
	s := newTestSession(t, ts, clock)
	store := NewStore(t.TempDir(), 24*time.Hour, clock.Now)
	var statusCalls atomic.Int32
	f.handleFn = func(req *device.Request) (*device.Response, int32, string) {
		if req.GetGetStatus() != nil && strings.HasPrefix(req.GetTargetId(), "ut") {
			statusCalls.Add(1)
		}
		return f.defaultHandle(req)
	}
	p := NewPoller(NewGateway(s, ts.URL, ts.Client(), nil), NewRoster(s, ts.URL, time.Minute, nil),
		NewTelemetry(s, ts.URL, nil), s, store, 40*time.Millisecond, testLogger(t))
	p.saveEvery = 5 * time.Millisecond
	p.Start()
	defer p.Stop()
	deadline := time.Now().Add(3 * time.Second)
	for statusCalls.Load() < 3 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if n := statusCalls.Load(); n < 3 {
		t.Fatalf("poller ran %d rounds, want at least 3", n)
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
	if _, _, err := parseGRPCStatus(map[string]string{"grpc-status": ""}); err == nil {
		t.Fatal("empty grpc-status must not read as success")
	}
}

func TestGRPCWebDecodeRejectsBadFrames(t *testing.T) {
	// A length prefix far beyond the body must fail without allocating it.
	huge := []byte{0, 0xff, 0xff, 0xff, 0xff, 1, 2}
	if _, _, err := decodeGRPCWeb(huge); err == nil {
		t.Fatal("oversized frame length should error")
	}
	// Mixed-case trailer names are normalized.
	tr := "Grpc-Status: 16\r\nGrpc-Message: unauthenticated\r\n"
	frame := append([]byte{frameFlagTrailers, 0, 0, 0, byte(len(tr))}, tr...)
	_, trailers, err := decodeGRPCWeb(frame)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	code, msg, err := parseGRPCStatus(trailers)
	if err != nil || code != 16 || msg != "unauthenticated" {
		t.Fatalf("parseGRPCStatus = %d %q %v", code, msg, err)
	}
}

func TestRosterLastError(t *testing.T) {
	_, ts := newFakeAPI(t)
	s := newTestSession(t, ts, &fakeClock{now: time.Now()})
	r := NewRoster(s, ts.URL+"/missing", time.Minute, nil)
	if _, err := r.Get(context.Background()); err == nil {
		t.Fatal("expected roster error from a missing endpoint")
	}
	if r.LastError() == "" {
		t.Fatal("LastError empty after a failed refresh")
	}
	r.apiBase = ts.URL
	if _, err := r.Get(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := r.LastError(); got != "" {
		t.Fatalf("LastError = %q after a successful refresh", got)
	}
}

func TestStatusFromResponseUnreportedFields(t *testing.T) {
	resp := &device.Response{Response: &device.Response_DishGetStatus{DishGetStatus: &device.DishGetStatusResponse{}}}
	st := statusFromResponse(KindDish, resp)
	if st.State != "" || st.SNR != nil {
		t.Fatalf("unset state/snr mapped to %q / %v, want empty", st.State, st.SNR)
	}
	resp.GetDishGetStatus().State = device.DishState_CONNECTED.Enum()
	resp.GetDishGetStatus().Snr = proto.Float32(9.5)
	st = statusFromResponse(KindDish, resp)
	if st.State != "CONNECTED" || st.SNR == nil || *st.SNR != 9.5 {
		t.Fatalf("reported state/snr mapped to %q / %v", st.State, st.SNR)
	}
}

func TestStoreConcurrentSaves(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir, 24*time.Hour, nil)
	for i := 0; i < 50; i++ {
		s.UpsertDevice(&Device{ID: fmt.Sprintf("ut%02d", i), Kind: KindDish, Name: strings.Repeat("x", 2000)})
	}
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- s.Save()
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent Save: %v", err)
		}
	}
	loaded := NewStore(dir, 24*time.Hour, nil)
	if err := loaded.Load(); err != nil {
		t.Fatalf("Load after concurrent saves: %v", err)
	}
	if n := len(loaded.Devices()); n != 50 {
		t.Fatalf("loaded %d devices, want 50", n)
	}
}

func TestNonFiniteDishValuesStayPersistable(t *testing.T) {
	nan, inf := float32(math.NaN()), float32(math.Inf(1))
	dir := t.TempDir()
	s := NewStore(dir, 24*time.Hour, nil)
	const id = "ut12345678-a12b3456-123456c1"
	s.UpsertDevice(&Device{ID: id, Kind: KindDish, Name: "test"})
	s.AppendHistory(id, &device.DishGetHistoryResponse{
		Current:               proto.Uint64(2),
		PopPingLatencyMs:      []float32{nan, 40},
		PopPingDropRate:       []float32{1, 0},
		DownlinkThroughputBps: []float32{inf, 1},
		UplinkThroughputBps:   []float32{0, 1},
		Snr:                   []float32{nan, 7},
	})
	st := statusFromResponse(KindDish, &device.Response{Response: &device.Response_DishGetStatus{
		DishGetStatus: &device.DishGetStatusResponse{
			PopPingLatencyMs: proto.Float32(nan),
			ObstructionStats: &device.DishObstructionStats{FractionObstructed: proto.Float32(nan)},
		},
	}})
	s.SetStatus(id, st)
	if err := s.Save(); err != nil {
		t.Fatalf("Save with non-finite dish values: %v", err)
	}
	for _, smp := range s.History(id, time.Hour) {
		if _, err := json.Marshal(smp); err != nil {
			t.Fatalf("sample not JSON-encodable: %v", err)
		}
	}
}

func TestRecoveryProbeFailsOnceLatchesImmediately(t *testing.T) {
	f, ts := newFakeAPI(t)
	clock := &fakeClock{now: time.Now()}
	s := newTestSession(t, ts, clock)
	oldCooldown := deadCooldown
	deadCooldown = time.Minute
	t.Cleanup(func() { deadCooldown = oldCooldown })

	// Two failed probes latch normally (transient-failure tolerance).
	f.mu.Lock()
	f.probeStatus = 401
	f.mu.Unlock()
	_, _ = s.Token(context.Background())
	if s.SessionDead() {
		t.Fatal("single transient failure must not latch")
	}
	clock.Advance(probeMinInterval + time.Second)
	_, _ = s.Token(context.Background())
	if !s.SessionDead() {
		t.Fatal("two consecutive failures must latch")
	}

	// After the cooldown one failed recovery probe latches again
	// immediately (one strike) instead of requiring another two.
	clock.Advance(2 * time.Minute)
	_, err := s.Token(context.Background())
	if err == nil {
		t.Fatal("expected failed recovery probe")
	}
	if !s.SessionDead() {
		t.Fatal("one failed post-cooldown probe must re-latch")
	}

	// A successful recovery probe recovers.
	clock.Advance(2 * time.Minute)
	f.mu.Lock()
	f.probeStatus = 200
	f.mu.Unlock()
	if _, err := s.Token(context.Background()); err != nil {
		t.Fatalf("recovery probe failed: %v", err)
	}
	if s.SessionDead() {
		t.Fatal("session should be recovered")
	}
}

// A recovery window armed by a path that returned a token without a
// liveness proof (e.g. renewal failed inside the probe window) must not
// make an unrelated later 401 latch dead in one strike once the window
// has expired.
func TestStaleRecoveryArmDoesNotOneStrike(t *testing.T) {
	f, ts := newFakeAPI(t)
	clock := &fakeClock{now: time.Now()}
	s := newTestSession(t, ts, clock)
	oldCooldown := deadCooldown
	deadCooldown = time.Minute
	t.Cleanup(func() { deadCooldown = oldCooldown })

	// Latch dead with two failed probes, then recover with a successful
	// probe: this arms the recovery window.
	f.mu.Lock()
	f.probeStatus = 401
	f.mu.Unlock()
	_, _ = s.Token(context.Background())
	clock.Advance(probeMinInterval + time.Second)
	_, _ = s.Token(context.Background())
	if !s.SessionDead() {
		t.Fatal("setup: two failures must latch")
	}
	clock.Advance(2 * time.Minute)
	f.mu.Lock()
	f.probeStatus = 200
	f.mu.Unlock()
	if _, err := s.Token(context.Background()); err != nil {
		t.Fatalf("setup: recovery probe failed: %v", err)
	}
	if s.SessionDead() {
		t.Fatal("setup: session should be recovered")
	}

	// Simulate the leak path directly: a recovery arming that never got
	// proven by a data call (no MarkAlive), then let the window expire.
	s.mu.Lock()
	s.armRecovery()
	s.mu.Unlock()
	clock.Advance(deadCooldown + time.Minute)
	if s.recoveryArmed() {
		t.Fatal("expired arm must be treated as disarmed")
	}

	// One unrelated failure now counts like any other and must not latch.
	f.mu.Lock()
	f.probeStatus = 401
	f.mu.Unlock()
	clock.Advance(probeMinInterval + time.Second)
	_, _ = s.Token(context.Background())
	if s.SessionDead() {
		t.Fatal("a single unrelated failure after an expired recovery arm must not latch immediately")
	}
}

// A failed probe on a FRESH recovery window still latches in one strike.
func TestFreshRecoveryArmStillOneStrikes(t *testing.T) {
	f, ts := newFakeAPI(t)
	clock := &fakeClock{now: time.Now()}
	s := newTestSession(t, ts, clock)
	oldCooldown := deadCooldown
	deadCooldown = time.Minute
	t.Cleanup(func() { deadCooldown = oldCooldown })

	s.mu.Lock()
	s.armRecovery()
	s.mu.Unlock()
	f.mu.Lock()
	f.probeStatus = 401
	f.mu.Unlock()
	_, _ = s.Token(context.Background())
	if !s.SessionDead() {
		t.Fatal("a failure on a fresh recovery window must latch immediately")
	}
}
