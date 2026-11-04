package marboragent

import (
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

var nonFiniteTokens = []string{"NaN", "+Inf", "-Inf", "Inf"}

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

func TestRenderPrometheusSkipsNonFinite(t *testing.T) {
	nan, inf := math.NaN(), math.Inf(1)
	tel := Telemetry{
		GPU:  &GPUBlock{Devices: []GPUInfo{{CorePercent: &nan, TemperatureC: &inf}}},
		Host: &HostTelemetry{CPUPercent: &nan, DiskFreeGB: inf},
	}
	out := RenderPrometheus(tel)
	for _, bad := range []string{"NaN", "Inf"} {
		if strings.Contains(out, bad) {
			t.Errorf("metrics output contains %s:\n%s", bad, out)
		}
	}
}

func TestHandleStatusNonFiniteStillValidJSON(t *testing.T) {
	nan := math.NaN()
	sched := NewScheduler("v-test")
	tel := Telemetry{GPU: &GPUBlock{Devices: []GPUInfo{{CorePercent: &nan}}}}
	sched.snap.Store(&tel)
	srv := &Server{Token: "sekret", Version: "v-test"}
	srv.SetScheduler(sched)

	req := httptest.NewRequest(http.MethodGet, "/v1/status", nil)
	req.Header.Set("Authorization", "Bearer sekret")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code == http.StatusOK {
		var got Telemetry
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatalf("200 with invalid JSON: %v", err)
		}
		return
	}
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 200 or 500", rec.Code)
	}
}
