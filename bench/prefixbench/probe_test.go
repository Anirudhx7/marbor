package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeEngine is a minimal streaming chat engine with a prefix cache keyed on
// the first message. A request whose first message was seen before reports
// 95% of its prompt as cached and answers faster; a new one is "uncached".
// capacity bounds how many residency-cell first messages stay resident (oldest
// evicted); other prompts are never evicted so background load is harmless.
type fakeEngine struct {
	mu       sync.Mutex
	seen     []string
	resident []string // residency prompts in arrival order, bounded by capacity
	firsts   []string // every first message received, in order
	capacity int
	hits     int
	prompt   float64
	cached   float64
	requests float64
	fail     bool          // answer every chat request with HTTP 500
	slow     time.Duration // extra delay before answering
	noUsage  bool          // omit the usage chunk
}

func (f *fakeEngine) lookup(first string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.firsts = append(f.firsts, first)
	for _, s := range f.seen {
		if s == first {
			f.hits++
			return true
		}
	}
	f.seen = append(f.seen, first)
	if f.capacity > 0 && strings.Contains(first, "residency-") {
		f.resident = append(f.resident, first)
		if len(f.resident) > f.capacity {
			evict := f.resident[0]
			f.resident = f.resident[1:]
			for i, k := range f.seen {
				if k == evict {
					f.seen = append(f.seen[:i], f.seen[i+1:]...)
					break
				}
			}
		}
	}
	return false
}

