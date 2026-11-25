package starlink

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/awlx/starlink-manager/api/device"
)

// Poller keeps the store fresh: it refreshes the roster, pulls telemetry
// enrichment, and polls each device through the gateway (status + history).
// Device failures never crash the loop; the UI shows stale data instead.
type Poller struct {
	gateway   *Gateway
	roster    *Roster
	telemetry *Telemetry
	session   *Session
	store     *Store
	interval  time.Duration
	saveEvery time.Duration
	log       *slog.Logger

	stop     chan struct{}
	stopOnce sync.Once
	done     chan struct{} // closed when run returns (after the final save)
	started  atomic.Bool
	// ctx is cancelled by Stop so in-flight calls end promptly.
	ctx    context.Context
	cancel context.CancelFunc

	// backoff grows when a tick fails (e.g. probe rate limit) and delays
	// the next attempt instead of hammering the API.
	backoff time.Duration
}

// NewPoller wires the poller.
func NewPoller(gateway *Gateway, roster *Roster, telemetry *Telemetry, session *Session, store *Store, interval time.Duration, log *slog.Logger) *Poller {
	ctx, cancel := context.WithCancel(context.Background())
	return &Poller{
		ctx:       ctx,
		cancel:    cancel,
		gateway:   gateway,
		roster:    roster,
		telemetry: telemetry,
		session:   session,
		store:     store,
		interval:  interval,
		saveEvery: time.Minute,
		log:       log,
		stop:      make(chan struct{}),
		done:      make(chan struct{}),
	}
}

// Start launches the polling loop in a goroutine.
func (p *Poller) Start() {
	p.started.Store(true)
	go func() {
		defer close(p.done)
		p.run()
	}()
}

// Stop terminates the loop and waits until the store has been flushed, so
// the final save is not lost when the process exits right after.
func (p *Poller) Stop() {
	p.stopOnce.Do(func() {
		p.cancel()
		close(p.stop)
	})
	if p.started.Load() {
		<-p.done
	}
}

// maxPollBackoff caps the retry backoff after failed ticks.
const maxPollBackoff = 2 * time.Minute

func (p *Poller) run() {
	save := time.NewTicker(p.saveEvery)
	defer save.Stop()

	// Probe the session once before the first tick so the very first
	// round already has a token and does not race the probe throttle.
	// When it fails, the first round waits for the backoff instead of
	// immediately running into the probe rate limit.
	if p.initialProbe() {
		// First tick immediately so the UI has data right away.
		p.tickRound()
	}
	// The next tick deadline is a single timer that survives store saves:
	// re-arming it on every loop iteration would let the save ticker
	// starve polling whenever interval+backoff exceeds the save period.
	next := time.NewTimer(p.interval + p.backoff)
	defer next.Stop()
	for {
		select {
		case <-p.stop:
			if err := p.store.Save(); err != nil {
				p.log.Warn("saving store on shutdown", "err", err)
			}
			return
		case <-save.C:
			if err := p.store.Save(); err != nil {
				p.log.Warn("saving store", "err", err)
			}
		case <-next.C:
			p.tickRound()
			next.Reset(p.interval + p.backoff)
		}
	}
}

// initialProbe performs one bounded session probe at startup. Failure is
// non-fatal: later ticks retry with backoff.
func (p *Poller) initialProbe() bool {
	ctx, cancel := context.WithTimeout(p.ctx, 20*time.Second)
	defer cancel()
	if err := p.session.Ensure(ctx); err != nil {
		p.log.Warn("initial session probe failed; will retry on next tick", "err", err)
		p.backoff = p.interval
		return false
	}
	return true
}

// tickRound runs one poll tick and adjusts the retry backoff.
func (p *Poller) tickRound() {
	if err := p.Tick(); err != nil {
		if p.backoff == 0 {
			p.backoff = p.interval
		} else if p.backoff < maxPollBackoff {
			p.backoff *= 2
			if p.backoff > maxPollBackoff {
				p.backoff = maxPollBackoff
			}
		}
		p.log.Warn("poll tick failed; retrying with backoff",
			"backoff", p.backoff.String(), "err", err)
		return
	}
	p.backoff = 0
}

