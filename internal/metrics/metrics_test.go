package metrics

import (
	"strings"
	"testing"
)

func TestRegistryRenderGaugeAndCounter(t *testing.T) {
	r := New(nil)
	r.SetGauge("starlink_devices_total", "", 3)
	r.SetGauge("starlink_devices_online", "", 1)
	r.SetGauge("starlink_session_dead", "", 0)
	r.SetDeviceGauge("starlink_device_up", "ut1234", "Dish Kitchen", 1)
	r.SetDeviceGauge("starlink_device_up", "ut5678", "Dish Backyard", 0)
	r.IncCounter("starlink_poll_errors_total", "")

	out := r.Render()
	for _, want := range []string{
		"# HELP starlink_devices_total Number of devices known on the account",
		"# TYPE starlink_devices_total gauge",
		"starlink_devices_total 3",
		"starlink_devices_online 1",
		"starlink_session_dead 0",
		"# TYPE starlink_device_up gauge",
		`starlink_device_up{device="ut1234",name="Dish Kitchen"} 1`,
		`starlink_device_up{device="ut5678",name="Dish Backyard"} 0`,
		"# TYPE starlink_poll_errors_total counter",
		"starlink_poll_errors_total 1",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("render missing %q\n---\n%s", want, out)
		}
	}
}

func TestRegistryRenderLabelEscaping(t *testing.T) {
	r := New(nil)
	// A device label containing a quote must be escaped via %q quoting.
	r.SetDeviceGauge("starlink_device_up", "utX", `Dish "quoted"`, 1)
	out := r.Render()
	if !strings.Contains(out, `starlink_device_up{device="utX",name="Dish \"quoted\""} 1`) {
		t.Errorf("label not escaped:\n%s", out)
	}
}

func TestRegistryRenderHistogram(t *testing.T) {
	r := New(nil)
	for _, v := range []float64{0.05, 0.3, 0.8, 1.5, 6.0} {
		r.ObserveHistogram("starlink_poll_round_duration_seconds", v)
	}
	out := r.Render()
	for _, want := range []string{
		"# TYPE starlink_poll_round_duration_seconds histogram",
		`starlink_poll_round_duration_seconds{le="0.1"} 1`,
		`starlink_poll_round_duration_seconds{le="0.5"} 2`,
		`starlink_poll_round_duration_seconds{le="1"} 3`,
		`starlink_poll_round_duration_seconds{le="2"} 4`,
		`starlink_poll_round_duration_seconds{le="5"} 4`,
		`starlink_poll_round_duration_seconds{le="10"} 5`,
		`starlink_poll_round_duration_seconds{le="+Inf"} 5`,
		"starlink_poll_round_duration_seconds_sum 8.65",
		"starlink_poll_round_duration_seconds_count 5",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("histogram render missing %q\n---\n%s", want, out)
		}
	}
}

func TestRegistryRenderStableAndSorted(t *testing.T) {
	r := New(nil)
	r.SetDeviceGauge("starlink_device_up", "ut2", "B", 1)
	r.SetDeviceGauge("starlink_device_up", "ut1", "A", 1)
	r.SetDeviceGauge("starlink_device_up", "ut3", "C", 0)
	first := r.Render()
	second := r.Render()
	if first != second {
		t.Errorf("render not stable between calls")
	}
	i1, i2, i3 := strings.Index(first, "ut1"), strings.Index(first, "ut2"), strings.Index(first, "ut3")
	if !(i1 < i2 && i2 < i3) {
		t.Errorf("device series not sorted by label key:\n%s", first)
	}
}

func TestRegistryRenderEmpty(t *testing.T) {
	r := New(nil)
	if out := r.Render(); out != "" {
		t.Errorf("empty registry should render nothing, got:\n%s", out)
	}
}

func TestRegistryCounterAccumulates(t *testing.T) {
	r := New(nil)
	r.IncCounter("starlink_poll_errors_total", "")
	r.IncCounter("starlink_poll_errors_total", "")
	r.IncCounter("starlink_poll_errors_total", "")
	out := r.Render()
	if !strings.Contains(out, "starlink_poll_errors_total 3") {
		t.Errorf("counter did not accumulate:\n%s", out)
	}
}

func TestRegistryGaugeReplaces(t *testing.T) {
	r := New(nil)
	r.SetGauge("starlink_session_dead", "", 1)
	r.SetGauge("starlink_session_dead", "", 0)
	out := r.Render()
	if strings.Count(out, "starlink_session_dead 0") != 1 || strings.Contains(out, "starlink_session_dead 1\n") {
		t.Errorf("gauge should be replaced, got:\n%s", out)
	}
}
