package main

// report.go - provenance (config.json), resume bookkeeping and the summary
// written at the end of a run. The summary lists raw per-arm percentiles, the
// paired comparison against the baseline arm, every validity flag, and a
// verdict only when a pre-registered criteria file was supplied.

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"
)

type provenance struct {
	Experiment   *experiment `json:"experiment"`
	Argv         []string    `json:"argv"`
	StartedAt    time.Time   `json:"started_at"`
	Host         string      `json:"host"`
	GoVersion    string      `json:"go_version"`
	GOOS         string      `json:"goos"`
	RouterBins   []binInfo   `json:"router_binaries"`
	NvidiaSMI    string      `json:"nvidia_smi,omitempty"`
	CriteriaFile string      `json:"criteria_file,omitempty"`
	CriteriaSHA  string      `json:"criteria_sha256,omitempty"`
}

type binInfo struct {
	Path    string `json:"path"`
	SHA256  string `json:"sha256"`
	Version string `json:"version,omitempty"`
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func commandOutput(name string, args ...string) string {
	out, err := exec.Command(name, args...).CombinedOutput()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// writeConfig records everything needed to audit or repeat the run. A
// criteria file, if given, is hashed and validated here, before any run
// starts, so a malformed or missing file fails fast.
func writeConfig(e *experiment, argv []string) error {
	host, _ := os.Hostname()
	p := provenance{
		Experiment: e, Argv: argv, StartedAt: time.Now().UTC(), Host: host,
		GoVersion: runtime.Version(), GOOS: runtime.GOOS,
		NvidiaSMI: commandOutput("nvidia-smi", "--query-gpu=name,driver_version,memory.total", "--format=csv,noheader"),
	}
	seen := map[string]bool{}
	for _, a := range e.Arms {
		bin := e.binFor(a)
		if a.Kind != "router" || bin == "" || seen[bin] {
			continue
		}
		seen[bin] = true
		sum, err := fileSHA256(bin)
		if err != nil {
			return fmt.Errorf("router binary %s: %w", bin, err)
		}
		p.RouterBins = append(p.RouterBins, binInfo{Path: bin, SHA256: sum, Version: commandOutput(bin, "-version")})
	}
	if e.Criteria != "" {
		if _, err := loadCriteria(e.Criteria); err != nil {
			return err
		}
		sum, err := fileSHA256(e.Criteria)
		if err != nil {
			return err
		}
		p.CriteriaFile, p.CriteriaSHA = e.Criteria, sum
	}
	b, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	path := filepath.Join(e.OutDir, "config.json")
	if old, err := os.ReadFile(path); err == nil {
		// Resuming: keep the original provenance, but refuse a changed setup.
		var prev provenance
		if json.Unmarshal(old, &prev) != nil || !sameExperiment(prev.Experiment, e) {
			return fmt.Errorf("%s exists with a different experiment; use a new --out directory", path)
		}
		return nil
	}
	return os.WriteFile(path, b, 0o644)
}

func sameExperiment(a, b *experiment) bool {
	if a == nil || b == nil {
		return false
	}
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return string(x) == string(y)
}

// completedRuns returns the run indexes that already have a runs.jsonl line.
func completedRuns(outDir string) map[int]bool {
	done := map[int]bool{}
	for _, l := range readRunLines(outDir) {
		done[l.Plan.Index] = true
	}
	return done
}

func readRunLines(outDir string) []runLine {
	f, err := os.Open(filepath.Join(outDir, "runs.jsonl"))
	if err != nil {
		return nil
	}
	defer f.Close()
	last := map[int]runLine{}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1<<20), 64<<20)
	for sc.Scan() {
		var l runLine
		if json.Unmarshal(sc.Bytes(), &l) == nil {
			last[l.Plan.Index] = l
		}
	}
	out := make([]runLine, 0, len(last))
	for _, l := range last {
		out = append(out, l)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Plan.Index < out[j].Plan.Index })
	return out
}

