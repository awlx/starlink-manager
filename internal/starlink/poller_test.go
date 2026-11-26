package starlink

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/awlx/starlink-manager/api/device"
	"google.golang.org/protobuf/proto"
)

func seededStore(t *testing.T, n int) (*Store, []*Device) {
	t.Helper()
	store := NewStore(t.TempDir(), 24*time.Hour, nil)
	for i := range n {
		store.UpsertDevice(&Device{ID: fmt.Sprintf("ut-%02d", i), Kind: KindDish, Name: fmt.Sprintf("dish %d", i)})
	}
	return store, store.Devices()
}

func TestPollDevicesBoundedConcurrency(t *testing.T) {
	store, devs := seededStore(t, 20)
	p := NewPoller(nil, nil, nil, nil, store, time.Minute, testLogger(t))
	p.SetConcurrency(3)
	var inFlight, peak atomic.Int32
	var mu sync.Mutex
	seen := map[string]int{}
	p.fetch = func(ctx context.Context, d *Device) devicePoll {
		n := inFlight.Add(1)
		for {
			old := peak.Load()
			if n <= old || peak.CompareAndSwap(old, n) {
				break
			}
		}
		time.Sleep(5 * time.Millisecond)
		inFlight.Add(-1)
		mu.Lock()
		seen[d.ID]++
		mu.Unlock()
		return devicePoll{dev: d, status: &DeviceStatus{State: "CONNECTED"}}
	}
	p.pollDevices(devs)
	if got := peak.Load(); got != 3 {
		t.Fatalf("peak concurrency = %d, want 3", got)
	}
	for _, d := range devs {
		if seen[d.ID] != 1 {
			t.Fatalf("device %s polled %d times", d.ID, seen[d.ID])
		}
		if got, _ := store.Device(d.ID); got.Status == nil {
			t.Fatalf("device %s status not applied", d.ID)
		}
	}
}

func TestSetConcurrencyClamps(t *testing.T) {
	p := NewPoller(nil, nil, nil, nil, nil, time.Minute, testLogger(t))
	for in, want := range map[int]int{-1: 1, 0: 1, 5: 5, 99: 16} {
		p.SetConcurrency(in)
		if p.concurrency != want {
			t.Errorf("SetConcurrency(%d) = %d, want %d", in, p.concurrency, want)
		}
	}
}

// TestPollDevicesConcurrentHistory runs the real gateway path for many
// dishes in parallel; under -race it proves workers never write the store
// and per-device history dedupe holds across rounds.
func TestPollDevicesConcurrentHistory(t *testing.T) {
	f, ts := newFakeAPI(t)
	clock := &fakeClock{now: time.Now()}
	s := newTestSession(t, ts, clock)
	var current atomic.Uint64
	current.Store(100)
	f.handleFn = func(req *device.Request) (*device.Response, int32, string) {
		if req.GetGetHistory() != nil {
			return &device.Response{Response: &device.Response_DishGetHistory{DishGetHistory: &device.DishGetHistoryResponse{
				Current:          proto.Uint64(current.Load()),
				PopPingLatencyMs: []float32{1, 2, 3, 4, 5},
			}}}, 0, ""
		}
		return f.defaultHandle(req)
	}
	store := NewStore(t.TempDir(), 24*time.Hour, clock.Now)
	for i := range 12 {
		store.UpsertDevice(&Device{ID: fmt.Sprintf("ut-%02d", i), Kind: KindDish})
	}
	p := NewPoller(NewGateway(s, ts.URL, ts.Client(), nil), nil, nil, s, store, time.Minute, testLogger(t))
	p.SetConcurrency(4)
	p.pollDevices(store.Devices())
	p.pollDevices(store.Devices()) // same counter: must not duplicate
	clock.Advance(2 * time.Second)
	current.Store(102)
	p.pollDevices(store.Devices())
	for _, d := range store.Devices() {
		if n := len(store.History(d.ID, time.Hour)); n != 7 {
			t.Fatalf("%s history = %d samples, want 7", d.ID, n)
		}
	}
}

// TestPollDevicesUnreachableRoundCompletes: hanging devices are cut off by
// the per-device deadline and the round still finishes promptly.
func TestPollDevicesUnreachableRoundCompletes(t *testing.T) {
	_, api := newFakeAPI(t)
	clock := &fakeClock{now: time.Now()}
	s := newTestSession(t, api, clock)
	if _, err := s.Token(context.Background()); err != nil {
		t.Fatalf("Token: %v", err)
	}
	release := make(chan struct{})
	hang := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	t.Cleanup(func() { close(release); hang.Close() })
	g := NewGateway(s, hang.URL, hang.Client(), nil)
	g.SetCallTimeout(50 * time.Millisecond)
	store, devs := seededStore(t, 16)
	p := NewPoller(g, nil, nil, s, store, time.Minute, testLogger(t))
	p.SetConcurrency(4)
	start := time.Now()
	done := make(chan struct{})
	go func() { p.pollDevices(devs); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("round did not complete with unreachable devices")
	}
	// 16 devices / 4 workers × one 50ms call each ≈ 200ms; sequential would be ≥ 800ms.
	if el := time.Since(start); el > 600*time.Millisecond {
		t.Errorf("round took %v; devices not polled in parallel", el)
	}
	for _, d := range devs {
		if got, _ := store.Device(d.ID); got.Status != nil {
			t.Fatalf("unreachable device %s got a status", d.ID)
		}
	}
}

