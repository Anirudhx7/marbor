package marboragent

import (
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

var nonFiniteTokens = []string{"nan", "NaN", "+Inf", "inf", "Inf", "-Inf", "infinity", "-infinity"}

func TestParseFiniteFloatRejectsNonFinite(t *testing.T) {
	for _, s := range nonFiniteTokens {
		if _, ok := parseFiniteFloat(s); ok {
			t.Errorf("parseFiniteFloat(%q) ok=true, want false", s)
		}
	}
	if v, ok := parseFiniteFloat("1.5"); !ok || v != 1.5 {
		t.Errorf("parseFiniteFloat(1.5) = %v,%v", v, ok)
	}
}

func TestParsePrometheusTextDropsNonFinite(t *testing.T) {
	out := parsePrometheusText([]byte("a NaN\nb +Inf\nc -Inf\nd 2\n"))
	for _, n := range []string{"a", "b", "c"} {
		if _, ok := out[n]; ok {
			t.Errorf("sample %q kept, want dropped", n)
		}
	}
	if len(out["d"]) != 1 {
		t.Errorf("finite sample lost: %v", out)
	}
}

func TestClampPercentNonFiniteIsNil(t *testing.T) {
	for _, v := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		if clampPercent(v) != nil {
			t.Errorf("clampPercent(%v) != nil", v)
		}
	}
}

func TestSumSamplesRejectsNonFiniteSum(t *testing.T) {
	_, ok := sumSamples(map[string][]float64{"x": {math.Inf(1), math.Inf(-1)}}, "x")
	if ok {
		t.Error("Inf + -Inf sum accepted")
	}
	if _, ok := sumSamples(map[string][]float64{"x": {math.MaxFloat64, math.MaxFloat64}}, "x"); ok {
		t.Error("overflowing sum accepted")
	}
}

func TestNvidiaParsersRejectNonFinite(t *testing.T) {
	for _, s := range nonFiniteTokens {
		if _, ok := parseCelsius(s); ok {
			t.Errorf("parseCelsius(%q) ok", s)
		}
		if _, ok := parseWatts(s); ok {
			t.Errorf("parseWatts(%q) ok", s)
		}
		if _, ok := parsePercent(s); ok {
			t.Errorf("parsePercent(%q) ok", s)
		}
	}
	for _, s := range []string{"101 %", "-1 %"} {
		if _, ok := parsePercent(s); ok {
			t.Errorf("parsePercent(%q) out of range accepted", s)
		}
	}
}

func TestIntelStatsRejectNonFinite(t *testing.T) {
	for _, s := range nonFiniteTokens {
		info := GPUInfo{}
		applyXPUStats(&info, []byte(`{"device_level":[{"metrics_type":"GPU_TEMPERATURE","value":"`+s+`"},{"metrics_type":"GPU_POWER","value":"`+s+`"},{"metrics_type":"GPU_UTILIZATION","value":"`+s+`"}]}`))
		if info.TemperatureC != nil || info.PowerWatts != nil || info.CorePercent != nil {
			t.Errorf("%s accepted: %+v", s, info)
		}
	}
	info := GPUInfo{}
	applyXPUStats(&info, []byte(`{"device_level":[{"metrics_type":"GPU_UTILIZATION","value":"250"}]}`))
	if info.CorePercent != nil {
		t.Error("out-of-range utilization accepted")
	}
}

func TestROCmRejectNonFinite(t *testing.T) {
	for _, s := range nonFiniteTokens {
		data := `{"card0":{"Temperature (Sensor edge) (C)":"` + s + `","Fan speed (%)":"` + s + `","Average Graphics Package Power (W)":"` + s + `","GPU use (%)":"` + s + `"}}`
		block, ok := parseROCmSMIJSON([]byte(data))
		if !ok {
			t.Fatalf("parse failed for %s", s)
		}
		d := block.Devices[0]
		if d.TemperatureC != nil || d.FanPercent != nil || d.PowerWatts != nil || d.CorePercent != nil {
			t.Errorf("%s accepted: %+v", s, d)
		}
	}
	block, _ := parseROCmSMIJSON([]byte(`{"card0":{"Fan speed (%)":"900","GPU use (%)":"-5"}}`))
	if d := block.Devices[0]; d.FanPercent != nil || d.CorePercent != nil {
		t.Errorf("out-of-range percent accepted: %+v", d)
	}
}

func TestParseFiniteFloatAccepts(t *testing.T) {
	for in, want := range map[string]float64{"+5": 5, " 7.5 ": 7.5, "0": 0, "-3": -3, "1e3": 1000} {
		if v, ok := parseFiniteFloat(in); !ok || v != want {
			t.Errorf("parseFiniteFloat(%q) = %v,%v want %v,true", in, v, ok, want)
		}
	}
	for _, in := range []string{"", "abc", "1e999"} {
		if _, ok := parseFiniteFloat(in); ok {
			t.Errorf("parseFiniteFloat(%q) ok", in)
		}
	}
}

func TestPercentBoundariesAccepted(t *testing.T) {
	for _, s := range []string{"0 %", "100 %"} {
		if _, ok := parsePercent(s); !ok {
			t.Errorf("parsePercent(%q) rejected a boundary", s)
		}
	}
	for _, v := range []float64{0, 100} {
		if !validPercent(v) {
			t.Errorf("validPercent(%v) = false", v)
		}
	}
	for _, v := range []float64{-0.001, 100.001, math.NaN(), math.Inf(1)} {
		if validPercent(v) {
			t.Errorf("validPercent(%v) = true", v)
		}
	}
	block, _ := parseROCmSMIJSON([]byte(`{"card0":{"Fan speed (%)":"100","GPU use (%)":"0"}}`))
	if d := block.Devices[0]; d.FanPercent == nil || *d.FanPercent != 100 || d.CorePercent == nil || *d.CorePercent != 0 {
		t.Errorf("rocm boundaries dropped: %+v", d)
	}
	info := GPUInfo{}
	applyXPUStats(&info, []byte(`{"device_level":[{"metrics_type":"GPU_UTILIZATION","value":"100"}]}`))
	if info.CorePercent == nil || *info.CorePercent != 100 {
		t.Errorf("intel boundary dropped: %+v", info)
	}
}