func readRequests(outDir string, index int) ([]reqRecord, error) {
	f, err := os.Open(requestsPath(outDir, index))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var recs []reqRecord
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1<<20), 16<<20)
	for sc.Scan() {
		var r reqRecord
		if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
			return nil, err
		}
		recs = append(recs, r)
	}
	return recs, sc.Err()
}

// readFileTail returns the last n bytes of a file as a string.
func readFileTail(path string, n int) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return "", err
	}
	off := st.Size() - int64(n)
	if off < 0 {
		off = 0
	}
	buf := make([]byte, st.Size()-off)
	if _, err := f.ReadAt(buf, off); err != nil && err != io.EOF {
		return "", err
	}
	return string(buf), nil
}

// writeSummary rebuilds summaries.json, paired.json and summary.md (and
// verdict.json when criteria were given) from the raw run records in outDir.
// It can be rerun at any time (the summarize command).
func writeSummary(outDir string) error {
	raw, err := os.ReadFile(filepath.Join(outDir, "config.json"))
	if err != nil {
		return fmt.Errorf("reading config.json: %w", err)
	}
	var prov provenance
	if err := json.Unmarshal(raw, &prov); err != nil || prov.Experiment == nil {
		return fmt.Errorf("config.json is unreadable: %v", err)
	}
	e := prov.Experiment

	var sums []runSummary
	for _, l := range readRunLines(outDir) {
		recs, err := readRequests(outDir, l.Plan.Index)
		if err != nil {
			return fmt.Errorf("run %d: %w", l.Plan.Index, err)
		}
		sums = append(sums, summarizeRun(l.runMeta, recs))
	}
	if len(sums) == 0 {
		return fmt.Errorf("no completed runs in %s", outDir)
	}
	paired := comparePaired(sums, e.Arms[0].Name, e.Seed)

	var v *verdict
	if e.Criteria != "" {
		c, err := loadCriteria(e.Criteria)
		if err != nil {
			return err
		}
		ev := evaluate(c, sums, paired)
		v = &ev
	}

	if err := writeJSON(filepath.Join(outDir, "summaries.json"), sums); err != nil {
		return err
	}
	if err := writeJSON(filepath.Join(outDir, "paired.json"), paired); err != nil {
		return err
	}
	if v != nil {
		if err := writeJSON(filepath.Join(outDir, "verdict.json"), v); err != nil {
			return err
		}
	}
	return os.WriteFile(filepath.Join(outDir, "summary.md"), []byte(renderSummary(&prov, sums, paired, v)), 0o644)
}

func writeJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o644)
}

func f1(v float64) string {
	if math.IsNaN(v) || v < 0 {
		return "-"
	}
	return fmt.Sprintf("%.1f", v)
}

func pct(v float64) string {
	if v < 0 || math.IsNaN(v) {
		return "-"
	}
	return fmt.Sprintf("%.0f%%", v*100)
}

type cellKey struct{ workload, arm string }

