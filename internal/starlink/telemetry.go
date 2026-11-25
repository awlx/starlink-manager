package starlink

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
)

// DeviceTelemetry is one enriched telemetry snapshot from the device-data
// cache endpoint. Cloud telemetry refreshes roughly every two minutes per
// device; values are treated as informational enrichment for the UI.
type DeviceTelemetry struct {
	DeviceID               string
	UtcTimestampNs         int64
	SoftwareVersion        string
	DownlinkThroughputMbps float64
	UplinkThroughputMbps   float64
	PingDropRateAvg        float64
	PingLatencyMsAvg       float64
	ObstructionPercentTime float64
	SignalQuality          float64
	UptimeSeconds          float64
	CountryCode            string
	ActiveAlerts           []string
}

// Telemetry queries the device-data telemetry cache. accountNumber comes
// from the session probe; the filter asks for everything newer than the
// given millisecond timestamp (0 = everything).
type Telemetry struct {
	session *Session
	apiBase string
	log     *slog.Logger
}

// NewTelemetry creates a telemetry cache client.
func NewTelemetry(session *Session, apiBase string, log *slog.Logger) *Telemetry {
	return &Telemetry{session: session, apiBase: strings.TrimRight(apiBase, "/"), log: log}
}

// Query returns telemetry snapshots for all account devices newer than
// afterMs. Failures are returned to the caller, which decides how to
// degrade (the UI tolerates missing enrichment).
func (t *Telemetry) Query(ctx context.Context, accountNumber string, afterMs int64) ([]*DeviceTelemetry, error) {
	body := map[string]any{
		"accountNumber": accountNumber,
		"filters": []map[string]any{
			{"operator": "gt", "field": "UtcTimestampNs", "value": afterMs},
		},
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	u := t.apiBase + "/device-data/cache/v1/telemetry"
	_, data, err := t.session.request(ctx, "POST", u, payload, "application/json")
	if err != nil {
		return nil, fmt.Errorf("telemetry query: %w", err)
	}
	return parseTelemetryResponse(data)
}

func parseTelemetryResponse(data []byte) ([]*DeviceTelemetry, error) {
	var root any
	if err := json.Unmarshal(data, &root); err != nil {
		return nil, fmt.Errorf("telemetry response: %w", err)
	}
	var entries []map[string]any
	switch v := root.(type) {
	case []any:
		entries = toMaps(v)
	case map[string]any:
		// Common wrapper shapes: {"devices": [...]}, {"data": [...]}, {"results": [...]}
		for _, key := range []string{"devices", "data", "results", "telemetry"} {
			if raw, ok := v[key]; ok {
				if arr, ok := raw.([]any); ok {
					entries = toMaps(arr)
					break
				}
			}
		}
	}
	var out []*DeviceTelemetry
	for _, e := range entries {
		if tm := parseTelemetryEntry(e); tm != nil {
			out = append(out, tm)
		}
	}
	return out, nil
}

func toMaps(arr []any) []map[string]any {
	out := make([]map[string]any, 0, len(arr))
	for _, item := range arr {
		if m, ok := item.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

func parseTelemetryEntry(m map[string]any) *DeviceTelemetry {
	id := mapString(m, "DeviceId", "deviceId", "device_id", "userTerminalId", "routerId")
	if id == "" {
		return nil
	}
	tm := &DeviceTelemetry{
		DeviceID:               id,
		UtcTimestampNs:         int64(mapFloat(m, "UtcTimestampNs", "utcTimestampNs")),
		SoftwareVersion:        mapString(m, "SoftwareVersion", "softwareVersion"),
		DownlinkThroughputMbps: mapFloat(m, "DownlinkThroughputMbps", "downlinkThroughputMbps"),
		UplinkThroughputMbps:   mapFloat(m, "UplinkThroughputMbps", "uplinkThroughputMbps"),
		PingDropRateAvg:        mapFloat(m, "PingDropRateAvg", "pingDropRateAvg"),
		PingLatencyMsAvg:       mapFloat(m, "PingLatencyMsAvg", "pingLatencyMsAvg"),
		ObstructionPercentTime: mapFloat(m, "ObstructionPercentTime", "obstructionPercentTime"),
		SignalQuality:          mapFloat(m, "SignalQuality", "signalQuality"),
		UptimeSeconds:          mapFloat(m, "UptimeSeconds", "uptimeSeconds", "Uptime"),
		CountryCode:            mapString(m, "CountryCode", "countryCode"),
	}
	if alerts, ok := lookup(m, "ActiveAlert", "activeAlerts", "ActiveAlerts"); ok {
		switch v := alerts.(type) {
		case []any:
			for _, item := range v {
				tm.ActiveAlerts = append(tm.ActiveAlerts, fmt.Sprintf("%v", item))
			}
		case string:
			if v != "" {
				tm.ActiveAlerts = []string{v}
			}
		}
	}
	return tm
}
