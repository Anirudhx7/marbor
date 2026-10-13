package main

import (
	"math"
	"reflect"
	"strings"
	"testing"
)

func TestBuildWorkloadDeterministicAndTagged(t *testing.T) {
	spec := defaultWorkloads(4, 3, 50, 20, 5, 5)["conv"]
	a := buildWorkload(spec, 42, "[ref run1]")
	b := buildWorkload(spec, 42, "[ref run1]")
	if !reflect.DeepEqual(a, b) {
		t.Fatal("same seed and tag must give identical workloads")
	}
	c := buildWorkload(spec, 42, "[ref run2]")
	if reflect.DeepEqual(a, c) {
		t.Fatal("different run tag must change the content")
	}
	if len(a) != 4 || len(a[0].Turns) != 3 {
		t.Fatalf("shape: %d convs, %d turns", len(a), len(a[0].Turns))
	}
	// Each later turn extends the earlier history exactly (prefix property).
	for _, cp := range a {
		for i := 1; i < len(cp.Turns); i++ {
			if !reflect.DeepEqual(cp.Turns[i][:len(cp.Turns[i-1])], cp.Turns[i-1]) {
				t.Fatalf("conv %d turn %d does not extend the previous turn", cp.ID, i+1)
			}
		}
	}
}

func TestSharedWorkloadCollapsesRouterKey(t *testing.T) {
	spec := defaultWorkloads(6, 2, 100, 400, 5, 5)["shared"]
	plans := buildWorkload(spec, 7, "[ref run1]")
	first := headKey(plans[0].Turns[0])
	for _, cp := range plans[1:] {
		if headKey(cp.Turns[0]) != first {
			t.Fatal("shared system prompt should give every conversation the same 200-char head key")
		}
	}
	conv := buildWorkload(defaultWorkloads(6, 2, 100, 400, 5, 5)["conv"], 7, "[ref run1]")
	if headKey(conv[0].Turns[0]) == headKey(conv[1].Turns[0]) {
		t.Fatal("conv workload must give distinct head keys")
	}
}

func TestPickEngineStableAndSpread(t *testing.T) {
	engs := []engineSpec{{"a", "http://a"}, {"b", "http://b"}, {"c", "http://c"}}
	counts := map[string]int{}
	for i := 0; i < 300; i++ {
		k := strings.Repeat("x", i%7) + string(rune('A'+i%26)) + strings.Repeat("y", i)
		e1, e2 := pickEngine(k, engs), pickEngine(k, engs)
		if e1 != e2 {
			t.Fatal("pickEngine must be deterministic")
		}
		counts[e1.Name]++
	}
	for _, e := range engs {
		if counts[e.Name] < 50 {
			t.Fatalf("engine %s got only %d of 300 keys: %v", e.Name, counts[e.Name], counts)
		}
	}
}

func TestParsePromAndSumMetric(t *testing.T) {
	raw := []byte("# HELP x\nvllm:prefix_cache_queries_total{model=\"m\"} 100\nvllm:prefix_cache_queries_total{model=\"n\"} 20\nvllm:prefix_cache_hits{model=\"m\"} 30 1700000000\nother 5\nbad line\n")
	s := parseProm(raw)
	if q, ok := sumMetric(s, "vllm:prefix_cache_queries"); !ok || q != 120 {
		t.Fatalf("queries = %v %v", q, ok)
	}
	if h, ok := sumMetric(s, "vllm:prefix_cache_hits"); !ok || h != 30 {
		t.Fatalf("hits = %v %v", h, ok)
	}
	if _, ok := sumMetric(s, "missing"); ok {
		t.Fatal("missing metric must report not found")
	}
}

func TestPercentileAndSignTest(t *testing.T) {
	if got := percentile([]float64{1, 2, 3, 4, 5}, 0.5); got != 3 {
		t.Fatalf("median = %v", got)
	}
	if !math.IsNaN(percentile(nil, 0.5)) {
		t.Fatal("empty sample must be NaN")
	}
	if p := signTestP(10, 0); p > 0.003 {
		t.Fatalf("10-0 split p = %v", p)
	}
	if p := signTestP(5, 5); p != 1 {
		t.Fatalf("5-5 split p = %v", p)
	}
	if p := signTestP(0, 0); p != 1 {
		t.Fatalf("no pairs p = %v", p)
	}
}

func syntheticSummaries(reps int, baseTTFT, armTTFT float64) []runSummary {
	var out []runSummary
	idx := 1
	for rep := 1; rep <= reps; rep++ {
		for _, a := range []struct {
			name string
			v    float64
		}{{"base", baseTTFT}, {"arm", armTTFT}} {
			out = append(out, runSummary{
				RunIndex: idx, Rep: rep, Workload: "conv", Arm: a.name,
				TTFTp50: a.v + float64(rep), TTFTp95: a.v * 2, TTFTp99: a.v * 3, ITLMean: 8,
				Throughput: 10, EngineCached: 0.5, MinNodeShare: 0.4, Flags: []string{},
			})
			idx++
		}
	}
	return out
}

