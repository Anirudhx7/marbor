package main

// probe.go - single-engine residency probe.
//
// The comparison modes (run, plan) need at least two engines because they
// compare routing arms. The probe measures ONE engine and starts no router:
// how much faster a request whose prefix is cached is than one whose prefix
// is not, whether that gap survives concurrent load, and how many distinct
// conversations the engine keeps resident before revisits stop hitting its
// prefix cache.
//
// Steps (select with --steps):
//
//	size       one multi-turn conversation: prompt tokens at turn 1 and at the last turn
//	idle       cached vs uncached TTFT, nothing else running (load 0)
//	load       the same probe while a closed loop of unique requests holds
//	           concurrency at each --levels value
//	residency  for each N in --residency: N distinct first turns, then revisit
//	           all N in the same order; hit fraction from the engine counters
//
// Output field names follow the JSON tags the other modes already write
// (ttft_ms, prompt_tokens, cached_tokens, engine_cached_frac), plus the probe
// summary fields on probeCell and residencyCell. A statistic that could not be
// observed is JSON null and renders as "-"; nothing is estimated.

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"math/rand"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// cachedShareLine is the fraction of a prompt that must be reported cached for
// a revisit to count as a hit on the per-request evidence.
const cachedShareLine = 0.5

// hitLine is the engine hit fraction at or above which a residency level
// still counts as resident.
const hitLine = 0.90

// gainLine is the relative TTFT gap (ttft_u - ttft_c) / ttft_u at or above
// which a load level is listed in the summary as having a worthwhile gap.
const gainLine = 0.10

// defaultMaxModelLen matches the usual --max-model-len used for a 7B probe;
// override with --max-model-len to match the engine under test.
const defaultMaxModelLen = 8192

// loadWarmupTimeout bounds the wait for every load worker to complete a
// successful request before probing starts. A variable so tests can shorten it.
var loadWarmupTimeout = 60 * time.Second

type probeConfig struct {
	Engine      engineSpec `json:"engine"`
	EngineKind  string     `json:"engine_kind"`
	Model       string     `json:"model"`
	Steps       []string   `json:"steps"`
	Probes      int        `json:"probes"`
	Levels      []int      `json:"levels"`
	Residency   []int      `json:"residency"`
	Turns       int        `json:"turns"`
	ContextWord int        `json:"context_words"`
	TurnWords   int        `json:"turn_words"`
	AnswerWords int        `json:"answer_words"`
	MaxTokens   int        `json:"max_tokens"`
	MaxModelLen int        `json:"max_model_len"`
	Seed        int64      `json:"seed"`
	Nonce       string     `json:"nonce"`
	Label       string     `json:"label,omitempty"`
	OutDir      string     `json:"out_dir"`
	TimeoutSec  int        `json:"timeout_sec"`
	Smoke       bool       `json:"smoke"`
	NotMeasure  bool       `json:"not_a_measurement"`
	timeout     time.Duration
}

// probeRec is one request of the probe: the raw record plus where it belongs.
type probeRec struct {
	reqRecord
	Step  string `json:"step"`
	Role  string `json:"role"`
	Level int    `json:"level"`
	N     int    `json:"n"`
}

// sizeResult is the prompt-size check. Pointer fields are null when the
// engine did not report a prompt size.
type sizeResult struct {
	PromptTokensTurn1    *int   `json:"prompt_tokens_turn1"`
	PromptTokensTurnLast *int   `json:"prompt_tokens_turn_last"`
	Turns                int    `json:"turns"`
	FitsModelLen         *bool  `json:"fits_model_len"`
	Error                string `json:"error,omitempty"`
}

// probeCell is cached versus uncached TTFT at one load level. Only complete
// pairs count: ttft_u is a conversation's first turn (prefix never seen),
// ttft_c the second turn on the same prefix, and a conversation whose either
// request failed is dropped. g is (ttft_u - ttft_c) / ttft_u on the mean and
// g_p50 the same on the median.
type probeCell struct {
	Step           string   `json:"step"`
	Load           int      `json:"load"`
	Pairs          int      `json:"n"`
	Errors         int      `json:"errors"`
	TTFTUp50       *float64 `json:"ttft_u_p50_ms"`
	TTFTCp50       *float64 `json:"ttft_c_p50_ms"`
	TTFTUmean      *float64 `json:"ttft_u_mean_ms"`
	TTFTCmean      *float64 `json:"ttft_c_mean_ms"`
	G              *float64 `json:"g"`
	GP50           *float64 `json:"g_p50"`
	CachedFrac     *float64 `json:"cached_tokens_over_prompt_tokens"`
	LoadRequests   int      `json:"load_requests,omitempty"`
	LoadErrors     int      `json:"load_errors,omitempty"`
	LoadFirstError string   `json:"load_first_error,omitempty"`
	LoadWarm       *bool    `json:"load_warm,omitempty"`
}