func renderSummary(prov *provenance, sums []runSummary, paired []pairedResult, v *verdict) string {
	e := prov.Experiment
	var b strings.Builder
	fmt.Fprintf(&b, "# prefixbench summary\n\n")
	if e.Smoke || e.EngineKind == "sim" {
		fmt.Fprintf(&b, "> NOT A MEASUREMENT. This run used smoke mode or the simulated engine; it validates the harness only and must not be quoted as a result.\n\n")
	}
	fmt.Fprintf(&b, "preset %s, engines %d (%s), reps %d, concurrency %d, seed %d, runs %d\n\n", e.Preset, len(e.Engines), e.EngineKind, e.Reps, e.Concurrency, e.Seed, len(sums))
	if prov.CriteriaSHA != "" {
		fmt.Fprintf(&b, "criteria file: %s (sha256 %s)\n\n", prov.CriteriaFile, prov.CriteriaSHA)
	} else {
		fmt.Fprintf(&b, "no criteria file supplied: numbers only, no verdict stated.\n\n")
	}
	if v != nil {
		fmt.Fprintf(&b, "## Verdict: %s (%s)\n\n", v.Outcome, v.Name)
		for _, r := range v.Reasons {
			fmt.Fprintf(&b, "- %s\n", r)
		}
		fmt.Fprintln(&b)
	}

	fmt.Fprintf(&b, "## Raw per-arm numbers (median across reps)\n\n")
	fmt.Fprintf(&b, "| workload | arm | runs | flagged | ttft p50 | ttft p95 | ttft p99 | itl mean | err | rps | colocation | engine cached | min node share |\n|---|---|---|---|---|---|---|---|---|---|---|---|---|\n")
	byCell := map[cellKey][]runSummary{}
	var cells []cellKey
	for _, s := range sums {
		c := cellKey{s.Workload, s.Arm}
		if _, ok := byCell[c]; !ok {
			cells = append(cells, c)
		}
		byCell[c] = append(byCell[c], s)
	}
	sort.Slice(cells, func(i, j int) bool {
		if cells[i].workload != cells[j].workload {
			return cells[i].workload < cells[j].workload
		}
		return cells[i].arm < cells[j].arm
	})
	for _, c := range cells {
		rs := byCell[c]
		col := func(get func(runSummary) float64) float64 {
			var o []float64
			for _, r := range rs {
				if x := get(r); !math.IsNaN(x) && x >= 0 {
					o = append(o, x)
				}
			}
			return median(o)
		}
		flagged := 0
		for _, r := range rs {
			if len(r.Flags) > 0 {
				flagged++
			}
		}
		fmt.Fprintf(&b, "| %s | %s | %d | %d | %s | %s | %s | %s | %s | %s | %s | %s | %s |\n", c.workload, c.arm, len(rs), flagged,
			f1(col(func(s runSummary) float64 { return s.TTFTp50 })),
			f1(col(func(s runSummary) float64 { return s.TTFTp95 })),
			f1(col(func(s runSummary) float64 { return s.TTFTp99 })),
			f1(col(func(s runSummary) float64 { return s.ITLMean })),
			pct(col(func(s runSummary) float64 { return s.ErrorFrac })),
			f1(col(func(s runSummary) float64 { return s.Throughput })),
			pct(col(func(s runSummary) float64 { return s.Colocation })),
			pct(col(func(s runSummary) float64 { return s.EngineCached })),
			pct(col(func(s runSummary) float64 { return s.MinNodeShare })))
	}
	fmt.Fprintf(&b, "\nTimes in ms. Colocation is the share of a conversation's later turns that landed on the node that served its previous turn. Engine cached is read from the engine's own counters.\n\n")

	fmt.Fprintf(&b, "## Paired comparison against %s\n\n", e.Arms[0].Name)
	fmt.Fprintf(&b, "Per-rep delta (arm minus baseline), median with a bootstrap 95%% interval, reps that improved or worsened, and a two-sided sign test. Ten pairs give a coarse interval; read it as a range of plausible values, not a precise bound.\n\n")
	fmt.Fprintf(&b, "| workload | arm | metric | pairs | median delta | 95%% CI | median rel | improved/worse | sign p |\n|---|---|---|---|---|---|---|---|---|\n")
	for _, p := range paired {
		fmt.Fprintf(&b, "| %s | %s | %s | %d | %.3g | [%.3g, %.3g] | %.1f%% | %d/%d | %.3f |\n", p.Workload, p.Arm, p.Metric, p.Pairs, p.MedianDelta, p.CILow, p.CIHigh, p.MedianRel*100, p.Improved, p.Worsened, p.SignP)
	}

	fmt.Fprintf(&b, "\n## Validity flags\n\n")
	flaggedAny := false
	for _, s := range sums {
		if len(s.Flags) > 0 {
			flaggedAny = true
			fmt.Fprintf(&b, "- run %d (%s, %s, rep %d): %s\n", s.RunIndex, s.Workload, s.Arm, s.Rep, strings.Join(s.Flags, ", "))
		}
	}
	if !flaggedAny {
		fmt.Fprintf(&b, "none\n")
	}
	return b.String()
}