func TestComparePairedDirection(t *testing.T) {
	sums := syntheticSummaries(10, 200, 100)
	ps := comparePaired(sums, "base", 1)
	p, ok := findPaired(ps, "conv", "arm", "ttft_p50_ms")
	if !ok || p.Pairs != 10 {
		t.Fatalf("pairs: %+v %v", p, ok)
	}
	if p.Improved != 10 || p.Worsened != 0 || p.CIHigh >= 0 || improvement(p) < 0.4 {
		t.Fatalf("expected clear improvement: %+v", p)
	}
}

func TestEvaluateVerdicts(t *testing.T) {
	c := &criteria{Name: "t", Workload: "conv", Arm: "arm", Baseline: "base", Metric: "ttft_p50_ms",
		MinRelImprovement: 0.1, MinRepsImproved: 8, RequireCIExcludesZero: true, MinPairs: 10}

	sums := syntheticSummaries(10, 200, 100)
	if v := evaluate(c, sums, comparePaired(sums, "base", 1)); v.Outcome != "PASS" {
		t.Fatalf("clear win should pass: %+v", v)
	}
	null := syntheticSummaries(10, 200, 200)
	if v := evaluate(c, null, comparePaired(null, "base", 1)); v.Outcome != "FAIL" {
		t.Fatalf("no effect must fail: %+v", v)
	}
	few := syntheticSummaries(4, 200, 100)
	if v := evaluate(c, few, comparePaired(few, "base", 1)); v.Outcome != "INVALID" {
		t.Fatalf("too few pairs must be invalid: %+v", v)
	}
	flagged := syntheticSummaries(10, 200, 100)
	flagged[1].Flags = []string{flagCollapsed}
	if v := evaluate(c, flagged, comparePaired(flagged, "base", 1)); v.Outcome != "INVALID" {
		t.Fatalf("a gate firing must invalidate: %+v", v)
	}
	c.NoRegression = []regressionLimit{{Workload: "conv", Arm: "arm", Metric: "throughput_rps", MaxWorse: 0.05}}
	if v := evaluate(c, sums, comparePaired(sums, "base", 1)); v.Outcome != "PASS" {
		t.Fatalf("no-regression within limit should pass: %+v", v)
	}
}

func TestSummarizeRunFlagsCollapsedPlacement(t *testing.T) {
	var recs []reqRecord
	for i := 0; i < 20; i++ {
		recs = append(recs, reqRecord{Conv: i, Turn: 1, Status: 200, TTFTMs: 50, TotalMs: 80, MeanITLMs: 8, Node: "a", NodeSource: "explain", PromptTokens: 100, CachedTokens: 0})
	}
	m := runMeta{Plan: runPlan{Index: 1, Rep: 1}, DurationMs: 1000, ExplainOK: 20, ExplainReq: 20,
		Deltas: []engineDelta{{Name: "a", Valid: true, PromptTokens: 2000}, {Name: "b", Valid: true}}}
	s := summarizeRun(m, recs)
	if !contains(s.Flags, flagCollapsed) {
		t.Fatalf("one-node placement must flag collapse, got %v", s.Flags)
	}
	if contains(s.Flags, flagNoEngineSignal) || contains(s.Flags, flagHighErrors) {
		t.Fatalf("unexpected flags %v", s.Flags)
	}
}

func contains(l []string, s string) bool {
	for _, x := range l {
		if x == s {
			return true
		}
	}
	return false
}

func TestBuildScheduleBalancedAndDeterministic(t *testing.T) {
	e := &experiment{Seed: 3, Reps: 10, Workloads: []workloadSpec{{Name: "conv"}, {Name: "unique"}},
		Arms: []armSpec{{Name: "a"}, {Name: "b"}, {Name: "c"}}}
	p1, p2 := buildSchedule(e), buildSchedule(e)
	if !reflect.DeepEqual(p1, p2) {
		t.Fatal("schedule must depend only on the seed")
	}
	if len(p1) != 60 {
		t.Fatalf("runs = %d", len(p1))
	}
	cell := map[[2]string]int{}
	for _, p := range p1 {
		cell[[2]string{p.Workload.Name, p.Arm.Name}]++
	}
	for k, n := range cell {
		if n != 10 {
			t.Fatalf("cell %v has %d reps", k, n)
		}
	}
}

func TestValidateRequiresMinRepsUnlessSmoke(t *testing.T) {
	base := func() *experiment {
		return &experiment{Engines: []engineSpec{{"a", "u"}, {"b", "v"}}, Model: "m", OutDir: "o", Reps: 3, Concurrency: 1, EngineKind: "sim",
			Arms: []armSpec{{Name: "x", Kind: "router"}}, DefaultBin: "bin"}
	}
	if err := base().validate(); err == nil {
		t.Fatal("3 reps without --smoke must be rejected")
	}
	e := base()
	e.Smoke = true
	if err := e.validate(); err != nil {
		t.Fatalf("smoke should allow 3 reps: %v", err)
	}
}
