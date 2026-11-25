package starlink

import (
	"encoding/json"
	"math"
	"net/http"
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

func TestStoreLoadCorruptFileStartsEmpty(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(dir, 24*time.Hour, nil)
	bad := filepath.Join(dir, "store.json")
	if err := os.WriteFile(bad, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := store.Load(); err != nil {
		t.Fatalf("Load on a corrupt file must not fail startup: %v", err)
	}
	if devices := store.Devices(); len(devices) != 0 {
		t.Fatalf("devices = %d, want 0 after corrupt load", len(devices))
	}
	// The corrupt file must be renamed aside, not deleted silently.
	entries, _ := os.ReadDir(dir)
	found := false
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "store.json.corrupt-") {
			found = true
		}
	}
	if !found {
		t.Fatal("corrupt store.json was not renamed aside")
	}
}

func TestStorePruneDevices(t *testing.T) {
	store := NewStore(t.TempDir(), 24*time.Hour, nil)
	store.UpsertDevice(&Device{ID: "ut1", Kind: KindDish, Name: "a", Active: true})
	store.UpsertDevice(&Device{ID: "ut2", Kind: KindDish, Name: "b", Active: true})
	store.UpsertDevice(&Device{ID: "r1", Kind: KindRouter, Name: "c", Active: true})

	// Departure marks retirement with a grace period; nothing is removed yet.
	removed := store.PruneDevices(map[string]struct{}{"ut1": {}, "r1": {}})
	if len(removed) != 0 {
		t.Fatalf("removed = %v on first prune, want none (grace period)", removed)
	}
	ids := map[string]bool{}
	var ut2 *Device
	for _, d := range store.Devices() {
		ids[d.ID] = true
		if d.ID == "ut2" {
			ut2 = d
		}
	}
	if !ids["ut2"] {
		t.Fatal("departed device must stay during the grace period")
	}
	if ut2 == nil || ut2.Retired == nil {
		t.Fatal("departed device must be marked retired")
	}
	if !ids["ut1"] || !ids["r1"] {
		t.Fatal("live devices must survive pruning")
	}
	// A returning device clears its retirement.
	store.UpsertDevice(&Device{ID: "ut2", Kind: KindDish, Name: "b", Active: true})
	_ = store.PruneDevices(map[string]struct{}{"ut1": {}, "r1": {}, "ut2": {}})
	for _, d := range store.Devices() {
		if d.ID == "ut2" && d.Retired != nil {
			t.Fatal("returning device must be un-retired")
		}
	}
	// After the grace period expires the device is pruned.
	expired := store.PruneDevices(map[string]struct{}{"ut1": {}, "r1": {}})
	if len(expired) != 0 {
		t.Fatalf("expired = %v, want none before grace expiry", expired)
	}
	for _, d := range store.Devices() {
		if d.ID == "ut2" {
			// Force the retire timestamp to the past to simulate expiry.
			past := time.Now().Add(-retireGracePeriod - time.Minute)
			store.mu.Lock()
			if inner, ok := store.devices["ut2"]; ok {
				inner.Retired = &past
			}
			store.mu.Unlock()
			break
		}
	}
	removed = store.PruneDevices(map[string]struct{}{"ut1": {}, "r1": {}})
	if len(removed) != 1 || removed[0] != "ut2" {
		t.Fatalf("removed = %v, want [ut2] after grace expiry", removed)
	}
	// An empty live set must not wipe the registry immediately (caller
	// guards, but the store only retires first).
	removed = store.PruneDevices(map[string]struct{}{})
	if len(removed) != 0 {
		t.Fatalf("removed = %v on empty live set, want none yet", removed)
	}
}

func TestStoreSaveConcurrentReads(t *testing.T) {
	// Race smoke test: Save must not hold the store lock while marshaling
	// and writing, so page handlers stay responsive.
	store := NewStore(t.TempDir(), 24*time.Hour, nil)
	store.UpsertDevice(&Device{ID: "ut1", Kind: KindDish, Name: "a", Active: true})

	var wg sync.WaitGroup
	stop := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			if err := store.Save(); err != nil {
				t.Errorf("Save: %v", err)
				return
			}
		}
	}()
	for i := 0; i < 200; i++ {
		_ = store.History("ut1", time.Hour)
		_ = store.Devices()
	}
	close(stop)
	wg.Wait()
}

