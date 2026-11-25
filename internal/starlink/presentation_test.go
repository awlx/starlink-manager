package starlink

import (
	"testing"
	"time"

	"github.com/awlx/starlink-manager/api/device"
	"google.golang.org/protobuf/proto"
)

func TestReportedFieldsPreservePresenceAndNestedDiagnostics(t *testing.T) {
	r := &device.DishGetStatusResponse{
		DeviceInfo:       &device.DeviceInfo{CountryCode: proto.String("DE"), UtcOffsetS: proto.Int32(3600)},
		StowRequested:    proto.Bool(false),
		ObstructionStats: &device.DishObstructionStats{Last_24HObstructedS: proto.Float32(0), WedgeFractionObstructed: []float32{0, .2}},
	}
	fields := reportedFields(r)
	for key, want := range map[string]string{"device_info.country_code": "DE", "stow_requested": "false", "obstruction_stats.last_24h_obstructed_s": "0", "obstruction_stats.wedge_fraction_obstructed[1]": "0.2"} {
		if fields[key] != want {
			t.Errorf("%s = %q, want %q", key, fields[key], want)
		}
	}
	if _, ok := fields["snr"]; ok {
		t.Error("absent SNR invented")
	}
}

func TestTelemetryAndRosterDistinguishMissingFromZero(t *testing.T) {
	tm := parseTelemetryEntry(map[string]any{"DeviceId": "ut1", "SignalQuality": float64(0)})
	if !tm.Present["signalQuality"] || tm.Present["pingLatencyMsAvg"] {
		t.Fatal("telemetry presence lost")
	}
	terminal := parseTerminal(map[string]any{"id": "ut1", "latitude": float64(0), "longitude": float64(0)}, "SL")
	fields := terminalDetails(terminal, &ServiceLine{Number: "SL"})
	if fields["Latitude / longitude (roster)"] != "0.000000, 0.000000" {
		t.Error("reported zero coordinates omitted")
	}
	if _, ok := fields["Offline (roster snapshot)"]; ok {
		t.Error("absent offline flag invented")
	}
}

func TestHistoryAndClientsKeepAdditionalReportedFields(t *testing.T) {
	store := NewStore(t.TempDir(), time.Hour, time.Now)
	store.UpsertDevice(&Device{ID: "test"})
	store.AppendHistory("test", &device.DishGetHistoryResponse{
		Current: proto.Uint64(1), PopPingLatencyMs: []float32{20}, Snr: []float32{0}, Obstructed: []bool{false},
	})
	samples := store.History("test", time.Hour)
	if len(samples) != 1 || !samples[0].SNRReported || !samples[0].ObstructedReported || samples[0].ScheduledReported {
		t.Fatalf("presence lost: %+v", samples)
	}
	store.SetClients("test", []*device.WifiClient{{
		Name: proto.String("client"), AssociatedTimeS: proto.Uint32(10),
		RxStats: &device.WifiClient_RxStats{Bytes: proto.Uint64(123), CountErrors: proto.Uint64(0)},
	}})
	d, _ := store.Device("test")
	if d.ClientDetails["clients[0].rx_stats.bytes"] != "123" || d.ClientDetails["clients[0].rx_stats.count_errors"] != "0" {
		t.Fatal("client diagnostics lost")
	}
}