// residencyCell is one N of the residency sweep. engine_cached_frac is the
// engine's own hit fraction over the revisit phase only; null when the engine
// counters were unavailable. revisit_ceiling is the highest hit fraction a
// perfect cache could report for these prompts (first-turn tokens over
// revisit-turn tokens).
type residencyCell struct {
	N              int      `json:"n"`
	Revisits       int      `json:"revisits"` // scored revisits: successful and reporting usage
	FillErrors     int      `json:"fill_errors"`
	Errors         int      `json:"errors"`
	EngineCachedFr *float64 `json:"engine_cached_frac"`
	ShareCached    *float64 `json:"share_cached_ge_half"`
	Ceiling        *float64 `json:"revisit_ceiling"`
}

type probeSummary struct {
	NotAMeasurement bool            `json:"not_a_measurement"`
	Interrupted     bool            `json:"interrupted"`
	Config          probeConfig     `json:"config"`
	Size            *sizeResult     `json:"size,omitempty"`
	Cells           []probeCell     `json:"cells,omitempty"`
	Residency       []residencyCell `json:"residency,omitempty"`
	K               string          `json:"k,omitempty"`
	GainLine        float64         `json:"gain_line"`
	GainLevels      []int           `json:"load_levels_g_ge_gain_line,omitempty"`
	Warnings        []string        `json:"warnings,omitempty"`
	StartedAt       time.Time       `json:"started_at"`
	DurationSec     float64         `json:"duration_sec"`
}

var validProbeSteps = []string{"size", "idle", "load", "residency"}

func fp(v float64) *float64 { return &v }

func parseIntList(s string) ([]int, error) {
	var out []int
	seen := map[int]bool{}
	for _, p := range strings.Split(s, ",") {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		n, err := strconv.Atoi(p)
		if err != nil || n < 1 {
			return nil, fmt.Errorf("%q: want positive integers", p)
		}
		if seen[n] {
			return nil, fmt.Errorf("%d listed twice (a repeated value would reuse its own cached prefixes)", n)
		}
		seen[n] = true
		out = append(out, n)
	}
	return out, nil
}

