// Package metrics exposes Prometheus-format operational metrics on a
// separate listener so monitoring can scrape the app without going through
// the authenticated web UI. The exposition is hand-rolled (a single
// text-format handler) to keep the dependency surface small; values are
// updated by the poller and web layers through a small registry.
package metrics

import (
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

// Registry collects metric samples. Gauges are plain values; the histogram
// and counters are aggregated here in float seconds.
type Registry struct {
	mu     sync.Mutex
	gauges map[string]float64 // name{labels} -> value
	// counters accumulate monotonically increasing totals.
	counters map[string]float64
	// histSamples collect per-bucket cumulative counts keyed by metric name.
	histSamples map[string][]float64
	log         *slog.Logger
}

// New creates an empty registry.
func New(log *slog.Logger) *Registry {
	return &Registry{
		gauges:      map[string]float64{},
		counters:    map[string]float64{},
		histSamples: map[string][]float64{},
		log:         log,
	}
}

// SetGauge records a gauge value, replacing any previous one.
func (r *Registry) SetGauge(name string, labels string, value float64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.gauges[name+"{"+labels+"}"] = value
}

// SetDeviceGauge records a gauge for one device (name{device="id",name="..."}).
func (r *Registry) SetDeviceGauge(name string, id, label string, value float64) {
	r.SetGauge(name, fmt.Sprintf("device=%q,name=%q", id, label), value)
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
	r.histSamples[name] = append(r.histSamples[name], v)
	if len(r.histSamples[name]) > 10000 { // bounded memory
		r.histSamples[name] = r.histSamples[name][len(r.histSamples[name])/2:]
	}
}

// Render produces the Prometheus text exposition.
func (r *Registry) Render() string {
	r.mu.Lock()
	defer r.mu.Unlock()

	var b strings.Builder
	write := func(name, help, typ string, entries func(func(name, labels string, val float64))) {
		b.WriteString("# HELP " + name + " " + help + "\n")
		b.WriteString("# TYPE " + name + " " + typ + "\n")
		entries(func(name, labels string, val float64) {
			b.WriteString(name)
			if labels != "" {
				b.WriteString("{" + labels + "}")
			}
			b.WriteString(" " + formatFloat(val) + "\n")
		})
	}

	// Gauge families, grouped per metric name (labels sorted for stability).
	gaugeNames := map[string]bool{}
	for key := range r.gauges {
		gaugeNames[key[:strings.Index(key, "{")]] = true
	}
	for _, name := range sortedKeys(gaugeNames) {
		write(name, gaugeHelp(name), "gauge", func(f func(string, string, float64)) {
			var keys []string
			for key := range r.gauges {
				if strings.HasPrefix(key, name+"{") {
					keys = append(keys, key)
				}
			}
			sort.Strings(keys)
			for _, key := range keys {
				f(name, key[len(name)+1:len(key)-1], r.gauges[key])
			}
		})
	}

	// Counter families.
	counterNames := map[string]bool{}
	for key := range r.counters {
		counterNames[key[:strings.Index(key, "{")]] = true
	}
	for _, name := range sortedKeys(counterNames) {
		write(name, counterHelp(name), "counter", func(f func(string, string, float64)) {
			var keys []string
			for key := range r.counters {
				if strings.HasPrefix(key, name+"{") {
					keys = append(keys, key)
				}
			}
			sort.Strings(keys)
			for _, key := range keys {
				f(name, key[len(name)+1:len(key)-1], r.counters[key])
			}
		})
	}

	// Histogram family: cumulative buckets 0.1/0.5/1/2/5/10/30s plus inf.
	histNames := []string{}
	for name := range r.histSamples {
		histNames = append(histNames, name)
	}
	sort.Strings(histNames)
	for _, name := range histNames {
		samples := r.histSamples[name]
		write(name, histogramHelp(name), "histogram", func(f func(string, string, float64)) {
			// Cumulative bucket counts.
			bounds := []float64{0.1, 0.5, 1, 2, 5, 10, 30}
			for _, bound := range bounds {
				count := 0
				for _, v := range samples {
					if v <= bound {
						count++
					}
				}
				f(name, fmt.Sprintf("le=%q", fmt.Sprintf("%g", bound)), float64(count))
			}
			f(name, "le=\"+Inf\"", float64(len(samples)))
			sum := 0.0
			for _, v := range samples {
				sum += v
			}
			f(name+"_sum", "", sum)
			f(name+"_count", "", float64(len(samples)))
		})
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
	if v == float64(int64(v)) {
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