// Tick performs one polling round. A returned error means the round should
// be retried later (with backoff); device data is never lost between ticks.
func (p *Poller) Tick() error {
	// Roster and telemetry share one bounded context; device polls get
	// their own budgets below so a long device list or slow devices cannot
	// starve the rest of the round.
	ctx, cancel := context.WithTimeout(p.ctx, max(p.interval*2, 4*p.gateway.CallTimeout()))
	defer cancel()

	if p.session.SessionDead() {
		p.log.Warn("starlink session dead; skipping poll (update STARLINK_SSO_COOKIE)")
		return nil
	}

	// 1. Refresh the roster (cached ~5 min internally).
	lines, err := p.roster.Get(ctx)
	if err != nil {
		// Transient (probe rate limit, network, API hiccup): keep any data
		// already collected and let the caller retry with backoff.
		return fmt.Errorf("roster unavailable: %w", err)
	} else {
		for _, sl := range lines {
			for _, t := range sl.Terminals {
				p.store.UpsertDevice(&Device{
					ID:          t.UserTerminalID,
					Kind:        KindDish,
					Name:        dishName(t),
					Serial:      t.SerialNumber,
					DishSerial:  t.DishSerialNumber,
					ServiceLine: sl.NicknameOrNumber(),
					Active:      t.Active,
				})
			}
			for _, r := range sl.Routers {
				p.store.UpsertDevice(&Device{
					ID:          r.RouterID,
					Kind:        KindRouter,
					Name:        routerName(r),
					ServiceLine: sl.NicknameOrNumber(),
					Active:      true,
				})
			}
		}
	}

	// 2. Telemetry enrichment (best effort).
	if acc := p.session.AccountNumber(); acc != "" {
		tms, err := p.telemetry.Query(ctx, acc, 0)
		if err != nil {
			p.log.Debug("telemetry enrichment failed", "err", err)
		} else {
			for _, tm := range tms {
				entry := map[string]any{
					"softwareVersion":        tm.SoftwareVersion,
					"downlinkThroughputMbps": tm.DownlinkThroughputMbps,
					"uplinkThroughputMbps":   tm.UplinkThroughputMbps,
					"pingDropRateAvg":        tm.PingDropRateAvg,
					"pingLatencyMsAvg":       tm.PingLatencyMsAvg,
					"obstructionPercentTime": tm.ObstructionPercentTime,
					"signalQuality":          tm.SignalQuality,
					"uptimeSeconds":          tm.UptimeSeconds,
					"countryCode":            tm.CountryCode,
					"timestamp":              tm.UtcTimestampNs,
				}
				if len(tm.ActiveAlerts) > 0 {
					entry["activeAlerts"] = tm.ActiveAlerts
				}
				p.store.SetTelemetry(tm.DeviceID, entry)
			}
		}
	}

	// 3. Poll every device through the gateway: status + history.
	for _, d := range p.store.Devices() {
		if p.ctx.Err() != nil {
			break
		}
		p.pollDevice(p.ctx, d)
	}
	return nil
}

// pollDevice fetches status and history; each gateway call is bounded by the
// gateway call timeout, the device as a whole by twice that (plus slack for
// a token refresh).
func (p *Poller) pollDevice(ctx context.Context, d *Device) {
	devCtx, cancel := context.WithTimeout(ctx, 3*p.gateway.CallTimeout())
	defer cancel()

	resp, err := p.gateway.GetStatus(devCtx, d.Kind, d.ID)
	if err != nil {
		p.logDeviceError(d, err)
		return
	}
	st := statusFromResponse(d.Kind, resp)
	if st != nil {
		p.store.SetStatus(d.ID, st)
		p.log.Debug("device status refreshed", "device", d.Name, "state", st.State)
	}

	if d.Kind == KindDish {
		hist, err := p.gateway.GetHistory(devCtx, d.Kind, d.ID)
		if err != nil {
			p.logDeviceError(d, err)
			return
		}
		if n := p.store.AppendHistory(d.ID, hist); n > 0 {
			p.log.Debug("history appended", "device", d.Name, "samples", n)
		}
	}
}

func (p *Poller) logDeviceError(d *Device, err error) {
	if err == nil {
		return
	}
	if err == ErrDeviceOffline {
		p.log.Info("device offline", "device", d.Name, "id", d.ID)
		return
	}
	p.log.Warn("device poll failed", "device", d.Name, "id", d.ID, "err", err)
}