func parseProbeFlags(args []string) (*probeConfig, error) {
	fs := flag.NewFlagSet("probe", flag.ContinueOnError)
	engine := fs.String("engine", "", "the one engine under test, as name=url (required)")
	kind := fs.String("engine-kind", "vllm", "engine probe kind: vllm, sim (mock with SIM_KV=1), or none")
	model := fs.String("model", "", "model id sent in requests (required)")
	steps := fs.String("steps", "size,idle,load,residency", "comma list from: size, idle, load, residency")
	probes := fs.Int("probes", 30, "probe conversations per cell")
	levels := fs.String("levels", "12,24,48", "load concurrencies for the load step")
	residency := fs.String("residency", "8,16,32,64,128,256", "N values for the residency step")
	turns := fs.Int("turns", 5, "turns in the size-check conversation")
	contextWords := fs.Int("context-words", 1500, "words in a conversation's first message")
	turnWords := fs.Int("turn-words", 40, "words in each later user message")
	answerWords := fs.Int("answer-words", 60, "words in each canned assistant message in the history")
	maxTokens := fs.Int("max-tokens", 64, "max_tokens per request")
	maxModelLen := fs.Int("max-model-len", defaultMaxModelLen, "the engine's context limit, used by the prompt-size check")
	seed := fs.Int64("seed", 1, "base random seed")
	nonce := fs.String("nonce", "", "text mixed into every prompt so a rerun never reuses cached prefixes (default: time based)")
	label := fs.String("label", "", "free-text label recorded in the output (for example the gpu-memory-utilization used)")
	out := fs.String("out", "", "output directory (required)")
	timeout := fs.Duration("timeout", 180*time.Second, "per-request timeout")
	smoke := fs.Bool("smoke", false, "mark the output as a harness check, never a result")
	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	engines, err := parseEngines(*engine)
	if err != nil {
		return nil, err
	}
	c := &probeConfig{
		EngineKind: *kind, Model: *model, Probes: *probes, Turns: *turns,
		ContextWord: *contextWords, TurnWords: *turnWords, AnswerWords: *answerWords,
		MaxTokens: *maxTokens, MaxModelLen: *maxModelLen, Seed: *seed, Nonce: *nonce, Label: *label,
		OutDir: *out, TimeoutSec: int((*timeout + time.Second - 1) / time.Second), Smoke: *smoke, timeout: *timeout,
	}
	if len(engines) != 1 {
		return nil, errors.New("probe measures exactly one engine (--engine name=url); the comparison modes need two")
	}
	c.Engine = engines[0]
	if c.Levels, err = parseIntList(*levels); err != nil {
		return nil, fmt.Errorf("--levels: %w", err)
	}
	if c.Residency, err = parseIntList(*residency); err != nil {
		return nil, fmt.Errorf("--residency: %w", err)
	}
	sort.Ints(c.Residency)
	seen := map[string]bool{}
	for _, s := range strings.Split(*steps, ",") {
		if s = strings.TrimSpace(s); s != "" {
			if seen[s] {
				return nil, fmt.Errorf("--steps: %q listed twice", s)
			}
			seen[s] = true
			c.Steps = append(c.Steps, s)
		}
	}
	if c.Nonce == "" {
		c.Nonce = strconv.FormatInt(time.Now().UnixNano(), 36)
	}
	if c.OutDir != "" {
		if c.OutDir, err = filepath.Abs(c.OutDir); err != nil {
			return nil, err
		}
	}
	c.NotMeasure = c.Smoke || c.EngineKind == "sim"
	return c, c.validate()
}

func (c *probeConfig) validate() error {
	positive := []struct {
		name string
		v    int
	}{
		{"--probes", c.Probes}, {"--context-words", c.ContextWord}, {"--turn-words", c.TurnWords},
		{"--answer-words", c.AnswerWords}, {"--max-tokens", c.MaxTokens}, {"--max-model-len", c.MaxModelLen},
		{"--timeout", int(c.timeout)},
	}
	for _, p := range positive {
		if p.v < 1 {
			return fmt.Errorf("%s must be positive", p.name)
		}
	}
	switch {
	case c.Model == "":
		return errors.New("--model is required")
	case c.OutDir == "":
		return errors.New("--out is required")
	case c.Turns < 2:
		return errors.New("--turns must be >= 2")
	case c.EngineKind != "vllm" && c.EngineKind != "sim" && c.EngineKind != "none":
		return fmt.Errorf("--engine-kind %q: want vllm, sim or none", c.EngineKind)
	case len(c.Steps) == 0:
		return errors.New("--steps is empty")
	case c.has("load") && len(c.Levels) == 0:
		return errors.New("--steps includes load but --levels is empty")
	case c.has("residency") && len(c.Residency) == 0:
		return errors.New("--steps includes residency but --residency is empty")
	}
	for _, s := range c.Steps {
		known := false
		for _, v := range validProbeSteps {
			known = known || s == v
		}
		if !known {
			return fmt.Errorf("unknown step %q (want %s)", s, strings.Join(validProbeSteps, ", "))
		}
	}
	return nil
}

func (c *probeConfig) has(step string) bool {
	for _, s := range c.Steps {
		if s == step {
			return true
		}
	}
	return false
}

// tag names one cell of one invocation, so no two cells and no two
// invocations share a prompt.
func (c *probeConfig) tag(cell string) string {
	return fmt.Sprintf("[probe %s %s]", c.Nonce, cell)
}

// spec builds the workload shape for n conversations of the given turns.
func (c *probeConfig) spec(n, turns int) workloadSpec {
	return workloadSpec{Name: "conv", Conversations: n, Turns: turns,
		ContextWords: c.ContextWord, TurnWords: c.TurnWords, AnswerWords: c.AnswerWords}
}

