package starlink

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
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
}

// DeviceStatus is the latest snapshot fetched through the device gateway.
type DeviceStatus struct {
	FetchedAt           time.Time        `json:"fetchedAt"`
	HardwareVersion     string           `json:"hardwareVersion,omitempty"`
	SoftwareVersion     string           `json:"softwareVersion,omitempty"`
	State               string           `json:"state,omitempty"`
	UptimeS             uint64           `json:"uptimeS,omitempty"`
	SNR                 float64          `json:"snr,omitempty"`
	PopPingLatencyMs    float64          `json:"popPingLatencyMs,omitempty"`
	PopPingDropRate     float64          `json:"popPingDropRate,omitempty"`
	DownlinkThroughput  float64          `json:"downlinkBps,omitempty"`
	UplinkThroughput    float64          `json:"uplinkBps,omitempty"`
	ObstructionFraction float64          `json:"obstructionFraction,omitempty"`
	ObstructionValidS   float64          `json:"obstructionValidS,omitempty"`
	ObstructionLast24hS float64          `json:"obstructionLast24hS,omitempty"`
	CurrentlyObstructed bool             `json:"currentlyObstructed,omitempty"`
	StowRequested       bool             `json:"stowRequested,omitempty"`
	Alerts              map[string]bool  `json:"alerts,omitempty"`
	Wifi                *WifiStatusExtra `json:"wifi,omitempty"`
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
	ID          string     `json:"id"`
	Kind        DeviceKind `json:"kind"`
	Name        string     `json:"name"`
	Serial      string     `json:"serial,omitempty"`
	DishSerial  string     `json:"dishSerial,omitempty"`
	ServiceLine string     `json:"serviceLine,omitempty"`
	Active      bool       `json:"active"`

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
	mu      sync.Mutex
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

// Load reads persisted devices and history from disk, if present.
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
		return fmt.Errorf("parsing %s: %w", s.logFile, err)
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
		s.devices[d.ID] = d
	}
	return nil
}

// Save writes the current state to disk atomically (tmp file + rename).
func (s *Store) Save() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.MkdirAll(s.dataDir, 0o755); err != nil {
		return err
	}
	devices := make([]*Device, 0, len(s.devices))
	for _, d := range s.devices {
		devices = append(devices, d)
	}
	payload, err := json.MarshalIndent(struct {
		Devices []*Device `json:"devices"`
	}{Devices: devices}, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.logFile + ".tmp"
	if err := os.WriteFile(tmp, payload, 0o600); err != nil {
		return err
	}
	s.dirty = false
	return os.Rename(tmp, s.logFile)
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
		existing.Active != info.Active || existing.Kind != info.Kind {
		existing.Name = info.Name
		existing.Serial = info.Serial
		existing.DishSerial = info.DishSerial
		existing.ServiceLine = info.ServiceLine
		existing.Active = info.Active
		existing.Kind = info.Kind
		s.dirty = true
	}
}

// Devices returns a snapshot of all devices.
func (s *Store) Devices() []*Device {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*Device, 0, len(s.devices))
	for _, d := range s.devices {
		out = append(out, d)
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
	var added int
	for k := first; k < current; k++ {
		ts := fetched.Add(-time.Duration(current-1-k) * time.Second)
		// Polls derive timestamps from their own fetch time; skip seconds
		// already stored so jitter or a reboot cannot duplicate samples.
		if !ts.After(lastTs) {
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
	}
	return added
}

// SetStatus stores the latest gateway status snapshot for a device.
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
	d.Telemetry = tm
	s.dirty = true
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
		return float64(arr[i])
	}
	return 0
}

func bAt(arr []bool, i int) bool {
	if i < len(arr) {
		return arr[i]
	}
	return false
}