type recordingSink struct {
	mu      sync.Mutex
	gauges  map[string]float64
	deleted []string
	hist    map[string][]float64
}

func (r *recordingSink) SetGauge(name, labels string, v float64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.gauges == nil {
		r.gauges = map[string]float64{}
	}
	r.gauges[name+"{"+labels+"}"] = v
}
func (r *recordingSink) SetDeviceGauge(name, id, label string, v float64) {
	r.SetGauge(name, id, v)
}
func (r *recordingSink) DeleteDevice(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.deleted = append(r.deleted, id)
}
func (r *recordingSink) IncCounter(string, string)        {}
func (r *recordingSink) ObserveHistogram(name string, v float64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.hist == nil {
		r.hist = map[string][]float64{}
	}
	r.hist[name] = append(r.hist[name], v)
}

// A healthy tick reports session_dead=0 (not only the 1 transition), and
// devices that left the roster have their series removed.
func TestPollerTickMetrics(t *testing.T) {
	_, ts := newFakeAPI(t)
	clock := &fakeClock{now: time.Now()}
	s := newTestSession(t, ts, clock)
	store := NewStore(t.TempDir(), 24*time.Hour, clock.Now)
	store.UpsertDevice(&Device{ID: "ut-gone", Kind: KindDish, Name: "Old dish"})
	p := NewPoller(NewGateway(s, ts.URL, ts.Client(), nil), NewRoster(s, ts.URL, time.Minute, nil),
		NewTelemetry(s, ts.URL, nil), s, store, 30*time.Second, testLogger(t))
	sink := &recordingSink{}
	p.SetMetrics(sink)
	if err := p.Tick(); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	sink.mu.Lock()
	defer sink.mu.Unlock()
	if v, ok := sink.gauges["starlink_session_dead{}"]; !ok || v != 0 {
		t.Errorf("session_dead = %v (set=%v), want 0", v, ok)
	}
	if !slices.Contains(sink.deleted, "ut-gone") {
		t.Errorf("retired device series not deleted: %v", sink.deleted)
	}
	if _, ok := sink.gauges["starlink_device_up{ut-gone}"]; ok {
		t.Error("retired device must not be polled")
	}
}

// Skipped rounds (session-dead early return, recovery not yet due) must
// not feed the poll-duration histogram; only real polling rounds do.
func TestPollerSkippedRoundNotObserved(t *testing.T) {
	f, ts := newFakeAPI(t)
	clock := &fakeClock{now: time.Now()}
	s := newTestSession(t, ts, clock)
	oldCooldown := deadCooldown
	deadCooldown = time.Minute
	t.Cleanup(func() { deadCooldown = oldCooldown })

	store := NewStore(t.TempDir(), 24*time.Hour, clock.Now)
	store.UpsertDevice(&Device{ID: "ut-x", Kind: KindDish, Name: "Dish"})
	p := NewPoller(NewGateway(s, ts.URL, ts.Client(), nil), NewRoster(s, ts.URL, time.Minute, nil),
		NewTelemetry(s, ts.URL, nil), s, store, 30*time.Second, testLogger(t))
	sink := &recordingSink{}
	p.SetMetrics(sink)

	// Latch the session dead with two failed probes (recovery not due
	// yet: the latch just happened, cooldown is a minute).
	f.mu.Lock()
	f.probeStatus = 401
	f.mu.Unlock()
	_, _ = s.Token(context.Background())
	clock.Advance(probeMinInterval + time.Second)
	_, _ = s.Token(context.Background())
	if !s.SessionDead() {
		t.Fatal("setup: session must be latched")
	}

	if err := p.Tick(); err != nil {
		t.Fatalf("skipped tick should return nil: %v", err)
	}
	sink.mu.Lock()
	n := len(sink.hist["starlink_poll_round_duration_seconds"])
	sink.mu.Unlock()
	if n != 0 {
		t.Errorf("skipped round recorded %d histogram observations, want 0", n)
	}

	// A live tick (after recovery) observes exactly one round.
	f.mu.Lock()
	f.probeStatus = 200
	f.mu.Unlock()
	clock.Advance(2 * time.Minute)
	if err := p.Tick(); err != nil {
		t.Fatalf("live tick: %v", err)
	}
	sink.mu.Lock()
	n = len(sink.hist["starlink_poll_round_duration_seconds"])
	sink.mu.Unlock()
	if n != 1 {
		t.Errorf("live round recorded %d histogram observations, want 1", n)
	}
}