// plans builds the conversations of one cell. Each conversation's first
// message starts with a conversation-specific token, placed before the
// shared cell tag: prefix caches hash fixed-size blocks chained from the
// start of the prompt, so a shared head would let one conversation's first
// turn partly hit another's cached blocks and understate the uncached cost.
func (c *probeConfig) plans(n, turns int, seedKey int, kind, cell string) []convPlan {
	plans := buildWorkload(c.spec(n, turns), runSeed(c.Seed, seedKey, kind), c.tag(cell))
	for i := range plans {
		for t := range plans[i].Turns {
			first := &plans[i].Turns[t][0]
			first.Content = fmt.Sprintf("c%d %s", plans[i].ID, first.Content)
		}
	}
	return plans
}

type probeRunner struct {
	cfg *probeConfig
	hc  *http.Client
	pc  *http.Client
	tg  target
	mu  sync.Mutex
	rec []probeRec
}

// send issues one request. A request cut short by cancellation is returned
// with its error set but not recorded, so a partial run never counts the
// interrupted request as an engine failure.
func (r *probeRunner) send(ctx context.Context, step, role string, level, n, conv, turn int, msgs []chatMessage) probeRec {
	rr := sendChatCtx(ctx, r.hc, r.tg, r.cfg.Model, msgs, r.cfg.MaxTokens)
	rr.Conv, rr.Turn = conv, turn
	pr := probeRec{reqRecord: rr, Step: step, Role: role, Level: level, N: n}
	if ctx.Err() != nil {
		return pr
	}
	r.mu.Lock()
	r.rec = append(r.rec, pr)
	r.mu.Unlock()
	return pr
}

func (r *probeRunner) snapshot() engineSnapshot {
	return probeEngine(r.pc, r.cfg.EngineKind, r.cfg.Engine)
}

// stepSize sends the first and the last turn of one conversation and records
// their prompt token counts.
func (r *probeRunner) stepSize(ctx context.Context) *sizeResult {
	cp := r.cfg.plans(1, r.cfg.Turns, 0, "size", "size")[0]
	first := r.send(ctx, "size", "turn1", 0, 0, 0, 1, cp.Turns[0])
	last := r.send(ctx, "size", "turn_last", 0, 0, 0, r.cfg.Turns, cp.Turns[len(cp.Turns)-1])
	if ctx.Err() != nil {
		return nil // interrupted: no size result rather than a failed one
	}
	res := &sizeResult{Turns: r.cfg.Turns}
	for _, rec := range []probeRec{first, last} {
		if !good(rec) && res.Error == "" {
			res.Error = rec.Error
			if res.Error == "" {
				res.Error = fmt.Sprintf("HTTP %d", rec.Status)
			}
		}
	}
	if first.PromptTokens >= 0 {
		v := first.PromptTokens
		res.PromptTokensTurn1 = &v
	}
	if last.PromptTokens >= 0 {
		v := last.PromptTokens
		res.PromptTokensTurnLast = &v
		fits := v+r.cfg.MaxTokens <= r.cfg.MaxModelLen
		res.FitsModelLen = &fits
	}
	return res
}

// pairs runs the sequential cached-vs-uncached probe: per conversation, turn 1
// then turn 2 on the same prefix. A conversation whose turn 1 failed is not
// continued, because its turn 2 would no longer be a cached request.
func (r *probeRunner) pairs(ctx context.Context, step string, level int) probeCell {
	plans := r.cfg.plans(r.cfg.Probes, 2, level, step, fmt.Sprintf("%s-%d", step, level))
	var recs []probeRec
	for _, cp := range plans {
		if ctx.Err() != nil {
			break
		}
		u := r.send(ctx, step, "u", level, 0, cp.ID, 1, cp.Turns[0])
		if ctx.Err() != nil {
			break // an interrupted request is neither scored nor an error
		}
		recs = append(recs, u)
		if !good(u) {
			continue
		}
		c := r.send(ctx, step, "c", level, 0, cp.ID, 2, cp.Turns[1])
		if ctx.Err() != nil {
			break
		}
		recs = append(recs, c)
	}
	return summarizePairs(step, level, recs)
}

