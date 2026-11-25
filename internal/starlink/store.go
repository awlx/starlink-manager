package starlink

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"time"

	"github.com/awlx/starlink-manager/api/device"
)

// Sample is one history data point (1 Hz from the dish history buffer).
type Sample struct {
	Ts                    time.Time `json:"ts"`
	PopPingLatencyMs      float64   `json:"pingMs"`
	PopPingDropRate       float64   `json:"dropRate"`
	DownlinkThroughputBps float64   `json:"downBps"`
	UplinkThroughputBps   float64   `json:"upBps"`
	SNR                   float64   `json:"snr"`
	Scheduled             bool      `json:"scheduled"`
	Obstructed            bool      `json:"obstructed"`
	SNRReported           bool      `json:"snrReported,omitempty"`
	ScheduledReported     bool      `json:"scheduledReported,omitempty"`
	ObstructedReported    bool      `json:"obstructedReported,omitempty"`
}

// DeviceStatus is the latest snapshot fetched through the device gateway.
type DeviceStatus struct {
	FetchedAt           time.Time         `json:"fetchedAt"`
	HardwareVersion     string            `json:"hardwareVersion,omitempty"`
	SoftwareVersion     string            `json:"softwareVersion,omitempty"`
	State               string            `json:"state,omitempty"`
	UptimeS             uint64            `json:"uptimeS,omitempty"`
	SNR                 *float64          `json:"snr,omitempty"` // nil: firmware did not report it
	PopPingLatencyMs    float64           `json:"popPingLatencyMs,omitempty"`
	PopPingDropRate     float64           `json:"popPingDropRate,omitempty"`
	DownlinkThroughput  float64           `json:"downlinkBps,omitempty"`
	UplinkThroughput    float64           `json:"uplinkBps,omitempty"`
	ObstructionFraction float64           `json:"obstructionFraction,omitempty"`
	ObstructionValidS   float64           `json:"obstructionValidS,omitempty"`
	ObstructionLast24hS float64           `json:"obstructionLast24hS,omitempty"`
	CurrentlyObstructed bool              `json:"currentlyObstructed,omitempty"`
	StowRequested       bool              `json:"stowRequested,omitempty"`
	Alerts              map[string]bool   `json:"alerts,omitempty"`
	Wifi                *WifiStatusExtra  `json:"wifi,omitempty"`
	Reported            map[string]string `json:"reported"`
}

// WifiStatusExtra carries router-specific status fields.
type WifiStatusExtra struct {
	SerialNumber   string  `json:"serialNumber,omitempty"`
	SKU            string  `json:"sku,omitempty"`
	IPv4WANAddress string  `json:"ipv4WanAddress,omitempty"`
	PingDropRate   float64 `json:"pingDropRate,omitempty"`
	PingLatencyMs  float64 `json:"pingLatencyMs,omitempty"`
	CaptivePortal  bool    `json:"captivePortalEnabled,omitempty"`
}

// Device is one tracked dish or router.
type Device struct {
	ID            string            `json:"id"`
	Kind          DeviceKind        `json:"kind"`
	Name          string            `json:"name"`
	Serial        string            `json:"serial,omitempty"`
	DishSerial    string            `json:"dishSerial,omitempty"`
	ServiceLine   string            `json:"serviceLine,omitempty"`
	Active        bool              `json:"active"`
	RosterDetails map[string]string `json:"rosterDetails,omitempty"`
	ClientDetails map[string]string `json:"clientDetails,omitempty"`
	// Retired marks a device that left the account roster. It is pruned
	// after the grace period so kit swaps keep their history available.
	Retired   *time.Time `json:"retired,omitempty"`
	RetiredAt time.Time  `json:"retiredAt,omitempty"`

	Status    *DeviceStatus  `json:"status,omitempty"`
	Cursor    int64          `json:"cursor,omitempty"` // dish history sample counter consumed last
	Telemetry map[string]any `json:"telemetry,omitempty"`
	LastSeen  time.Time      `json:"lastSeen,omitempty"`
	Samples   []Sample       `json:"samples,omitempty"`
}

// Store keeps the device registry, per-device rolling history and latest
// status. History is aggregated across polls using the dish history cursor
// and persisted to a JSON file in the data dir so charts survive restarts.
type Store struct {
	mu sync.Mutex
	// saveMu serializes Save: concurrent saves would share the temp file
	// and could rename a truncated or interleaved payload into place.
	saveMu  sync.Mutex
	devices map[string]*Device
	window  time.Duration
	dataDir string
	logFile string
	dirty   bool
	now     func() time.Time
}

