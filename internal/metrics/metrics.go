// Package metrics exposes Prometheus-format operational metrics on a
// separate listener so monitoring can scrape the app without going through
// the authenticated web UI. The exposition is hand-rolled (a single
// text-format handler) to keep the dependency surface small; values are
// updated by the poller and web layers through a small registry.
package metrics

import (
	"fmt"
	"log/slog"
	"math"
	"net"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

// histogramBounds are the upper bounds (seconds) of the histogram buckets.
var histogramBounds = []float64{0.1, 0.5, 1, 2, 5, 10, 30}

// histogram keeps cumulative-able bucket counts, so _count and _sum stay
// monotonic for the lifetime of the process.
type histogram struct {
	buckets []uint64 // per-bound (non-cumulative) counts; len(histogramBounds)
	count   uint64
	sum     float64
}

// Registry collects metric samples. Gauges are plain values; counters and
// the histogram accumulate monotonically.
type Registry struct {
	mu     sync.Mutex
	gauges map[string]float64 // name{labels} -> value
	// deviceSeries maps device id -> metric name -> gauge key, so a rename
	// replaces the old series and a retired device can be removed.
	deviceSeries map[string]map[string]string
	counters     map[string]float64
	histograms   map[string]*histogram
	collectors   []func()
	log          *slog.Logger
}

// New creates an empty registry.
func New(log *slog.Logger) *Registry {
	return &Registry{
		gauges:       map[string]float64{},
		deviceSeries: map[string]map[string]string{},
		counters:     map[string]float64{},
		histograms:   map[string]*histogram{},
		log:          log,
	}
}

// OnCollect registers a function that refreshes gauges right before every
// scrape (called without the registry lock held).
func (r *Registry) OnCollect(fn func()) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.collectors = append(r.collectors, fn)
}

// SetGauge records a gauge value, replacing any previous one. labels must
// already be in exposition form (see Label).
func (r *Registry) SetGauge(name string, labels string, value float64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.gauges[name+"{"+labels+"}"] = value
}

// SetDeviceGauge records a gauge for one device (name{device="id",name="..."}).
// A previous series of the same metric for that device (e.g. under an old
// device name) is replaced.
func (r *Registry) SetDeviceGauge(name string, id, label string, value float64) {
	key := name + "{" + Label("device", id) + "," + Label("name", label) + "}"
	r.mu.Lock()
	defer r.mu.Unlock()
	series := r.deviceSeries[id]
	if series == nil {
		series = map[string]string{}
		r.deviceSeries[id] = series
	}
	if old, ok := series[name]; ok && old != key {
		delete(r.gauges, old)
	}
	series[name] = key
	r.gauges[key] = value
}

// DeleteDevice removes every gauge series of a device (retired or pruned),
// so stale values such as starlink_device_up=1 do not linger.
func (r *Registry) DeleteDevice(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, key := range r.deviceSeries[id] {
		delete(r.gauges, key)
	}
	delete(r.deviceSeries, id)
}

// IncCounter adds to a counter.
func (r *Registry) IncCounter(name, labels string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	key := name + "{" + labels + "}"
	r.counters[key]++
}

// ObserveHistogram adds one observation to the histogram buckets.
func (r *Registry) ObserveHistogram(name string, v float64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	h := r.histograms[name]
	if h == nil {
		h = &histogram{buckets: make([]uint64, len(histogramBounds))}
		r.histograms[name] = h
	}
	for i, bound := range histogramBounds {
		if v <= bound {
			h.buckets[i]++
			break
		}
	}
	h.count++
	h.sum += v
}

// Label formats one label pair with Prometheus text-format escaping
// (backslash, double quote and newline; everything else is literal UTF-8).
func Label(name, value string) string {
	var b strings.Builder
	b.WriteString(name)
	b.WriteString(`="`)
	for _, c := range value {
		switch c {
		case '\\':
			b.WriteString(`\\`)
		case '"':
			b.WriteString(`\"`)
		case '\n':
			b.WriteString(`\n`)
		default:
			b.WriteRune(c)
		}
	}
	b.WriteByte('"')
	return b.String()
}