// stepLoad runs the probe while workers keep `level` unique single-turn
// requests in flight (so level+1 requests are in flight while a probe request
// is outstanding). Probing starts once every background worker has completed a
// successful request; if that does not happen within loadWarmupTimeout the
// cell is flagged and a warning is added to the summary.
func (r *probeRunner) stepLoad(ctx context.Context, level int) probeCell {
	lctx, stop := context.WithCancel(ctx)
	defer stop()
	var wg sync.WaitGroup
	var sent, failed, ready atomic.Int64
	var firstErr atomic.Value
	for w := 0; w < level; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			rng := rand.New(rand.NewSource(runSeed(r.cfg.Seed, level, fmt.Sprintf("load|%d", w))))
			warmed := false
			for i := 0; lctx.Err() == nil; i++ {
				msg := fmt.Sprintf("w%d-%d %s load: %s", w, i, r.cfg.tag(fmt.Sprintf("load-%d", level)), makeWords(rng, r.cfg.ContextWord))
				rr := sendChatCtx(lctx, r.hc, r.tg, r.cfg.Model, []chatMessage{{Role: "user", Content: msg}}, r.cfg.MaxTokens)
				if lctx.Err() != nil {
					return
				}
				sent.Add(1)
				if rr.Error == "" && !warmed {
					warmed = true
					ready.Add(1)
				}
				if rr.Error != "" {
					failed.Add(1)
					firstErr.CompareAndSwap(nil, rr.Error)
					select { // do not hammer an engine that fails fast
					case <-time.After(100 * time.Millisecond):
					case <-lctx.Done():
					}
				}
			}
		}(w)
	}
	deadline := time.Now().Add(loadWarmupTimeout)
	for ready.Load() < int64(level) && time.Now().Before(deadline) && ctx.Err() == nil {
		time.Sleep(50 * time.Millisecond)
	}
	warm := ready.Load() >= int64(level)
	cell := r.pairs(ctx, "load", level)
	cell.LoadWarm = &warm
	stop()
	wg.Wait()
	cell.LoadRequests, cell.LoadErrors = int(sent.Load()), int(failed.Load())
	if e, ok := firstErr.Load().(string); ok {
		cell.LoadFirstError = e
	}
	return cell
}

// stepResidency sends n distinct first turns, then revisits all n in the same
// order with turn 2, and reads the engine counters across the revisit phase.
// A conversation whose first turn failed is not revisited.
func (r *probeRunner) stepResidency(ctx context.Context, n int) residencyCell {
	plans := r.cfg.plans(n, 2, n, "residency", fmt.Sprintf("residency-%d", n))
	fills := map[int]probeRec{}
	fillErrors := 0
	for _, cp := range plans {
		if ctx.Err() != nil {
			break
		}
		rec := r.send(ctx, "residency", "fill", 0, n, cp.ID, 1, cp.Turns[0])
		if ctx.Err() != nil {
			break
		}
		if good(rec) {
			fills[cp.ID] = rec
		} else {
			fillErrors++
		}
	}
	before := r.snapshot()
	var revisits []probeRec
	for _, cp := range plans {
		if ctx.Err() != nil {
			break
		}
		if _, ok := fills[cp.ID]; !ok {
			continue
		}
		rec := r.send(ctx, "residency", "revisit", 0, n, cp.ID, 2, cp.Turns[1])
		if ctx.Err() != nil {
			break
		}
		revisits = append(revisits, rec)
	}
	cell := summarizeResidency(n, fills, revisits)
	cell.FillErrors = fillErrors
	if d := diffSnapshots([]engineSnapshot{before}, []engineSnapshot{r.snapshot()}); len(d) == 1 && d[0].Valid && d[0].PromptTokens > 0 {
		cell.EngineCachedFr = fp(d[0].CachedFrac)
	}
	return cell
}

func good(r probeRec) bool { return r.Error == "" && r.Status == http.StatusOK && r.TTFTMs >= 0 }

// nilIfEmpty returns the statistic of vals, or nil when there is nothing to
// compute it over (the helpers return NaN on empty input).
func nilIfEmpty(vals []float64, f func([]float64) float64) *float64 {
	if len(vals) == 0 {
		return nil
	}
	return fp(f(vals))
}

// gap is (uncached - cached) / uncached, nil when either side is missing.
func gap(uncached, cached *float64) *float64 {
	if uncached == nil || cached == nil || *uncached <= 0 {
		return nil
	}
	return fp((*uncached - *cached) / *uncached)
}

