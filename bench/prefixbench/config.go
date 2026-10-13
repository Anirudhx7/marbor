package main

// config.go - experiment definition: engines, routing arms, workloads, and
// the flag parsing that builds them. Everything that decides what a run does
// is captured here and written verbatim to config.json so a run can be
// reproduced and audited later.

import (
	"errors"
	"flag"
	"fmt"
	"path/filepath"
	"strings"
	"time"
)

// minReps is the smallest number of interleaved repetitions per
// (workload, arm) cell accepted for a real experiment. --smoke lowers it for
// harness checks that are never reported as results.
const minReps = 10

type engineSpec struct {
	Name string `json:"name"`
	URL  string `json:"url"`
}

// armSpec is one routing condition under test.
//
//	kind "router": requests go through a freshly started router process whose
//	               settings are overridden by Settings.
//	kind "hash":   requests bypass the router; the harness sends every request
//	               carrying the same conversation head to the same engine, with
//	               no awareness of load (a prefix-only baseline).
type armSpec struct {
	Name     string         `json:"name"`
	Kind     string         `json:"kind"`
	Bin      string         `json:"bin,omitempty"`
	Settings map[string]any `json:"settings,omitempty"`
}

// workloadSpec describes the synthetic traffic of one run. All lengths are in
// words; the engine reports the real token counts.
type workloadSpec struct {
	Name          string `json:"name"`
	Conversations int    `json:"conversations"`
	Turns         int    `json:"turns"`
	SystemWords   int    `json:"system_words"`
	ContextWords  int    `json:"context_words"`
	TurnWords     int    `json:"turn_words"`
	AnswerWords   int    `json:"answer_words"`
}

type experiment struct {
	Preset      string         `json:"preset"`
	Engines     []engineSpec   `json:"engines"`
	EngineKind  string         `json:"engine_kind"`
	Runtime     string         `json:"runtime"`
	Model       string         `json:"model"`
	Arms        []armSpec      `json:"arms"`
	Workloads   []workloadSpec `json:"workloads"`
	Reps        int            `json:"reps"`
	Concurrency int            `json:"concurrency"`
	ThinkMs     int            `json:"think_ms"`
	MaxTokens   int            `json:"max_tokens"`
	Seed        int64          `json:"seed"`
	DefaultBin  string         `json:"default_bin"`
	RouterSet   map[string]any `json:"router_set,omitempty"`
	OutDir      string         `json:"out_dir"`
	Criteria    string         `json:"criteria,omitempty"`
	TimeoutSec  int            `json:"timeout_sec"`
	Smoke       bool           `json:"smoke"`
	timeout     time.Duration
}

type listFlag []string

func (l *listFlag) String() string     { return strings.Join(*l, ",") }
func (l *listFlag) Set(v string) error { *l = append(*l, v); return nil }

// parseEngines parses "name=url,name=url".
func parseEngines(s string) ([]engineSpec, error) {
	var out []engineSpec
	seen := map[string]bool{}
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		name, url, ok := strings.Cut(part, "=")
		if !ok || name == "" || url == "" {
			return nil, fmt.Errorf("engine %q: want name=url", part)
		}
		if seen[name] {
			return nil, fmt.Errorf("engine name %q repeated", name)
		}
		seen[name] = true
		out = append(out, engineSpec{Name: name, URL: strings.TrimRight(url, "/")})
	}
	return out, nil
}

// defaultWorkloads returns the three traffic shapes:
//
//	conv:   many multi-turn conversations, each with its own long first
//	        message, so the only reusable prefix is a conversation's own history.
//	shared: conversations that all start with the same long system prompt.
//	unique: single-turn requests with unique content (no reusable prefix); the
//	        control that shows what routing does when there is nothing to reuse.
func defaultWorkloads(conversations, turns, contextWords, systemWords, turnWords, answerWords int) map[string]workloadSpec {
	return map[string]workloadSpec{
		"conv":   {Name: "conv", Conversations: conversations, Turns: turns, ContextWords: contextWords, TurnWords: turnWords, AnswerWords: answerWords},
		"shared": {Name: "shared", Conversations: conversations, Turns: turns, SystemWords: systemWords, ContextWords: contextWords / 10, TurnWords: turnWords, AnswerWords: answerWords},
		"unique": {Name: "unique", Conversations: conversations * turns, Turns: 1, ContextWords: contextWords, TurnWords: turnWords},
	}
}