// NewStore creates a store with the given history window and data dir.
func NewStore(dataDir string, window time.Duration, now func() time.Time) *Store {
	if now == nil {
		now = time.Now
	}
	return &Store{
		devices: map[string]*Device{},
		window:  window,
		dataDir: dataDir,
		logFile: filepath.Join(dataDir, "store.json"),
		now:     now,
	}
}

// Load reads persisted devices and history from disk, if present. A
// corrupt or unparsable file is renamed aside and the store starts empty
// instead of failing startup.
func (s *Store) Load() error {
	data, err := os.ReadFile(s.logFile)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var snap struct {
		Devices []*Device `json:"devices"`
	}
	if err := json.Unmarshal(data, &snap); err != nil {
		aside := fmt.Sprintf("%s.corrupt-%d", s.logFile, s.now().Unix())
		if rerr := os.Rename(s.logFile, aside); rerr == nil {
			s.logWarn("persisted store is corrupt; renamed aside and starting empty",
				"file", s.logFile, "aside", aside, "err", err)
		} else {
			s.logWarn("persisted store is corrupt and could not be renamed aside; starting empty",
				"file", s.logFile, "err", err)
		}
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	cutoff := s.now().Add(-s.window)
	for _, d := range snap.Devices {
		if d == nil || d.ID == "" {
			continue
		}
		kept := d.Samples[:0]
		for _, sample := range d.Samples {
			if !sample.Ts.Before(cutoff) {
				kept = append(kept, sample)
			}
		}
		d.Samples = kept
		if len(d.Samples) > persistedSampleCap {
			d.Samples = append([]Sample(nil), d.Samples[len(d.Samples)-persistedSampleCap:]...)
		}
		s.devices[d.ID] = d
	}
	return nil
}

// persistedSampleCap bounds the samples persisted per device. At 1 Hz the
// 24 h window already produces ~86k samples per dish (~14 MB of JSON);
// the cap prevents a misconfigured window from blowing up the store file.
const persistedSampleCap = 90_000

// Save writes the current state to disk atomically: the snapshot is taken
// under the lock, but marshaling and writing happen outside it so page
// handlers are never blocked by a multi-megabyte serialization. The write
// goes to a temp file that is fsynced, renamed over the live file, and the
// directory entry is fsynced before the dirty flag is cleared, so a crash
// cannot leave an empty or truncated store.
func (s *Store) Save() error {
	s.saveMu.Lock()
	defer s.saveMu.Unlock()
	s.mu.Lock()
	if err := os.MkdirAll(s.dataDir, 0o755); err != nil {
		s.mu.Unlock()
		return err
	}
	devices := make([]*Device, 0, len(s.devices))
	for _, d := range s.devices {
		cp := *d
		// Deep-copy the sample slice: the poller appends concurrently.
		cp.Samples = append([]Sample(nil), d.Samples...)
		devices = append(devices, &cp)
	}
	dirty := s.dirty
	s.mu.Unlock()

	payload, err := json.Marshal(struct {
		Devices []*Device `json:"devices"`
	}{Devices: devices})
	if err != nil {
		return err
	}
	tmp := s.logFile + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(payload); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, s.logFile); err != nil {
		os.Remove(tmp)
		return err
	}
	// fsync the directory so the rename is durable before clearing dirty.
	if dir, derr := os.Open(s.dataDir); derr == nil {
		_ = dir.Sync()
		dir.Close()
	}
	if dirty {
		s.mu.Lock()
		s.dirty = false
		s.mu.Unlock()
	}
	return nil
}

func (s *Store) logWarn(msg string, args ...any) {
	slog.Warn(msg, append([]any{"component", "store"}, args...)...)
}

// UpsertDevice registers or updates registry metadata for a device.
func (s *Store) UpsertDevice(info *Device) {
	s.mu.Lock()
	defer s.mu.Unlock()
	existing, ok := s.devices[info.ID]
	if !ok {
		info.Samples = nil
		s.devices[info.ID] = info
		s.dirty = true
		return
	}
	if existing.Name != info.Name || existing.Serial != info.Serial ||
		existing.DishSerial != info.DishSerial || existing.ServiceLine != info.ServiceLine ||
		existing.Active != info.Active || existing.Kind != info.Kind || !reflect.DeepEqual(existing.RosterDetails, info.RosterDetails) {
		existing.Name = info.Name
		existing.Serial = info.Serial
		existing.DishSerial = info.DishSerial
		existing.ServiceLine = info.ServiceLine
		existing.Active = info.Active
		existing.Kind = info.Kind
		existing.RosterDetails = info.RosterDetails
		s.dirty = true
	}
}