// summarizePairs turns the u and c records of one cell into a probeCell. Only
// conversations with both requests good are scored; every failed request is
// counted in errors.
func summarizePairs(step string, level int, recs []probeRec) probeCell {
	cell := probeCell{Step: step, Load: level}
	type pair struct{ u, c *probeRec }
	byConv := map[int]*pair{}
	for i := range recs {
		r := &recs[i]
		if !good(*r) {
			cell.Errors++
			continue
		}
		p := byConv[r.Conv]
		if p == nil {
			p = &pair{}
			byConv[r.Conv] = p
		}
		switch r.Role {
		case "u":
			p.u = r
		case "c":
			p.c = r
		}
	}
	var u, c []float64
	var cached, prompt float64
	for _, p := range byConv {
		if p.u == nil || p.c == nil {
			continue
		}
		u, c = append(u, p.u.TTFTMs), append(c, p.c.TTFTMs)
		if p.c.PromptTokens > 0 && p.c.CachedTokens >= 0 {
			cached += float64(p.c.CachedTokens)
			prompt += float64(p.c.PromptTokens)
		}
	}
	cell.Pairs = len(u)
	cell.TTFTUp50, cell.TTFTCp50 = nilIfEmpty(u, median), nilIfEmpty(c, median)
	cell.TTFTUmean, cell.TTFTCmean = nilIfEmpty(u, mean), nilIfEmpty(c, mean)
	cell.G, cell.GP50 = gap(cell.TTFTUmean, cell.TTFTCmean), gap(cell.TTFTUp50, cell.TTFTCp50)
	if prompt > 0 {
		cell.CachedFrac = fp(cached / prompt)
	}
	return cell
}

// summarizeResidency scores the revisits of one N against the per-request
// evidence. fills are the successful first turns, used for the ceiling.
func summarizeResidency(n int, fills map[int]probeRec, revisits []probeRec) residencyCell {
	cell := residencyCell{N: n}
	hit, scored := 0, 0
	var fillTok, revTok float64
	for _, r := range revisits {
		if !good(r) {
			cell.Errors++
			continue
		}
		if r.PromptTokens <= 0 || r.CachedTokens < 0 {
			continue
		}
		scored++
		if float64(r.CachedTokens) >= cachedShareLine*float64(r.PromptTokens) {
			hit++
		}
		if f, ok := fills[r.Conv]; ok && f.PromptTokens > 0 {
			fillTok += float64(f.PromptTokens)
			revTok += float64(r.PromptTokens)
		}
	}
	cell.Revisits = scored
	if scored > 0 {
		cell.ShareCached = fp(float64(hit) / float64(scored))
	}
	if revTok > 0 {
		cell.Ceiling = fp(fillTok / revTok)
	}
	return cell
}

// residentK is the last N before the first level whose engine hit fraction
// falls below hitLine, with levels taken in ascending order. Every level
// passing gives ">max" (max being the largest requested level), the first
// level failing gives "<min". If any requested level has no engine counters
// the answer is "-", because a missing level could hide the first failure.
func residentK(cells []residencyCell) string {
	if len(cells) == 0 {
		return "-"
	}
	sorted := append([]residencyCell(nil), cells...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].N < sorted[j].N })
	for _, c := range sorted {
		if c.EngineCachedFr == nil {
			return "-"
		}
	}
	for i, c := range sorted {
		if *c.EngineCachedFr < hitLine {
			if i == 0 {
				return "<" + strconv.Itoa(c.N)
			}
			return strconv.Itoa(sorted[i-1].N)
		}
	}
	return ">" + strconv.Itoa(sorted[len(sorted)-1].N)
}

// monotonicWarnings flags a level that passes after a lower level failed:
// residency should only get worse as N grows, so a later pass is noise or an
// engine effect, and k (which stops at the first failure) does not show it.
func monotonicWarnings(cells []residencyCell) []string {
	sorted := append([]residencyCell(nil), cells...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].N < sorted[j].N })
	var out []string
	failed := 0
	for _, c := range sorted {
		if c.EngineCachedFr == nil {
			continue
		}
		switch {
		case *c.EngineCachedFr < hitLine && failed == 0:
			failed = c.N
		case *c.EngineCachedFr >= hitLine && failed != 0:
			out = append(out, fmt.Sprintf("N=%d passed after N=%d failed: residency is not monotonic here, so k stops at the first failure and the later pass should not be read as capacity", c.N, failed))
		}
	}
	return out
}