func TestStoreSaveRoundTrip(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(dir, 24*time.Hour, nil)
	store.UpsertDevice(&Device{ID: "ut1", Kind: KindDish, Name: "a", Active: true})
	now := time.Now().Unix()
	store.AppendHistory("ut1", &device.DishGetHistoryResponse{
		Current:          proto.Uint64(uint64(now)),
		PopPingLatencyMs: []float32{10, 11, 12},
		Obstructed:       []bool{true, false, false},
	})
	if err := store.Save(); err != nil {
		t.Fatal(err)
	}
	// No tmp file may survive a successful save.
	if _, err := os.Stat(filepath.Join(dir, "store.json.tmp")); !os.IsNotExist(err) {
		t.Fatal("tmp file survived save")
	}
	store2 := NewStore(dir, 24*time.Hour, nil)
	if err := store2.Load(); err != nil {
		t.Fatal(err)
	}
	d, ok := store2.Device("ut1")
	if !ok {
		t.Fatal("device lost across save/load")
	}
	if len(store2.History("ut1", time.Hour)) == 0 {
		t.Fatal("history lost across save/load")
	}
	_ = d
}

func TestStoreTelemetryNaNDoesNotBrickPersistence(t *testing.T) {
	store := NewStore(t.TempDir(), 24*time.Hour, nil)
	store.UpsertDevice(&Device{ID: "ut1", Kind: KindDish, Name: "a", Active: true})
	store.SetTelemetry("ut1", map[string]any{
		"signalQuality":          math.NaN(),
		"downlinkThroughputMbps": math.Inf(1),
		"uptimeSeconds":          float64(42),
	})
	for i := 0; i < 3; i++ {
		if err := store.Save(); err != nil {
			t.Fatalf("save %d failed: %v", i, err)
		}
	}
	var snap struct {
		Devices []*Device `json:"devices"`
	}
	data, err := os.ReadFile(filepath.Join(store.dataDir, "store.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &snap); err != nil {
		t.Fatalf("persisted JSON unparsable: %v", err)
	}
	if snap.Devices[0].Telemetry["uptimeSeconds"].(float64) != 42 {
		t.Fatal("clean telemetry value lost")
	}
	if v := snap.Devices[0].Telemetry["signalQuality"].(float64); math.IsNaN(v) || v != 0 {
		t.Fatalf("NaN not sanitized: %v", v)
	}
}

func TestCookieJarMaxAge(t *testing.T) {
	jar := newCookieJar()
	u := &url.URL{Scheme: "https", Host: "api.example.test", Path: "/"}

	// Max-Age > 0 sets a rolling expiry.
	jar.SetCookies(u, []*http.Cookie{{Name: "a", Value: "v", MaxAge: 1, Path: "/"}})
	if got := jar.Cookies(u); len(got) != 1 || got[0].Value != "v" {
		t.Fatalf("cookies after Max-Age set = %v", got)
	}
	time.Sleep(1100 * time.Millisecond)
	if got := jar.Cookies(u); len(got) != 0 {
		t.Fatalf("expired Max-Age cookie still sent: %v", got)
	}

	// Max-Age <= 0 deletes the cookie even with an empty value (the
	// server-expired form), and must be handled before the empty-value
	// guard skips it.
	jar.SetCookies(u, []*http.Cookie{{Name: "b", Value: "v", Path: "/"}})
	jar.SetCookies(u, []*http.Cookie{{Name: "b", Value: "", MaxAge: -1, Path: "/"}})
	if got := jar.Cookies(u); len(got) != 0 {
		t.Fatalf("Max-Age delete ignored: %v", got)
	}
	jar.SetCookies(u, []*http.Cookie{{Name: "c", Value: "v", Path: "/"}})
	past := time.Now().Add(-time.Minute)
	jar.SetCookies(u, []*http.Cookie{{Name: "c", Value: "", Expires: past, Path: "/"}})
	if got := jar.Cookies(u); len(got) != 0 {
		t.Fatalf("expired-Expires empty-value delete ignored: %v", got)
	}
}

func TestTelemetryAlertCodeTranslation(t *testing.T) {
	body := `{"data":{"values":[["u",1700000000000000000,"ut12345678-a12b3456-123456c1",120.5,18.5,0.01,42,0.5,86400,0.8,0,-1,"2024.05.01","DE",false,[51]],
		["u",1700000001000000000,"ut12345678-a12b3456-123456c1",120.5,18.5,0.01,42,0.5,86400,0.8,0,-1,"2024.05.01","DE",false,[50,53]]],
		"columnNamesByDeviceType":{"u":["DeviceType","UtcTimestampNs","DeviceId","DownlinkThroughput","UplinkThroughput","PingDropRateAvg","PingLatencyMsAvg","ObstructionPercentTime","Uptime","SignalQuality","H3CellId","SecondsUntilSwupdateRebootPossible","RunningSoftwareVersion","CountryCode","InTerritorialWaters","ActiveAlerts"]}},
		"metadata":{"enums":{"DeviceType":{"u":"UserTerminal"},"AlertsByDeviceType":{"u":{"50":"ethernet_slow_10mbps","51":"ethernet_slow_100mbps","53":"actuator_motor_stuck"}}}}}`
	tms, err := parseTelemetryResponse([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if len(tms) != 2 {
		t.Fatalf("entries = %d, want 2", len(tms))
	}
	if len(tms[0].ActiveAlerts) != 1 || tms[0].ActiveAlerts[0] != "ethernet_slow_100mbps" {
		t.Fatalf("alerts = %v, want [ethernet_slow_100mbps]", tms[0].ActiveAlerts)
	}
	want := []string{"ethernet_slow_10mbps", "actuator_motor_stuck"}
	for i, w := range want {
		if tms[1].ActiveAlerts[i] != w {
			t.Fatalf("alerts[%d] = %q, want %q", i, tms[1].ActiveAlerts[i], w)
		}
	}
}

func TestTelemetryUnknownAlertCodeKept(t *testing.T) {
	body := `{"data":{"values":[["u",1700000000000000000,"ut12345678-a12b3456-123456c1",120.5,18.5,0.01,42,0.5,86400,0.8,0,-1,"2024.05.01","DE",false,[999]]],
		"columnNamesByDeviceType":{"u":["DeviceType","UtcTimestampNs","DeviceId","DownlinkThroughput","UplinkThroughput","PingDropRateAvg","PingLatencyMsAvg","ObstructionPercentTime","Uptime","SignalQuality","H3CellId","SecondsUntilSwupdateRebootPossible","RunningSoftwareVersion","CountryCode","InTerritorialWaters","ActiveAlerts"]}},
		"metadata":{"enums":{"AlertsByDeviceType":{"u":{"50":"ethernet_slow_10mbps"}}}}}`
	tms, err := parseTelemetryResponse([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if len(tms) != 1 || tms[0].ActiveAlerts[0] != "999" {
		t.Fatalf("unknown code mangled: %v", tms[0].ActiveAlerts)
	}
}

func TestAppendHistoryClockStepReanchors(t *testing.T) {
	clock := &fakeClock{now: time.Date(2026, 2, 1, 12, 0, 0, 0, time.UTC)}
	st := NewStore(t.TempDir(), 24*time.Hour, clock.Now)
	now := clock.now
	st.UpsertDevice(&Device{ID: "u1", Kind: KindDish, Name: "dish", Active: true})

	// First poll: 3 samples ending at "now".
	hist := func(current uint64, n int) *device.DishGetHistoryResponse {
		lat := make([]float32, n)
		for i := range lat {
			lat[i] = 20
		}
		return &device.DishGetHistoryResponse{Current: proto.Uint64(current), PopPingLatencyMs: lat}
	}
	added1 := st.AppendHistory("u1", hist(1, 3))
	if added1 != 1 {
		t.Fatalf("first poll added %d samples, want 1", added1)
	}
	samples := st.History("u1", 24*time.Hour)
	last := samples[len(samples)-1].Ts
	if !last.Equal(now.Truncate(time.Second)) {
		t.Fatalf("first poll anchored at %v, want %v", last, now.Truncate(time.Second))
	}

	// Clock jumps 30 minutes forward before the next poll; derived
	// timestamps would overlap the past. The store must re-anchor and
	// keep appending instead of dropping samples.
	clock.Advance(30 * time.Minute)
	now = clock.now
	before := len(samples)
	added := st.AppendHistory("u1", hist(4, 5))
	if added == 0 {
		t.Fatal("clock step froze history: no samples appended")
	}
	samples = st.History("u1", 24*time.Hour)
	if len(samples) != before+added {
		t.Fatalf("sample count %d, want %d", len(samples), before+added)
	}
	// Newest sample must be near the stepped clock, not stuck at old anchor.
	newest := samples[len(samples)-1].Ts
	if diff := newest.Sub(now.Truncate(time.Second)); diff > 2*time.Second {
		t.Fatalf("newest sample %v is %v behind stepped clock", newest, diff)
	}

	// Reverse direction: clock steps backwards; samples stamped in the
	// future must be discarded, not persisted.
	clock.Advance(-15 * time.Minute)
	now = clock.now
	added3 := st.AppendHistory("u1", hist(6, 7))
	if added3 != 0 {
		t.Fatalf("third poll after backward step added %d samples, want 0", added3)
	}
	samples = st.History("u1", 24*time.Hour)
	if len(samples) != 4 {
		t.Fatalf("history len %d, want 4 (only the earlier polls)", len(samples))
	}
}