func parseRouterSet(vals []string) (map[string]any, error) {
	out := map[string]any{}
	for _, v := range vals {
		k, raw, ok := strings.Cut(v, "=")
		if !ok || k == "" {
			return nil, fmt.Errorf("--router-set %q: want key=value", v)
		}
		out[k] = coerce(raw)
	}
	return out, nil
}

func coerce(raw string) any {
	switch raw {
	case "true":
		return true
	case "false":
		return false
	}
	var n float64
	if _, err := fmt.Sscanf(raw, "%g", &n); err == nil && fmt.Sprint(n) == raw {
		return n
	}
	return raw
}

// parseRunFlags builds an experiment from the `run` subcommand's arguments.
func parseRunFlags(args []string) (*experiment, error) {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	var routerSet, armBins listFlag
	preset := fs.String("preset", "prefix", "prefix (load-only, prefix-only, prefix-plus-load) or loadspread (baseline vs candidate router build)")
	engines := fs.String("engines", "", "backend engines as name=url,name=url (required)")
	engineKind := fs.String("engine-kind", "vllm", "engine probe kind: vllm, sim (mock with SIM_KV=1), or none")
	runtime := fs.String("runtime", "vllm", "runtime name used when registering engines with the router")
	model := fs.String("model", "", "model id sent in requests (required)")
	bin := fs.String("router-bin", "", "path to the router binary (required for router arms)")
	fs.Var(&armBins, "arm-bin", "loadspread preset only: baseline=PATH and candidate=PATH (repeat twice)")
	fs.Var(&routerSet, "router-set", "extra routing setting for every router arm, key=value (repeatable)")
	workloads := fs.String("workloads", "conv,unique", "comma list from: conv, shared, unique")
	reps := fs.Int("reps", minReps, "interleaved repetitions per (workload, arm) cell")
	conc := fs.Int("concurrency", 12, "simultaneous conversations (clients)")
	conversations := fs.Int("conversations", 24, "conversations per run")
	turns := fs.Int("turns", 5, "turns per conversation")
	contextWords := fs.Int("context-words", 1500, "words in a conversation's first message")
	systemWords := fs.Int("system-words", 600, "words in the shared system prompt (shared workload)")
	turnWords := fs.Int("turn-words", 40, "words in each later user message")
	answerWords := fs.Int("answer-words", 60, "words in each canned assistant message in the history")
	think := fs.Int("think-ms", 300, "mean pause between a client's turns")
	maxTokens := fs.Int("max-tokens", 64, "max_tokens per request")
	seed := fs.Int64("seed", 1, "base random seed")
	out := fs.String("out", "", "output directory (required)")
	criteria := fs.String("criteria", "", "pre-registered criteria file (optional)")
	timeout := fs.Duration("timeout", 180*time.Second, "per-request timeout")
	weight := fs.Float64("prefix-weight", 10, "prefix_locality_weight for the prefix-plus-load arm")
	smoke := fs.Bool("smoke", false, "allow fewer than the minimum repetitions (harness checks only, never reported as results)")
	if err := fs.Parse(args); err != nil {
		return nil, err
	}

	e := &experiment{
		Preset: *preset, EngineKind: *engineKind, Runtime: *runtime, Model: *model,
		Reps: *reps, Concurrency: *conc, ThinkMs: *think, MaxTokens: *maxTokens,
		Seed: *seed, DefaultBin: *bin, OutDir: *out, Criteria: *criteria,
		TimeoutSec: int(timeout.Seconds()), Smoke: *smoke, timeout: *timeout,
	}
	var err error
	if e.Engines, err = parseEngines(*engines); err != nil {
		return nil, err
	}
	if e.RouterSet, err = parseRouterSet(routerSet); err != nil {
		return nil, err
	}
	if _, ok := e.RouterSet["session_affinity"]; !ok {
		e.RouterSet["session_affinity"] = false
	}
	wl := defaultWorkloads(*conversations, *turns, *contextWords, *systemWords, *turnWords, *answerWords)
	for _, name := range strings.Split(*workloads, ",") {
		name = strings.TrimSpace(name)
		spec, ok := wl[name]
		if !ok {
			return nil, fmt.Errorf("unknown workload %q", name)
		}
		e.Workloads = append(e.Workloads, spec)
	}
	switch *preset {
	case "prefix":
		e.Arms = []armSpec{
			{Name: "load-only", Kind: "router", Settings: map[string]any{"prefix_locality_enabled": false}},
			{Name: "prefix-only", Kind: "hash"},
			{Name: "prefix-plus-load", Kind: "router", Settings: map[string]any{"prefix_locality_enabled": true, "prefix_locality_weight": *weight}},
		}
	case "loadspread":
		bins := map[string]string{}
		for _, ab := range armBins {
			k, v, ok := strings.Cut(ab, "=")
			if !ok {
				return nil, fmt.Errorf("--arm-bin %q: want baseline=PATH or candidate=PATH", ab)
			}
			bins[k] = v
		}
		if bins["baseline"] == "" || bins["candidate"] == "" {
			return nil, errors.New("preset loadspread needs --arm-bin baseline=PATH and --arm-bin candidate=PATH")
		}
		e.Arms = []armSpec{
			{Name: "baseline", Kind: "router", Bin: bins["baseline"]},
			{Name: "candidate", Kind: "router", Bin: bins["candidate"]},
		}
	default:
		return nil, fmt.Errorf("unknown preset %q", *preset)
	}
	if err := e.absolutize(); err != nil {
		return nil, err
	}
	return e, e.validate()
}