// Temperature and power are not range-checked (a sensor's plausible range is
// card-specific), so finite extremes pass through unchanged; only non-finite
// readings are dropped.
func TestTempPowerFiniteExtremesPassThrough(t *testing.T) {
	block, _ := parseROCmSMIJSON([]byte(`{"card0":{"Temperature (Sensor edge) (C)":"-40","Average Graphics Package Power (W)":"1000000"}}`))
	d := block.Devices[0]
	if d.TemperatureC == nil || *d.TemperatureC != -40 || d.PowerWatts == nil || *d.PowerWatts != 1e6 {
		t.Errorf("rocm extremes: %+v", d)
	}
	info := GPUInfo{}
	applyXPUStats(&info, []byte(`{"device_level":[{"metrics_type":"GPU_TEMPERATURE","value":"-273"},{"metrics_type":"GPU_POWER","value":"-1"}]}`))
	if info.TemperatureC == nil || *info.TemperatureC != -273 || info.PowerWatts == nil || *info.PowerWatts != -1 {
		t.Errorf("intel extremes: %+v", info)
	}
}

func TestParsePrometheusTextMixedFiniteAndNaN(t *testing.T) {
	out := parsePrometheusText([]byte("m{a=\"1\"} 2\nm{a=\"2\"} NaN\nm{a=\"3\"} 3\nm{a=\"4\"} +Inf\n"))
	if got := out["m"]; len(got) != 2 || got[0] != 2 || got[1] != 3 {
		t.Fatalf("m = %v, want [2 3]", got)
	}
	if sum, ok := sumSamples(out, "m"); !ok || sum != 5 {
		t.Errorf("sumSamples = %v,%v want 5,true", sum, ok)
	}
}

func TestRenderPrometheusSkipsNonFinite(t *testing.T) {
	nan, inf := math.NaN(), math.Inf(1)
	core, cpu := 55.0, 12.0
	tel := Telemetry{
		GPU:  &GPUBlock{Devices: []GPUInfo{{CorePercent: &core, TemperatureC: &inf}, {Index: 1, CorePercent: &nan}}},
		Host: &HostTelemetry{CPUPercent: &cpu, DiskFreeGB: inf, DiskTotalGB: 500},
	}
	out := RenderPrometheus(tel)
	for _, bad := range []string{"NaN", "Inf"} {
		if strings.Contains(out, bad) {
			t.Errorf("metrics output contains %s:\n%s", bad, out)
		}
	}
	if !strings.Contains(out, " 55") || !strings.Contains(out, " 12") || !strings.Contains(out, " 500") {
		t.Errorf("finite sibling gauges missing:\n%s", out)
	}
}

func statusRequest(srv *Server) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "/v1/status", nil)
	req.Header.Set("Authorization", "Bearer sekret")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

func TestHandleStatusSanitizesNonFinite(t *testing.T) {
	nan, inf := math.NaN(), math.Inf(-1)
	core, cpu := 42.0, 17.0
	sched := NewScheduler("v-test")
	tel := Telemetry{
		GPU:  &GPUBlock{Devices: []GPUInfo{{CorePercent: &nan, TemperatureC: &inf}, {Index: 1, CorePercent: &core}}},
		Host: &HostTelemetry{CPUPercent: &cpu, DiskFreeGB: math.NaN(), DiskTotalGB: 800},
	}
	sched.snap.Store(&tel)
	srv := &Server{Token: "sekret", Version: "v-test"}
	srv.SetScheduler(sched)

	rec := statusRequest(srv)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("content-type = %q", ct)
	}
	if !strings.HasSuffix(rec.Body.String(), "}\n") {
		t.Errorf("body missing trailing newline: %q", rec.Body.String())
	}
	var got Telemetry
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	d0, d1 := got.GPU.Devices[0], got.GPU.Devices[1]
	if d0.CorePercent != nil || d0.TemperatureC != nil {
		t.Errorf("non-finite readings not dropped: %+v", d0)
	}
	if d1.CorePercent == nil || *d1.CorePercent != 42 {
		t.Errorf("finite sibling device lost: %+v", d1)
	}
	if got.Host.CPUPercent == nil || *got.Host.CPUPercent != 17 || got.Host.DiskFreeGB != 0 || got.Host.DiskTotalGB != 800 {
		t.Errorf("host not sanitized correctly: %+v", got.Host)
	}
	// The scheduler's cached snapshot must be untouched by sanitizing.
	if tel.GPU.Devices[0].CorePercent == nil || !math.IsNaN(*tel.GPU.Devices[0].CorePercent) {
		t.Error("cached snapshot was mutated")
	}
}

func TestHandleStatusMarshalFailureIs500(t *testing.T) {
	orig := marshalStatus
	marshalStatus = func(any) ([]byte, error) { return nil, errors.New("boom") }
	defer func() { marshalStatus = orig }()
	sched := NewScheduler("v-test")
	sched.snap.Store(&Telemetry{})
	srv := &Server{Token: "sekret", Version: "v-test"}
	srv.SetScheduler(sched)
	if rec := statusRequest(srv); rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
}