// Render produces the Prometheus text exposition. Registered collectors
// run first so fleet-level gauges are current at scrape time.
func (r *Registry) Render() string {
	r.mu.Lock()
	collectors := append([]func(){}, r.collectors...)
	r.mu.Unlock()
	for _, fn := range collectors {
		fn()
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	var b strings.Builder
	header := func(name, help, typ string) {
		b.WriteString("# HELP " + name + " " + help + "\n")
		b.WriteString("# TYPE " + name + " " + typ + "\n")
	}
	sample := func(name, labels string, val float64) {
		b.WriteString(name)
		if labels != "" {
			b.WriteString("{" + labels + "}")
		}
		b.WriteString(" " + formatFloat(val) + "\n")
	}
	family := func(m map[string]float64, typ string, help func(string) string) {
		byName := map[string][]string{}
		for key := range m {
			name := key[:strings.Index(key, "{")]
			byName[name] = append(byName[name], key)
		}
		names := make([]string, 0, len(byName))
		for name := range byName {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			keys := byName[name]
			sort.Strings(keys)
			header(name, help(name), typ)
			for _, key := range keys {
				sample(name, key[len(name)+1:len(key)-1], m[key])
			}
		}
	}
	family(r.gauges, "gauge", gaugeHelp)
	family(r.counters, "counter", counterHelp)

	histNames := make([]string, 0, len(r.histograms))
	for name := range r.histograms {
		histNames = append(histNames, name)
	}
	sort.Strings(histNames)
	for _, name := range histNames {
		h := r.histograms[name]
		header(name, histogramHelp(name), "histogram")
		var cum uint64
		for i, bound := range histogramBounds {
			cum += h.buckets[i]
			sample(name+"_bucket", Label("le", formatFloat(bound)), float64(cum))
		}
		sample(name+"_bucket", `le="+Inf"`, float64(h.count))
		sample(name+"_sum", "", h.sum)
		sample(name+"_count", "", float64(h.count))
	}
	return b.String()
}

func sortedKeys(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func formatFloat(v float64) string {
	switch {
	case math.IsNaN(v):
		return "NaN"
	case math.IsInf(v, 1):
		return "+Inf"
	case math.IsInf(v, -1):
		return "-Inf"
	}
	if v == math.Trunc(v) && math.Abs(v) < 1e15 {
		return fmt.Sprintf("%d", int64(v))
	}
	return fmt.Sprintf("%g", v)
}

func gaugeHelp(name string) string {
	switch name {
	case "starlink_device_up":
		return "1 when the device answered a poll within the online TTL, else 0"
	case "starlink_device_pop_ping_drop_rate":
		return "Fraction of ping drops per device (0..1), most recent status"
	case "starlink_device_pop_ping_latency_ms":
		return "Pop ping latency in milliseconds, most recent status"
	case "starlink_device_downlink_bps":
		return "Downlink throughput in bits per second, most recent status"
	case "starlink_device_uplink_bps":
		return "Uplink throughput in bits per second, most recent status"
	case "starlink_device_obstruction_fraction":
		return "Fraction of the sky view obstructed (0..1), most recent status"
	case "starlink_devices_total":
		return "Number of devices known on the account"
	case "starlink_devices_online":
		return "Number of devices that answered a poll within the online TTL"
	case "starlink_session_dead":
		return "1 while the Starlink session is latched dead"
	case "starlink_store_size_bytes":
		return "Size of the persisted store file in bytes"
	}
	return name
}

func counterHelp(name string) string {
	switch name {
	case "starlink_poll_errors_total":
		return "Poll rounds that ended with an error"
	}
	return name
}

func histogramHelp(name string) string {
	switch name {
	case "starlink_poll_round_duration_seconds":
		return "Duration of one complete poll round in seconds"
	}
	return name
}

// Serve mounts the /metrics endpoint on the given address. Bind it to a
// private interface (loopback or nebula) — it is intentionally not behind
// the web-app authentication.
func Serve(r *Registry, addr string, log *slog.Logger) (*http.Server, error) {
	mux := http.NewServeMux()
	mux.HandleFunc("/metrics", func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		_, _ = w.Write([]byte(r.Render()))
	})
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		log.Info("metrics listening", "addr", addr)
		if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			log.Error("metrics server failed", "err", err)
		}
	}()
	return srv, nil
}