// statusFromResponse converts a gateway get_status response into a
// DeviceStatus for the given device kind.
func statusFromResponse(kind DeviceKind, resp *device.Response) *DeviceStatus {
	if resp == nil {
		return nil
	}
	if kind == KindRouter {
		wifi := resp.GetWifiGetStatus()
		if wifi == nil {
			return nil
		}
		return &DeviceStatus{
			FetchedAt:       time.Now(),
			HardwareVersion: wifi.GetDeviceInfo().GetHardwareVersion(),
			SoftwareVersion: wifi.GetDeviceInfo().GetSoftwareVersion(),
			UptimeS:         wifi.GetDeviceState().GetUptimeS(),
			Wifi: &WifiStatusExtra{
				SerialNumber:   wifi.GetSerialNumber(),
				SKU:            wifi.GetSku(),
				IPv4WANAddress: wifi.GetIpv4WanAddress(),
				PingDropRate:   float64(wifi.GetPingDropRate()),
				PingLatencyMs:  float64(wifi.GetPingLatencyMs()),
				CaptivePortal:  wifi.GetCaptivePortalEnabled(),
			},
		}
	}
	dish := resp.GetDishGetStatus()
	if dish == nil {
		return nil
	}
	info := dish.GetDeviceInfo()
	state := dish.GetDeviceState()
	obs := dish.GetObstructionStats()
	alerts := map[string]bool{}
	for name, active := range alertFields(dish.GetAlerts()) {
		if active {
			alerts[name] = true
		}
	}
	// Current dish firmware omits state and snr; keep them empty instead of
	// reporting UNKNOWN / 0 dB for a dish that is actually connected.
	var stateName string
	if dish.State != nil {
		stateName = dishStateName(dish.GetState())
	}
	var snr *float64
	if dish.Snr != nil {
		v := float64(dish.GetSnr())
		snr = &v
	}
	return &DeviceStatus{
		FetchedAt:           time.Now(),
		HardwareVersion:     info.GetHardwareVersion(),
		SoftwareVersion:     info.GetSoftwareVersion(),
		State:               stateName,
		UptimeS:             state.GetUptimeS(),
		SNR:                 snr,
		PopPingLatencyMs:    float64(dish.GetPopPingLatencyMs()),
		PopPingDropRate:     float64(dish.GetPopPingDropRate()),
		DownlinkThroughput:  float64(dish.GetDownlinkThroughputBps()),
		UplinkThroughput:    float64(dish.GetUplinkThroughputBps()),
		ObstructionFraction: float64(obs.GetFractionObstructed()),
		ObstructionValidS:   float64(obs.GetValidS()),
		ObstructionLast24hS: float64(obs.GetLast_24HObstructedS()),
		CurrentlyObstructed: obs.GetCurrentlyObstructed(),
		StowRequested:       dish.GetStowRequested(),
		Alerts:              alerts,
	}
}

// alertFields reflects the bool alert flags into a name→active map.
func alertFields(a *device.DishAlerts) map[string]bool {
	if a == nil {
		return nil
	}
	return map[string]bool{
		"motors_stuck":           a.GetMotorsStuck(),
		"thermal_shutdown":       a.GetThermalShutdown(),
		"thermal_throttle":       a.GetThermalThrottle(),
		"mast_not_near_vertical": a.GetMastNotNearVertical(),
		"unexpected_location":    a.GetUnexpectedLocation(),
		"slow_ethernet_speeds":   a.GetSlowEthernetSpeeds(),
	}
}

func dishStateName(s device.DishState) string {
	switch s {
	case device.DishState_CONNECTED:
		return "CONNECTED"
	case device.DishState_SEARCHING:
		return "SEARCHING"
	case device.DishState_BOOTING:
		return "BOOTING"
	default:
		return "UNKNOWN"
	}
}

// dishName builds a friendly name: location nickname > service line
// nickname > "STARLINK <last 6 hex>".
func dishName(t *TerminalInfo) string {
	if t.LocationNickname != "" {
		return t.LocationNickname
	}
	if t.ServiceLine != "" {
		return t.ServiceLine
	}
	id := t.UserTerminalID
	if len(id) >= 6 {
		return "STARLINK " + strings.ToUpper(lastHex(id, 6))
	}
	return "STARLINK " + id
}

// routerName builds a friendly router name from its ID.
func routerName(r *RouterInfo) string {
	id := strings.TrimPrefix(r.RouterID, "Router-")
	if len(id) >= 12 {
		return "Router " + lastHex(id, 12)
	}
	return "Router " + id
}

// lastHex returns the last n characters that are hex-ish from a device ID.
func lastHex(id string, n int) string {
	if len(id) > n {
		id = id[len(id)-n:]
	}
	return id
}

// NicknameOrNumber is a helper on ServiceLine.
func (sl *ServiceLine) NicknameOrNumber() string {
	if sl.Nickname != "" {
		return sl.Nickname
	}
	return sl.Number
}