func (f *fakeEngine) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		if f.fail {
			http.Error(w, "engine down", http.StatusInternalServerError)
			return
		}
		var req struct {
			Messages []chatMessage `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		hit := f.lookup(req.Messages[0].Content)
		tokens := 0
		for _, m := range req.Messages {
			tokens += len(strings.Fields(m.Content))
		}
		cached := 0
		delay := 60 * time.Millisecond
		if hit {
			cached, delay = tokens*19/20, 5*time.Millisecond
		}
		f.mu.Lock()
		f.prompt += float64(tokens)
		f.cached += float64(cached)
		f.requests++
		f.mu.Unlock()
		select {
		case <-time.After(delay + f.slow):
		case <-r.Context().Done():
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\n")
		fl.Flush()
		if !f.noUsage {
			fmt.Fprintf(w, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":%d,\"prompt_tokens_details\":{\"cached_tokens\":%d}}}\n\n", tokens, cached)
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	})
	mux.HandleFunc("/sim/stats", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]float64{"requests": f.requests, "prompt_tokens": f.prompt, "cached_tokens": f.cached})
	})
	return mux
}

// probeArgs builds a small, fast probe command line for an engine spec such as "e=http://host".
func probeArgs(engine, out string, extra ...string) []string {
	return append([]string{"--engine", engine, "--engine-kind", "sim", "--model", "m", "--out", out,
		"--context-words", "40", "--turn-words", "5", "--answer-words", "5", "--max-tokens", "4", "--nonce", "t"}, extra...)
}

func mustParse(t *testing.T, url string, extra ...string) *probeConfig {
	t.Helper()
	cfg, err := parseProbeFlags(probeArgs("e="+url, t.TempDir(), extra...))
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestProbeFlagsRequireExactlyOneEngine(t *testing.T) {
	if _, err := parseProbeFlags(probeArgs("e=http://a", t.TempDir())); err != nil {
		t.Fatalf("one engine must be accepted: %v", err)
	}
	if _, err := parseProbeFlags(probeArgs("a=http://a,b=http://b", t.TempDir())); err == nil || !strings.Contains(err.Error(), "exactly one engine") {
		t.Fatalf("two engines must be rejected by probe, got %v", err)
	}
	if _, err := parseProbeFlags([]string{"--model", "m", "--out", t.TempDir()}); err == nil {
		t.Fatal("missing engine must be rejected")
	}
}

// The comparison modes keep their two-engine floor.
func TestComparisonModesStillNeedTwoEngines(t *testing.T) {
	_, err := parseRunFlags([]string{"--engines", "a=http://a", "--model", "m", "--out", t.TempDir(), "--router-bin", "x", "--smoke", "--reps", "1"})
	if err == nil || !strings.Contains(err.Error(), "at least 2 engines") {
		t.Fatalf("run with one engine must fail with the 2-engine rule, got %v", err)
	}
}

func TestProbeFlagsRejectBadInput(t *testing.T) {
	for name, extra := range map[string][]string{
		"unknown step":          {"--steps", "idle,bogus"},
		"duplicate step":        {"--steps", "idle,idle"},
		"bad level":             {"--levels", "12,x"},
		"duplicate level":       {"--levels", "12,12"},
		"duplicate residency":   {"--residency", "8,8"},
		"zero residency":        {"--residency", "0"},
		"load without levels":   {"--steps", "load", "--levels", ""},
		"residency without Ns":  {"--steps", "residency", "--residency", ""},
		"one turn":              {"--turns", "1"},
		"no probes":             {"--probes", "0"},
		"zero context words":    {"--context-words", "0"},
		"zero max tokens":       {"--max-tokens", "0"},
		"zero max model length": {"--max-model-len", "0"},
		"zero timeout":          {"--timeout", "0s"},
	} {
		if _, err := parseProbeFlags(probeArgs("e=http://a", t.TempDir(), extra...)); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

func TestSimOrSmokeIsMarkedNotAMeasurement(t *testing.T) {
	c, err := parseProbeFlags(probeArgs("e=http://a", t.TempDir()))
	if err != nil || !c.NotMeasure {
		t.Fatalf("sim engine must be marked not a measurement: %v %v", c, err)
	}
	c, err = parseProbeFlags(probeArgs("e=http://a", t.TempDir(), "--engine-kind", "vllm", "--smoke"))
	if err != nil || !c.NotMeasure {
		t.Fatalf("smoke must be marked not a measurement: %v %v", c, err)
	}
	c, err = parseProbeFlags(probeArgs("e=http://a", t.TempDir(), "--engine-kind", "vllm"))
	if err != nil || c.NotMeasure {
		t.Fatalf("a real engine run must not be marked: %v %v", c, err)
	}
}

func rec(role string, conv int, ttft float64, cached int) probeRec {
	return probeRec{reqRecord: reqRecord{Conv: conv, Status: 200, TTFTMs: ttft, PromptTokens: 100, CachedTokens: cached}, Role: role}
}

func bad(role string, conv int) probeRec {
	return probeRec{reqRecord: reqRecord{Conv: conv, Status: 500, Error: "boom", TTFTMs: -1}, Role: role}
}

func TestSummarizePairsGap(t *testing.T) {
	cell := summarizePairs("idle", 0, []probeRec{rec("u", 0, 100, 0), rec("c", 0, 40, 90), rec("u", 1, 200, 0), rec("c", 1, 60, 90)})
	if cell.Pairs != 2 || cell.Errors != 0 {
		t.Fatalf("pairs %d errors %d", cell.Pairs, cell.Errors)
	}
	if *cell.TTFTUmean != 150 || *cell.TTFTCmean != 50 {
		t.Fatalf("means %v %v", *cell.TTFTUmean, *cell.TTFTCmean)
	}
	if want := (150.0 - 50.0) / 150.0; *cell.G != want {
		t.Fatalf("g %v want %v", *cell.G, want)
	}
	if *cell.CachedFrac != 0.9 {
		t.Fatalf("cached frac %v", *cell.CachedFrac)
	}
}

// A conversation whose uncached turn failed must not contribute its second
// turn: that request would no longer be a cached one.
func TestSummarizePairsDropsUnmatchedPairs(t *testing.T) {
	cell := summarizePairs("idle", 0, []probeRec{
		bad("u", 0), rec("c", 0, 5, 90),
		rec("u", 1, 100, 0), rec("c", 1, 20, 90),
		rec("u", 2, 100, 0), bad("c", 2),
	})
	if cell.Pairs != 1 || cell.Errors != 2 {
		t.Fatalf("pairs %d errors %d", cell.Pairs, cell.Errors)
	}
	if *cell.TTFTUmean != 100 || *cell.TTFTCmean != 20 {
		t.Fatalf("only the complete pair may be scored: %v %v", *cell.TTFTUmean, *cell.TTFTCmean)
	}
}

func TestSummarizePairsNoDataIsNullNotZero(t *testing.T) {
	cell := summarizePairs("idle", 0, []probeRec{bad("u", 0), bad("c", 0)})
	if cell.Pairs != 0 || cell.Errors != 2 {
		t.Fatalf("pairs %d errors %d", cell.Pairs, cell.Errors)
	}
	for name, p := range map[string]*float64{"u50": cell.TTFTUp50, "c50": cell.TTFTCp50, "umean": cell.TTFTUmean, "cmean": cell.TTFTCmean, "g": cell.G, "g50": cell.GP50, "cached": cell.CachedFrac} {
		if p != nil {
			t.Errorf("%s must be nil with no data, got %v", name, *p)
		}
	}
	b, err := json.Marshal(cell)
	if err != nil {
		t.Fatalf("a no-data cell must marshal: %v", err)
	}
	if !strings.Contains(string(b), `"g":null`) {
		t.Fatalf("g must be null: %s", b)
	}
}

func TestSummarizePairsNoCachedEvidenceIsDash(t *testing.T) {
	c := rec("c", 0, 10, -1)
	c.PromptTokens = -1
	cell := summarizePairs("idle", 0, []probeRec{rec("u", 0, 30, -1), c})
	if cell.CachedFrac != nil || val(cell.CachedFrac, "%.3f") != "-" {
		t.Fatalf("missing usage must render as dash, got %v", cell.CachedFrac)
	}
	if cell.G == nil {
		t.Fatal("g needs only TTFT, not usage")
	}
}

func TestSummarizeResidency(t *testing.T) {
	fills := map[int]probeRec{0: rec("fill", 0, 10, 0), 1: rec("fill", 1, 10, 0), 2: rec("fill", 2, 10, 0)}
	for k, f := range fills {
		f.PromptTokens = 93
		fills[k] = f
	}
	cell := summarizeResidency(3, fills, []probeRec{
		rec("revisit", 0, 5, 95),  // hit
		rec("revisit", 1, 50, 10), // miss
		bad("revisit", 2),         // error, not a miss
		{reqRecord: reqRecord{Conv: 3, Status: 200, TTFTMs: 5, PromptTokens: -1, CachedTokens: -1}, Role: "revisit"}, // no usage: unscored
	})
	if cell.Revisits != 2 || cell.Errors != 1 {
		t.Fatalf("revisits %d errors %d", cell.Revisits, cell.Errors)
	}
	if *cell.ShareCached != 0.5 {
		t.Fatalf("share %v", *cell.ShareCached)
	}
	if *cell.Ceiling != 0.93 {
		t.Fatalf("ceiling %v", *cell.Ceiling)
	}
	empty := summarizeResidency(3, nil, nil)
	if empty.ShareCached != nil || empty.Ceiling != nil {
		t.Fatal("no revisits must give null share and ceiling, not zero")
	}
}

func TestResidentK(t *testing.T) {
	cell := func(n int, fr float64) residencyCell { return residencyCell{N: n, EngineCachedFr: fp(fr)} }
	cases := []struct {
		name  string
		cells []residencyCell
		want  string
	}{
		{"all pass", []residencyCell{cell(8, 1), cell(16, 0.95)}, ">16"},
		{"decay", []residencyCell{cell(8, 1), cell(16, 0.92), cell(32, 0.4), cell(64, 0.1)}, "16"},
		{"none pass", []residencyCell{cell(8, 0.5), cell(16, 0.1)}, "<8"},
		{"no engine counters", []residencyCell{{N: 8}}, "-"},
		{"unsorted input", []residencyCell{cell(32, 0.2), cell(8, 1), cell(16, 0.95)}, "16"},
		{"dip then pass stops at the first failure", []residencyCell{cell(8, 1), cell(16, 0.5), cell(32, 0.95)}, "8"},
		{"missing counters on the top level", []residencyCell{cell(8, 1), cell(16, 1), {N: 32}}, "-"},
		{"missing counters on a middle level", []residencyCell{cell(8, 1), {N: 16}, cell(32, 1)}, "-"},
		{"empty", nil, "-"},
	}
	for _, c := range cases {
		if got := residentK(c.cells); got != c.want {
			t.Errorf("%s: got %q want %q", c.name, got, c.want)
		}
	}
}

func TestMonotonicWarnings(t *testing.T) {
	cell := func(n int, fr float64) residencyCell { return residencyCell{N: n, EngineCachedFr: fp(fr)} }
	if w := monotonicWarnings([]residencyCell{cell(8, 1), cell(16, 0.9), cell(32, 0.2)}); len(w) != 0 {
		t.Fatalf("clean decay must not warn: %v", w)
	}
	w := monotonicWarnings([]residencyCell{cell(32, 0.95), cell(8, 1), cell(16, 0.3)})
	if len(w) != 1 || !strings.Contains(w[0], "N=32 passed after N=16 failed") {
		t.Fatalf("dip then pass must warn: %v", w)
	}
}

func TestCeilingWarning(t *testing.T) {
	w := ceilingWarnings([]residencyCell{{N: 8, Ceiling: fp(0.5)}, {N: 16, Ceiling: fp(0.95)}, {N: 32}})
	if len(w) != 1 || !strings.Contains(w[0], "N=8") {
		t.Fatalf("warnings %v", w)
	}
}

func TestProbeEndToEndAgainstFakeEngine(t *testing.T) {
	fe := &fakeEngine{capacity: 12}
	srv := httptest.NewServer(fe.handler())
	defer srv.Close()
	cfg := mustParse(t, srv.URL, "--probes", "4", "--levels", "2", "--residency", "4,8,24")
	sum, recs, err := runProbe(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !sum.NotAMeasurement || sum.Interrupted {
		t.Fatalf("flags: %+v", sum)
	}
	z := sum.Size
	if z == nil || z.PromptTokensTurn1 == nil || z.PromptTokensTurnLast == nil || *z.PromptTokensTurnLast <= *z.PromptTokensTurn1 || z.FitsModelLen == nil || !*z.FitsModelLen {
		t.Fatalf("size: %+v", z)
	}
	if len(sum.Cells) != 2 || sum.Cells[0].Step != "idle" || sum.Cells[1].Step != "load" || sum.Cells[1].Load != 2 {
		t.Fatalf("cells: %+v", sum.Cells)
	}
	idle := sum.Cells[0]
	if idle.Pairs != 4 || idle.Errors != 0 || idle.G == nil || *idle.G < 0.3 {
		t.Fatalf("cached must be clearly faster than uncached on the fake engine: %+v", idle)
	}
	if idle.CachedFrac == nil || *idle.CachedFrac < 0.9 {
		t.Fatalf("cached evidence missing: %+v", idle)
	}
	// Capacity 12 on the fake: N=4 and N=8 stay resident, N=24 decays.
	if len(sum.Residency) != 3 {
		t.Fatalf("residency: %+v", sum.Residency)
	}
	if *sum.Residency[0].ShareCached != 1 || *sum.Residency[1].ShareCached != 1 {
		t.Fatalf("small N must stay resident: %+v", sum.Residency)
	}
	if *sum.Residency[2].ShareCached >= 0.5 {
		t.Fatalf("N=24 must decay on a 12-slot fake: %+v", sum.Residency[2])
	}
	if sum.K != "8" {
		t.Fatalf("k %q want 8", sum.K)
	}
	if len(recs) == 0 {
		t.Fatal("no request records")
	}
	dir := t.TempDir()
	if err := writeProbe(dir, sum, recs); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"probe.json", "probe.md", "requests.jsonl"} {
		if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
			t.Errorf("missing output %s", f)
		}
	}
	md := renderProbe(sum)
	for _, want := range []string{"NOT A MEASUREMENT", "ttft_u_p50_ms", "engine_cached_frac", "k (largest N"} {
		if !strings.Contains(md, want) {
			t.Errorf("rendered probe lacks %q", want)
		}
	}
}

// Every first message in a cell must differ from the others within the first
// few tokens, and nothing may repeat across cells or invocations except the
// deliberate turn-2 revisit.
func TestPromptsAreUniqueAcrossConversationsCellsAndInvocations(t *testing.T) {
	fe := &fakeEngine{}
	srv := httptest.NewServer(fe.handler())
	defer srv.Close()
	for _, nonce := range []string{"one", "two"} {
		cfg := mustParse(t, srv.URL, "--steps", "idle,residency", "--probes", "3", "--residency", "4,8", "--nonce", nonce)
		if _, _, err := runProbe(context.Background(), cfg); err != nil {
			t.Fatal(err)
		}
	}
	// Per invocation: 3 idle pairs + 4 and 8 residency pairs = 15 revisits that
	// legitimately hit. Any more hits means a cell or an invocation reused a prompt.
	if want := 3 + 4 + 8 + 3 + 4 + 8; fe.hits != want {
		t.Fatalf("hits %d want %d: a prompt was reused across cells or runs", fe.hits, want)
	}
	heads := map[string]bool{}
	for _, f := range fe.firsts {
		heads[f[:12]] = true
	}
	if len(heads) < 8 {
		t.Fatalf("first messages share their opening tokens: only %d distinct 12-char heads in %d requests", len(heads), len(fe.firsts))
	}
}

func TestProbeStepsSubset(t *testing.T) {
	fe := &fakeEngine{}
	srv := httptest.NewServer(fe.handler())
	defer srv.Close()
	sum, _, err := runProbe(context.Background(), mustParse(t, srv.URL, "--steps", "idle", "--probes", "3"))
	if err != nil || sum.Size != nil || len(sum.Residency) != 0 || len(sum.Cells) != 1 || sum.K != "" {
		t.Fatalf("steps subset ignored: %+v %v", sum, err)
	}
}

func TestProbeEngineDownWritesOutputWithNulls(t *testing.T) {
	defer func(d time.Duration) { loadWarmupTimeout = d }(loadWarmupTimeout)
	loadWarmupTimeout = 300 * time.Millisecond
	fe := &fakeEngine{fail: true}
	srv := httptest.NewServer(fe.handler())
	defer srv.Close()
	cfg := mustParse(t, srv.URL, "--probes", "3", "--levels", "2", "--residency", "4")
	sum, recs, err := runProbe(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if sum.Size == nil || sum.Size.Error == "" || sum.Size.FitsModelLen != nil {
		t.Fatalf("size must carry the error and no fabricated verdict: %+v", sum.Size)
	}
	for _, c := range sum.Cells {
		if c.Pairs != 0 || c.Errors == 0 || c.G != nil || c.TTFTUmean != nil {
			t.Fatalf("all-error cell must have no statistics: %+v", c)
		}
	}
	if sum.Cells[1].LoadErrors == 0 || sum.Cells[1].LoadFirstError == "" {
		t.Fatalf("load errors must be surfaced: %+v", sum.Cells[1])
	}
	if w := sum.Cells[1].LoadWarm; w == nil || *w {
		t.Fatalf("a dead engine must not count as warm: %v", w)
	}
	if len(sum.Warnings) == 0 || !strings.Contains(sum.Warnings[0], "steady state") {
		t.Fatalf("a cold load cell must carry a warning: %v", sum.Warnings)
	}
	if sum.Residency[0].FillErrors != 4 || sum.Residency[0].Revisits != 0 {
		t.Fatalf("failed fills must be errors, not decayed cache: %+v", sum.Residency[0])
	}
	if len(sum.GainLevels) != 0 {
		t.Fatalf("gain levels from no data: %v", sum.GainLevels)
	}
	if err := writeProbe(t.TempDir(), sum, recs); err != nil {
		t.Fatalf("output must be written when the engine is down: %v", err)
	}
}

func TestProbeEngineKindNoneHasNoEngineCounters(t *testing.T) {
	fe := &fakeEngine{}
	srv := httptest.NewServer(fe.handler())
	defer srv.Close()
	cfg := mustParse(t, srv.URL, "--engine-kind", "none", "--steps", "residency", "--residency", "4")
	sum, _, err := runProbe(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if sum.Residency[0].EngineCachedFr != nil || sum.K != "-" {
		t.Fatalf("no counters must give null hit fraction and k '-': %+v k=%s", sum.Residency[0], sum.K)
	}
	if *sum.Residency[0].ShareCached != 1 {
		t.Fatalf("per-request evidence must still be reported: %+v", sum.Residency[0])
	}
}

func TestProbeCancelStopsPromptlyAndMarksInterrupted(t *testing.T) {
	fe := &fakeEngine{slow: 30 * time.Second}
	srv := httptest.NewServer(fe.handler())
	defer srv.Close()
	cfg := mustParse(t, srv.URL, "--levels", "3", "--residency", "4,8")
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(300 * time.Millisecond); cancel() }()
	start := time.Now()
	sum, _, err := runProbe(ctx, cfg)
	if err == nil || !sum.Interrupted {
		t.Fatalf("cancel must be reported: err=%v interrupted=%v", err, sum.Interrupted)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatalf("cancel took %s; in-flight requests were not cancelled", time.Since(start))
	}
	for _, c := range sum.Cells {
		if c.Errors != 0 || c.LoadErrors != 0 {
			t.Fatalf("interrupted requests must not count as engine errors: %+v", c)
		}
	}
	for _, c := range sum.Residency {
		if c.Errors != 0 || c.FillErrors != 0 {
			t.Fatalf("interrupted requests must not count as engine errors: %+v", c)
		}
	}
	if len(sum.Warnings) != 0 {
		t.Fatalf("an interrupted load step must not warn about warm-up: %v", sum.Warnings)
	}
	if sum.K != "-" || len(sum.GainLevels) != 0 {
		t.Fatalf("an interrupted run must not state k or gain levels: k=%q gain=%v", sum.K, sum.GainLevels)
	}
	if !strings.Contains(renderProbe(sum), "INTERRUPTED") {
		t.Fatal("rendered output must say interrupted")
	}
}

func TestProbeWithoutUsageReportsDashes(t *testing.T) {
	fe := &fakeEngine{noUsage: true}
	srv := httptest.NewServer(fe.handler())
	defer srv.Close()
	sum, _, err := runProbe(context.Background(), mustParse(t, srv.URL, "--steps", "size,idle", "--probes", "2"))
	if err != nil {
		t.Fatal(err)
	}
	if sum.Size.PromptTokensTurn1 != nil || sum.Size.FitsModelLen != nil || sum.Cells[0].CachedFrac != nil {
		t.Fatalf("missing usage must stay null: size=%+v cell=%+v", sum.Size, sum.Cells[0])
	}
	if sum.Cells[0].G == nil {
		t.Fatal("TTFT gap needs no usage")
	}
}

func TestProbeRefusesToOverwriteAnExistingResult(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "probe.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := cmdProbe(probeArgs("e=http://127.0.0.1:1", dir))
	if err == nil || !strings.Contains(err.Error(), "already holds a probe result") {
		t.Fatalf("existing result must be refused, got %v", err)
	}
}

func TestMarkdownTextIsSanitized(t *testing.T) {
	cfg := &probeConfig{Engine: engineSpec{Name: "e", URL: "http://x"}, Model: "m|n", Label: "a\nb`c"}
	md := renderProbe(&probeSummary{Config: *cfg})
	if strings.Contains(md, "m|n") || strings.Contains(md, "a\nb") || strings.Contains(md, "`") {
		t.Fatalf("free text must not break markdown: %q", md)
	}
}

func TestSizeFitsIsInclusiveOfTheLimit(t *testing.T) {
	fe := &fakeEngine{}
	srv := httptest.NewServer(fe.handler())
	defer srv.Close()
	cfg := mustParse(t, srv.URL, "--steps", "size")
	sum, _, err := runProbe(context.Background(), cfg)
	if err != nil || sum.Size.PromptTokensTurnLast == nil {
		t.Fatalf("size: %+v %v", sum.Size, err)
	}
	last := *sum.Size.PromptTokensTurnLast
	cfg2 := mustParse(t, srv.URL, "--steps", "size", "--nonce", "t2", "--max-model-len", fmt.Sprint(last+cfg.MaxTokens))
	sum2, _, err := runProbe(context.Background(), cfg2)
	if err != nil || sum2.Size.FitsModelLen == nil || !*sum2.Size.FitsModelLen {
		t.Fatalf("prompt + max_tokens equal to the limit must fit: %+v %v", sum2.Size, err)
	}
}