// Devices returns copies of all devices with samples excluded. Callers read
// them without the store lock while the poller keeps updating the store, so
// they must not share the stored structs.
func (s *Store) Devices() []*Device {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*Device, 0, len(s.devices))
	for _, d := range s.devices {
		cp := *d
		cp.Samples = nil
		out = append(out, &cp)
	}
	return out
}

// Device returns a copy of one device (registry + status + telemetry) with
// samples excluded.
func (s *Store) Device(id string) (*Device, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, ok := s.devices[id]
	if !ok {
		return nil, false
	}
	cp := *d
	cp.Samples = nil
	return &cp, true
}

// AppendHistory merges fresh dish history samples.
//
// The dish history is a ring buffer: `current` counts the samples the dish
// has taken since boot (1 Hz), sample number k lives at index k % len, and
// the newest sample is number current-1. Sample k was taken about
// current-1-k seconds before the fetch. The device cursor stores the
// `current` value consumed last, so each poll appends only the samples taken
// since. A counter smaller than the cursor means the dish rebooted and the
// cursor starts over.
func (s *Store) AppendHistory(id string, hist *device.DishGetHistoryResponse) int {
	if hist == nil {
		return 0
	}
	lat := hist.GetPopPingLatencyMs()
	drop := hist.GetPopPingDropRate()
	down := hist.GetDownlinkThroughputBps()
	up := hist.GetUplinkThroughputBps()
	snr := hist.GetSnr()
	sched := hist.GetScheduled()
	obs := hist.GetObstructed()

	n := int64(len(lat))
	if n == 0 {
		return 0
	}
	current := int64(hist.GetCurrent())
	if current <= 0 {
		return 0
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	d, ok := s.devices[id]
	if !ok {
		return 0
	}
	cursor := d.Cursor
	if current < cursor {
		cursor = 0 // the dish rebooted and restarted its sample counter
	}
	if current == cursor {
		return 0
	}
	first := max(current-n, cursor, 0)
	fetched := s.now().Truncate(time.Second)
	var lastTs time.Time
	if len(d.Samples) > 0 {
		lastTs = d.Samples[len(d.Samples)-1].Ts
	}
	// Wall-clock steps (NTP corrections, host suspend/resume) skew derived
	// timestamps. If the newest derived timestamp (the fetch time) would not
	// land clearly past what is already stored, re-anchor to the current
	// observed time so the series self-heals instead of dropping every later
	// sample forever.
	anchor := fetched
	if len(d.Samples) > 0 && !fetched.After(lastTs.Add(2*sampleInterval())) {
		anchor = s.now().Truncate(time.Second)
	}
	var added int
	for k := first; k < current; k++ {
		ts := anchor.Add(-time.Duration(current-1-k) * time.Second)
		// Polls derive timestamps from their own fetch time; skip seconds
		// already stored so jitter or a reboot cannot duplicate samples.
		if !ts.After(lastTs) {
			continue
		}
		if ts.After(anchor.Add(2 * sampleInterval())) { // discard future-stamped samples (clock steps)
			continue
		}
		i := int(k % n)
		d.Samples = append(d.Samples, Sample{
			Ts:                    ts,
			PopPingLatencyMs:      f32At(lat, i),
			PopPingDropRate:       f32At(drop, i),
			DownlinkThroughputBps: f32At(down, i),
			UplinkThroughputBps:   f32At(up, i),
			SNR:                   f32At(snr, i),
			SNRReported:           i < len(snr),
			ScheduledReported:     i < len(sched),
			ObstructedReported:    i < len(obs),
			Scheduled:             bAt(sched, i),
			Obstructed:            bAt(obs, i),
		})
		lastTs = ts
		added++
	}
	d.Cursor = current
	d.LastSeen = s.now()
	if added > 0 {
		s.dirty = true
	}
	// Trim beyond the history window (do it cheaply in bulk).
	if len(d.Samples) > 0 {
		cutoffTs := s.now().Add(-s.window)
		// Samples are appended in ascending timestamp order; find trim point.
		trim := 0
		for trim < len(d.Samples) && d.Samples[trim].Ts.Before(cutoffTs) {
			trim++
		}
		if trim > 1024 { // compact only when meaningful
			d.Samples = append([]Sample(nil), d.Samples[trim:]...)
		}
		// Hard cap so a misconfigured window cannot blow up the store.
		if len(d.Samples) > persistedSampleCap {
			d.Samples = append([]Sample(nil), d.Samples[len(d.Samples)-persistedSampleCap:]...)
		}
	}
	return added
}

// retireGracePeriod keeps departed devices (and their history) visible for
// one day after they leave the roster, then prunes them. This covers kit
// swaps where a terminal disappears briefly and comes back with the same ID.
const retireGracePeriod = 24 * time.Hour

// PruneDevices marks devices whose IDs are absent from the given live set
// as retired and reports those pruned after the grace period. It is called
// after a successful roster fetch so devices removed from the account stop
// being polled instead of lingering as unreachable ghosts, while recent
// leavers stay renderable for reference.
func (s *Store) PruneDevices(live map[string]struct{}) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var removed []string
	for id, d := range s.devices {
		if _, ok := live[id]; ok {
			if d.Retired != nil {
				d.Retired = nil
				d.RetiredAt = time.Time{}
				s.dirty = true
			}
			continue
		}
		if d.Retired == nil {
			now := s.now()
			d.Retired = &now
			d.RetiredAt = now
			s.dirty = true
			continue
		}
		if s.now().Sub(*d.Retired) >= retireGracePeriod {
			delete(s.devices, id)
			removed = append(removed, id)
			s.dirty = true
		}
	}
	return removed
}

