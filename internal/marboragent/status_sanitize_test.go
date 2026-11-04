package marboragent

import (
	"math"
	"reflect"
	"strings"
	"testing"
)

type sanitizeInner struct {
	V float64
	P *float64
}

type sanitizeSample struct {
	F32    float32
	NilPtr *float64
	NilSl  []float64
	Nested []sanitizeInner
	Iface  any
	hidden *float64
}

func TestScrubFloatsCases(t *testing.T) {
	nan := math.NaN()
	inf := math.Inf(1)
	shared := nan
	in := sanitizeSample{
		F32:    float32(inf),
		Nested: []sanitizeInner{{V: nan, P: &nan}, {V: 2, P: &inf}},
		Iface:  sanitizeInner{V: inf, P: &nan},
		hidden: &shared,
	}
	out := withoutNonFinite(in)
	if out.F32 != 0 || out.NilPtr != nil || out.NilSl != nil {
		t.Errorf("scalar/nil cases wrong: %+v", out)
	}
	if out.Nested[0].V != 0 || out.Nested[0].P != nil || out.Nested[1].V != 2 || out.Nested[1].P != nil {
		t.Errorf("nested slice not scrubbed: %+v", out.Nested)
	}
	if got, ok := out.Iface.(sanitizeInner); !ok || got.V != 0 || got.P != nil {
		t.Errorf("interface value not scrubbed: %#v", out.Iface)
	}
	if out.hidden != &shared {
		t.Error("unexported field should be carried over untouched")
	}
	if !math.IsNaN(in.Nested[0].V) || in.Nested[0].P == nil {
		t.Error("input was mutated")
	}
}

// TestTelemetryShapeGuard fails when Telemetry grows a field kind the
// sanitizer's guarantees were reviewed against, so a new field forces a
// revisit of the non-finite handling.
func TestTelemetryShapeGuard(t *testing.T) {
	seen := map[reflect.Type]bool{}
	var walk func(path string, ty reflect.Type)
	walk = func(path string, ty reflect.Type) {
		switch ty.Kind() {
		case reflect.Interface, reflect.Map, reflect.Array:
			t.Errorf("%s: %s in Telemetry needs review of withoutNonFinite", path, ty.Kind())
		case reflect.Float32, reflect.Float64:
			t.Errorf("%s: non-pointer float needs review without omitempty", path)
		case reflect.Ptr:
			// *float64 is the supported shape: a non-finite value becomes nil.
			if k := ty.Elem().Kind(); k != reflect.Float32 && k != reflect.Float64 {
				walk(path, ty.Elem())
			}
		case reflect.Slice:
			if k := ty.Elem().Kind(); k == reflect.Float32 || k == reflect.Float64 {
				t.Errorf("%s: float slice needs review", path)
			}
			walk(path+"[]", ty.Elem())
		case reflect.Struct:
			if seen[ty] {
				return
			}
			seen[ty] = true
			for i := 0; i < ty.NumField(); i++ {
				f := ty.Field(i)
				if !f.IsExported() {
					continue
				}
				p := path + "." + f.Name
				// A plain float64 is fine only when omitempty maps the scrubbed 0 to unknown.
				if f.Type.Kind() == reflect.Float64 && strings.Contains(f.Tag.Get("json"), "omitempty") {
					continue
				}
				walk(p, f.Type)
			}
		}
	}
	walk("Telemetry", reflect.TypeOf(Telemetry{}))
}

func TestParseUptimeSeconds(t *testing.T) {
	for in, want := range map[string]int64{"0.00": 0, "12345.67": 12345} {
		if got, ok := parseUptimeSeconds(in); !ok || got != want {
			t.Errorf("parseUptimeSeconds(%q) = %d,%v want %d,true", in, got, ok, want)
		}
	}
	for _, in := range []string{"-1", "NaN", "+Inf", "1e11", "1e300", "abc", ""} {
		if _, ok := parseUptimeSeconds(in); ok {
			t.Errorf("parseUptimeSeconds(%q) ok=true, want false", in)
		}
	}
}
