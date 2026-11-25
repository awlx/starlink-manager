package starlink

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strconv"
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
	Present                map[string]bool
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
	rows, err := parseTelemetryResponse(data)
	if err == nil && t.session != nil {
		// A successful authenticated telemetry query proves the session
		// is alive.
		t.session.MarkAlive()
	}
	return rows, err
}

func parseTelemetryResponse(data []byte) ([]*DeviceTelemetry, error) {
	var root any
	if err := json.Unmarshal(data, &root); err != nil {
		return nil, fmt.Errorf("telemetry response: %w", err)
	}
	// Production shape: {"data":{"values":[["u",ts,id,...],...],
	// "columnNamesByDeviceType":{"u":[...],"r":[...]}},"metadata":{...}}.
	// Rows are positional arrays; column names come per device type.
	var wrapper struct {
		Data struct {
			Values                  []json.RawMessage   `json:"values"`
			ColumnNamesByDeviceType map[string][]string `json:"columnNamesByDeviceType"`
		} `json:"data"`
		Metadata struct {
			Enums struct {
				AlertsByDeviceType map[string]map[string]any `json:"AlertsByDeviceType"`
			} `json:"enums"`
		} `json:"metadata"`
	}
	var entries []map[string]any
	var alertEnums map[string]map[string]any
	if err := json.Unmarshal(data, &wrapper); err == nil && len(wrapper.Data.Values) > 0 && wrapper.Data.ColumnNamesByDeviceType != nil {
		alertEnums = wrapper.Metadata.Enums.AlertsByDeviceType
		for _, rv := range wrapper.Data.Values {
			var row []any
			if err := json.Unmarshal(rv, &row); err != nil {
				continue
			}
			if len(row) == 0 {
				continue
			}
			dtype, _ := row[0].(string)
			cols := wrapper.Data.ColumnNamesByDeviceType[dtype]
			if len(cols) == 0 {
				continue
			}
			m := map[string]any{}
			for i, name := range cols {
				if i < len(row) {
					m[name] = row[i]
				}
			}
			entries = append(entries, m)
		}
	}
	if entries == nil {
		switch v := root.(type) {
		case []any:
			entries = toMaps(v)
		case map[string]any:
			// Fallback shapes: {"devices": [...]}, {"results": [...]}, object-per-device maps
			for _, key := range []string{"devices", "data", "results", "telemetry"} {
				if raw, ok := v[key]; ok {
					if arr, ok := raw.([]any); ok {
						entries = toMaps(arr)
						break
					}
				}
			}
			if entries == nil {
				// {"<deviceId>": {...}, ...} object-of-maps shape
				entries = toMaps(mapValues(v))
			}
		}
	}
	var out []*DeviceTelemetry
	for _, e := range entries {
		if tm := parseTelemetryEntry(e); tm != nil {
			translateAlertCodes(alertEnums, tm)
			out = append(out, tm)
		}
	}
	return out, nil
}

// translateAlertCodes maps numeric alert codes to the names the account API
// publishes in metadata.enums.AlertsByDeviceType (e.g. 50 →
// ethernet_slow_10mbps, 51 → ethernet_slow_100mbps). Unknown codes are kept
// as-is so new firmware alerts still surface.
func translateAlertCodes(enum map[string]map[string]any, tm *DeviceTelemetry) {
	if len(enum) == 0 || len(tm.ActiveAlerts) == 0 {
		return
	}
	for i, a := range tm.ActiveAlerts {
		if _, err := strconv.Atoi(a); err != nil {
			continue // already a name
		}
		for _, codes := range enum {
			if v, ok := codes[a]; ok {
				if name, ok := v.(string); ok && name != "" {
					tm.ActiveAlerts[i] = name
				}
				break
			}
		}
	}
}

func mapValues(m map[string]any) []any {
	out := make([]any, 0, len(m))
	for _, v := range m {
		out = append(out, v)
	}
	return out
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
		SoftwareVersion:        mapString(m, "RunningSoftwareVersion", "SoftwareVersion", "softwareVersion"),
		DownlinkThroughputMbps: mapFloat(m, "DownlinkThroughputMbps", "DownlinkThroughput", "downlinkThroughputMbps"),
		UplinkThroughputMbps:   mapFloat(m, "UplinkThroughputMbps", "UplinkThroughput", "uplinkThroughputMbps"),
		PingDropRateAvg:        mapFloat(m, "PingDropRateAvg", "PingDropRate", "pingDropRateAvg"),
		PingLatencyMsAvg:       mapFloat(m, "PingLatencyMsAvg", "PingLatencyMs", "pingLatencyMsAvg"),
		ObstructionPercentTime: mapFloat(m, "ObstructionPercentTime", "obstructionPercentTime"),
		SignalQuality:          mapFloat(m, "SignalQuality", "signalQuality"),
		UptimeSeconds:          mapFloat(m, "UptimeSeconds", "Uptime", "uptimeSeconds", "Uptime"),
		CountryCode:            mapString(m, "CountryCode", "countryCode"),
		Present:                map[string]bool{},
	}
	for key, aliases := range map[string][]string{
		"timestamp":              {"UtcTimestampNs", "utcTimestampNs"},
		"softwareVersion":        {"RunningSoftwareVersion", "SoftwareVersion", "softwareVersion"},
		"downlinkThroughputMbps": {"DownlinkThroughputMbps", "DownlinkThroughput", "downlinkThroughputMbps"},
		"uplinkThroughputMbps":   {"UplinkThroughputMbps", "UplinkThroughput", "uplinkThroughputMbps"},
		"pingDropRateAvg":        {"PingDropRateAvg", "PingDropRate", "pingDropRateAvg"},
		"pingLatencyMsAvg":       {"PingLatencyMsAvg", "PingLatencyMs", "pingLatencyMsAvg"},
		"obstructionPercentTime": {"ObstructionPercentTime", "obstructionPercentTime"},
		"signalQuality":          {"SignalQuality", "signalQuality"},
		"uptimeSeconds":          {"UptimeSeconds", "Uptime", "uptimeSeconds"},
		"countryCode":            {"CountryCode", "countryCode"},
	} {
		v, ok := lookup(m, aliases...)
		tm.Present[key] = ok && v != nil
	}
	if alerts, ok := lookup(m, "ActiveAlert", "ActiveAlerts", "activeAlerts"); ok {
		switch v := alerts.(type) {
		case []any:
			for _, item := range v {
				tm.ActiveAlerts = append(tm.ActiveAlerts, fmt.Sprintf("%v", item))
			}
		case float64:
			// Numeric alert codes arrive as floats in positional rows.
			if v != 0 {
				tm.ActiveAlerts = append(tm.ActiveAlerts, fmt.Sprintf("%d", int64(v)))
			}
		case string:
			if v != "" {
				tm.ActiveAlerts = []string{v}
			}
		}
	}
	return tm
}