// FileSize returns the byte size of the persisted store file (0 if absent).
func (s *Store) FileSize() int64 {
	fi, err := os.Stat(s.logFile)
	if err != nil {
		return 0
	}
	return fi.Size()
}

func (s *Store) SetStatus(id string, st *DeviceStatus) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, ok := s.devices[id]
	if !ok {
		return
	}
	d.Status = st
	d.LastSeen = s.now()
	s.dirty = true
}

// SetTelemetry stores the latest telemetry-cache enrichment entry.
func (s *Store) SetTelemetry(id string, tm map[string]any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, ok := s.devices[id]
	if !ok {
		return
	}

	// Sanitize floats: NaN/Inf break json.Marshal, which would make every
	// save fail until restart.
	d.Telemetry = sanitizeTelemetry(tm)
	s.dirty = true
}

// SetClients keeps the full read-only client snapshot for the HTML diagnostics.
func (s *Store) SetClients(id string, clients []*device.WifiClient) {
	fields := map[string]string{"Clients fetched (UTC)": s.now().UTC().Format(time.RFC3339)}
	for i, client := range clients {
		for key, value := range reportedFields(client) {
			fields[fmt.Sprintf("clients[%d].%s", i, key)] = value
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if d, ok := s.devices[id]; ok {
		d.ClientDetails = fields
		s.dirty = true
	}
}

// sanitizeTelemetry replaces NaN/±Inf float values with 0 and drops nils.
func sanitizeTelemetry(tm map[string]any) map[string]any {
	out := make(map[string]any, len(tm))
	for k, v := range tm {
		switch val := v.(type) {
		case float64:
			if math.IsNaN(val) || math.IsInf(val, 0) {
				val = 0
			}
			out[k] = val
		case nil:
			// drop
		default:
			out[k] = v
		}
	}
	return out
}

// History returns samples for a device within the given window ending now.
func (s *Store) History(id string, window time.Duration) []Sample {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, ok := s.devices[id]
	if !ok {
		return nil
	}
	cutoff := s.now().Add(-window)
	out := make([]Sample, 0, len(d.Samples))
	for _, sample := range d.Samples {
		if !sample.Ts.Before(cutoff) {
			out = append(out, sample)
		}
	}
	return out
}

// MarkSeen updates lastSeen without touching status.
func (s *Store) MarkSeen(id string, at time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if d, ok := s.devices[id]; ok {
		d.LastSeen = at
	}
}

func f32At(arr []float32, i int) float64 {
	if i < len(arr) {
		return finite(arr[i])
	}
	return 0
}

// finite converts a dish float to float64, mapping NaN and ±Inf (which
// some dish firmware reports for unavailable values) to 0: encoding/json
// rejects them, which would break persistence and the JSON API.
func finite(v float32) float64 {
	f := float64(v)
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return 0
	}
	return f
}

func bAt(arr []bool, i int) bool {
	if i < len(arr) {
		return arr[i]
	}
	return false
}

// sampleInterval is the 1 Hz cadence of dish history samples.
func sampleInterval() time.Duration { return time.Second }