// ceilingWarnings flags residency levels where even a perfect cache could not
// reach hitLine, so K would measure the prompt shape instead of residency.
func ceilingWarnings(cells []residencyCell) []string {
	var out []string
	for _, c := range cells {
		if c.Ceiling != nil && *c.Ceiling < hitLine {
			out = append(out, fmt.Sprintf("N=%d: a perfect cache could report at most %.2f (first-turn tokens over revisit-turn tokens), below the %.2f line, so k reflects the prompt shape; use a longer first message or shorter later turns", c.N, *c.Ceiling, hitLine))
		}
	}
	return out
}

func runProbe(ctx context.Context, cfg *probeConfig) (*probeSummary, []probeRec, error) {
	maxLevel := 0
	for _, l := range cfg.Levels {
		maxLevel = max(maxLevel, l)
	}
	tr := &http.Transport{MaxIdleConnsPerHost: maxLevel + 8}
	defer tr.CloseIdleConnections()
	r := &probeRunner{
		cfg: cfg,
		hc:  &http.Client{Timeout: cfg.timeout, Transport: tr},
		pc:  &http.Client{Timeout: 10 * time.Second},
		tg:  target{baseURL: cfg.Engine.URL},
	}
	sum := &probeSummary{NotAMeasurement: cfg.NotMeasure, Config: *cfg, GainLine: gainLine, StartedAt: time.Now()}
	if cfg.has("size") {
		sum.Size = r.stepSize(ctx)
	}
	if cfg.has("idle") && ctx.Err() == nil {
		sum.Cells = append(sum.Cells, r.pairs(ctx, "idle", 0))
	}
	if cfg.has("load") {
		for _, lv := range cfg.Levels {
			if ctx.Err() != nil {
				break
			}
			sum.Cells = append(sum.Cells, r.stepLoad(ctx, lv))
		}
	}
	if cfg.has("residency") {
		for _, n := range cfg.Residency {
			if ctx.Err() != nil {
				break
			}
			sum.Residency = append(sum.Residency, r.stepResidency(ctx, n))
		}
	}
	sum.Interrupted = ctx.Err() != nil
	for _, c := range sum.Cells {
		if c.LoadWarm != nil && !*c.LoadWarm && !sum.Interrupted {
			sum.Warnings = append(sum.Warnings, fmt.Sprintf("load %d: background load did not reach steady state within %s (not every worker completed a successful request); this cell may have been measured under less load than stated", c.Load, loadWarmupTimeout))
		}
	}
	if cfg.has("residency") {
		sum.K = "-"
		if !sum.Interrupted {
			sum.K = residentK(sum.Residency)
		}
		sum.Warnings = append(sum.Warnings, ceilingWarnings(sum.Residency)...)
		sum.Warnings = append(sum.Warnings, monotonicWarnings(sum.Residency)...)
	}
	if !sum.Interrupted {
		for _, c := range sum.Cells {
			if c.Step == "load" && c.G != nil && *c.G >= gainLine {
				sum.GainLevels = append(sum.GainLevels, c.Load)
			}
		}
	}
	sum.DurationSec = time.Since(sum.StartedAt).Seconds()
	return sum, r.rec, ctx.Err()
}

func cmdProbe(args []string) error {
	cfg, err := parseProbeFlags(args)
	if err != nil {
		return err
	}
	if _, err := os.Stat(filepath.Join(cfg.OutDir, "probe.json")); err == nil {
		return fmt.Errorf("%s already holds a probe result; choose a new --out", cfg.OutDir)
	}
	if err := os.MkdirAll(cfg.OutDir, 0o755); err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	sum, recs, runErr := runProbe(ctx, cfg)
	if err := writeProbe(cfg.OutDir, sum, recs); err != nil {
		return err
	}
	fmt.Print(renderProbe(sum))
	return runErr
}

func writeProbe(dir string, sum *probeSummary, recs []probeRec) error {
	if err := writeJSON(filepath.Join(dir, "probe.json"), sum); err != nil {
		return err
	}
	var buf []byte
	for _, r := range recs {
		b, err := json.Marshal(r)
		if err != nil {
			return err
		}
		buf = append(append(buf, b...), '\n')
	}
	if err := os.WriteFile(filepath.Join(dir, "requests.jsonl"), buf, 0o644); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "probe.md"), []byte(renderProbe(sum)), 0o644)
}