// absolutize resolves every path the router supervisor uses, because each
// router runs with its own working directory.
func (e *experiment) absolutize() error {
	paths := []*string{&e.DefaultBin, &e.OutDir, &e.Criteria}
	for i := range e.Arms {
		paths = append(paths, &e.Arms[i].Bin)
	}
	for _, p := range paths {
		if *p == "" {
			continue
		}
		abs, err := filepath.Abs(*p)
		if err != nil {
			return err
		}
		*p = abs
	}
	return nil
}

func (e *experiment) validate() error {
	switch {
	case len(e.Engines) < 2:
		return errors.New("need at least 2 engines (--engines a=url,b=url)")
	case e.Model == "":
		return errors.New("--model is required")
	case e.OutDir == "":
		return errors.New("--out is required")
	case e.Reps < 1:
		return errors.New("--reps must be >= 1")
	case e.Reps < minReps && !e.Smoke:
		return fmt.Errorf("--reps %d is below the minimum %d; pass --smoke for harness checks (results from a smoke run must not be reported)", e.Reps, minReps)
	case e.Concurrency < 1:
		return errors.New("--concurrency must be >= 1")
	case e.EngineKind != "vllm" && e.EngineKind != "sim" && e.EngineKind != "none":
		return fmt.Errorf("--engine-kind %q: want vllm, sim or none", e.EngineKind)
	}
	for _, a := range e.Arms {
		if a.Kind == "router" && a.Bin == "" && e.DefaultBin == "" {
			return fmt.Errorf("arm %q needs a router binary (--router-bin)", a.Name)
		}
	}
	return nil
}

func (e *experiment) binFor(a armSpec) string {
	if a.Bin != "" {
		return a.Bin
	}
	return e.DefaultBin
}