// val renders an optional statistic, "-" when it was not observed.
func val(v *float64, format string) string {
	if v == nil {
		return "-"
	}
	return fmt.Sprintf(format, *v)
}

func renderProbe(s *probeSummary) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# prefixbench probe\n\n")
	if s.NotAMeasurement {
		fmt.Fprintf(&b, "> NOT A MEASUREMENT. This probe used smoke mode or the simulated engine; it validates execution and output shape only and must not be quoted as a result.\n\n")
	}
	if s.Interrupted {
		fmt.Fprintf(&b, "> INTERRUPTED. The run was stopped early; later cells are missing and k is not computed.\n\n")
	}
	fmt.Fprintf(&b, "engine %s (%s), kind %s, model %s, nonce %s", mdText(s.Config.Engine.Name), mdText(s.Config.Engine.URL), s.Config.EngineKind, mdText(s.Config.Model), mdText(s.Config.Nonce))
	if s.Config.Label != "" {
		fmt.Fprintf(&b, ", label %s", mdText(s.Config.Label))
	}
	fmt.Fprintf(&b, ", %.0fs\n\n", s.DurationSec)
	if z := s.Size; z != nil {
		fmt.Fprintf(&b, "prompt_tokens turn 1: %s, turn %d: %s, fits max-model-len %d with max_tokens: %s",
			optInt(z.PromptTokensTurn1), z.Turns, optInt(z.PromptTokensTurnLast), s.Config.MaxModelLen, optBool(z.FitsModelLen))
		if z.Error != "" {
			fmt.Fprintf(&b, " (error: %s)", z.Error)
		}
		fmt.Fprint(&b, "\n\n")
	}
	if len(s.Cells) > 0 {
		fmt.Fprintf(&b, "| step | load | n | ttft_u_p50_ms | ttft_c_p50_ms | ttft_u_mean_ms | ttft_c_mean_ms | g | g_p50 | cached/prompt | errors |\n|---|---|---|---|---|---|---|---|---|---|---|\n")
		for _, c := range s.Cells {
			fmt.Fprintf(&b, "| %s | %d | %d | %s | %s | %s | %s | %s | %s | %s | %d |\n", c.Step, c.Load, c.Pairs,
				val(c.TTFTUp50, "%.1f"), val(c.TTFTCp50, "%.1f"), val(c.TTFTUmean, "%.1f"), val(c.TTFTCmean, "%.1f"),
				val(c.G, "%.3f"), val(c.GP50, "%.3f"), val(c.CachedFrac, "%.3f"), c.Errors)
		}
		fmt.Fprintln(&b)
		for _, c := range s.Cells {
			if c.LoadErrors > 0 {
				fmt.Fprintf(&b, "load %d: %d of %d background requests failed, first error: %s\n", c.Load, c.LoadErrors, c.LoadRequests, c.LoadFirstError)
			}
		}
	}
	if len(s.Residency) > 0 {
		fmt.Fprintf(&b, "| N | revisits | engine_cached_frac | share_cached_ge_half | revisit_ceiling | fill_errors | errors |\n|---|---|---|---|---|---|---|\n")
		for _, c := range s.Residency {
			fmt.Fprintf(&b, "| %d | %d | %s | %s | %s | %d | %d |\n", c.N, c.Revisits,
				val(c.EngineCachedFr, "%.3f"), val(c.ShareCached, "%.3f"), val(c.Ceiling, "%.3f"), c.FillErrors, c.Errors)
		}
		fmt.Fprintf(&b, "\nk (largest N with engine hit fraction >= %.2f): %s\n", hitLine, s.K)
	}
	for _, w := range s.Warnings {
		fmt.Fprintf(&b, "\nWARNING: %s\n", w)
	}
	if len(s.GainLevels) > 0 {
		fmt.Fprintf(&b, "\nload levels with g >= %.2f: %v\n", s.GainLine, s.GainLevels)
	}
	return b.String()
}

func optInt(v *int) string {
	if v == nil {
		return "-"
	}
	return strconv.Itoa(*v)
}

func optBool(v *bool) string {
	if v == nil {
		return "-"
	}
	return strconv.FormatBool(*v)
}

// mdReplacer keeps free text from breaking the markdown output.
var mdReplacer = strings.NewReplacer("\n", " ", "\r", " ", "|", "/", "`", "'")

func mdText(v string) string { return mdReplacer.Replace(v) }
