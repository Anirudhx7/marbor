// prefixbench compares routing arms on prefix-sharing traffic.
//
// It starts a fresh router per run (or dispatches directly for a prefix-only
// baseline), drives a deterministic multi-turn workload against two or more
// inference engines, and records every request plus independent evidence of
// what the engines and the router actually did. Arms are interleaved across
// repetitions so slow drift in the hardware does not favor one arm.
//
// Subcommands:
//
//	plan      print the interleaved run schedule without running anything
//	run       execute the schedule and write raw results and a summary
//	summarize rebuild summary.md and summary.json from an existing output dir
//	capture   save raw engine responses (to see which usage fields exist)
//
// The tool reports measurements and their variability. It states a verdict
// only when given a criteria file written before the run (see verdict.go).
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"os/signal"
	"path/filepath"
	"time"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "plan":
		err = cmdPlan(os.Args[2:])
	case "run":
		err = cmdRun(os.Args[2:])
	case "summarize":
		err = cmdSummarize(os.Args[2:])
	case "capture":
		err = cmdCapture(os.Args[2:])
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: prefixbench plan|run|summarize|capture [flags]   (each subcommand accepts -h)")
}

// buildSchedule lays out every run. For each repetition, workloads are taken
// in a shuffled order and, within a workload, arms are shuffled, so every arm
// is exposed to the same slow drift. The schedule depends only on the seed.
func buildSchedule(e *experiment) []runPlan {
	rng := rand.New(rand.NewSource(e.Seed))
	var plans []runPlan
	idx := 1
	for rep := 1; rep <= e.Reps; rep++ {
		wls := append([]workloadSpec(nil), e.Workloads...)
		rng.Shuffle(len(wls), func(i, j int) { wls[i], wls[j] = wls[j], wls[i] })
		for _, w := range wls {
			arms := append([]armSpec(nil), e.Arms...)
			rng.Shuffle(len(arms), func(i, j int) { arms[i], arms[j] = arms[j], arms[i] })
			for _, a := range arms {
				plans = append(plans, runPlan{Index: idx, Rep: rep, Workload: w, Arm: a})
				idx++
			}
		}
	}
	return plans
}

func cmdPlan(args []string) error {
	e, err := parseRunFlags(args)
	if err != nil {
		return err
	}
	for _, p := range buildSchedule(e) {
		fmt.Printf("run %3d  rep %2d  workload %-7s arm %s\n", p.Index, p.Rep, p.Workload.Name, p.Arm.Name)
	}
	return nil
}

func cmdRun(args []string) error {
	e, err := parseRunFlags(args)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(e.OutDir, "requests"), 0o755); err != nil {
		return err
	}
	if err := writeConfig(e, args); err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	runFile, err := os.OpenFile(filepath.Join(e.OutDir, "runs.jsonl"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer runFile.Close()
	errFile, err := os.OpenFile(filepath.Join(e.OutDir, "errors.log"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer errFile.Close()
	done := completedRuns(e.OutDir)

	plans := buildSchedule(e)
	for _, p := range plans {
		if ctx.Err() != nil {
			fmt.Fprintln(os.Stderr, "interrupted; completed runs are saved, rerun the same command to resume")
			break
		}
		if done[p.Index] {
			fmt.Printf("run %3d/%d skipped (already recorded)\n", p.Index, len(plans))
			continue
		}
		start := time.Now()
		fmt.Printf("run %3d/%d  rep %d  %-7s  %s ... ", p.Index, len(plans), p.Rep, p.Workload.Name, p.Arm.Name)
		res, err := executeRun(ctx, e, p, e.OutDir)
		if res == nil || ctx.Err() != nil {
			if err != nil {
				fmt.Fprintf(errFile, "%s run %d (%s/%s): %v\n", time.Now().Format(time.RFC3339), p.Index, p.Workload.Name, p.Arm.Name, err)
				fmt.Printf("FAILED: %v\n", err)
			}
			continue
		}
		if err := persistRun(e.OutDir, runFile, res); err != nil {
			return err
		}
		sum := summarizeRun(metaOf(res), res.Records)
		fmt.Printf("%s  ttft p50 %.0f ms  errors %d/%d  flags %v\n", time.Since(start).Round(time.Second), sum.TTFTp50, sum.Errors, sum.Requests, sum.Flags)
	}
	return writeSummary(e.OutDir)
}

func metaOf(r *runResult) runMeta {
	return runMeta{Plan: r.Plan, DurationMs: r.DurationMs, Deltas: r.Deltas, ExplainOK: r.ExplainOK, ExplainReq: r.ExplainReq}
}

// runLine is one runs.jsonl entry: the run's metadata and raw engine readings.
type runLine struct {
	runMeta
	StartedAt time.Time        `json:"started_at"`
	Before    []engineSnapshot `json:"before"`
	After     []engineSnapshot `json:"after"`
	Settings  json.RawMessage  `json:"settings,omitempty"`
	RouterLog string           `json:"router_log_tail,omitempty"`
}

// persistRun saves a finished run. Its requests go to their own file (so a
// rerun after a crash overwrites instead of duplicating), and the runs.jsonl
// line is written last: a run counts as complete only once that line exists.
func persistRun(outDir string, runFile *os.File, r *runResult) error {
	var buf []byte
	for _, rec := range r.Records {
		rb, err := json.Marshal(rec)
		if err != nil {
			return err
		}
		buf = append(append(buf, rb...), '\n')
	}
	if err := os.WriteFile(requestsPath(outDir, r.Plan.Index), buf, 0o644); err != nil {
		return err
	}
	b, err := json.Marshal(runLine{metaOf(r), r.StartedAt, r.Before, r.After, r.Settings, r.RouterLog})
	if err != nil {
		return err
	}
	if _, err := runFile.Write(append(b, '\n')); err != nil {
		return err
	}
	return runFile.Sync()
}

func requestsPath(outDir string, index int) string {
	return filepath.Join(outDir, "requests", fmt.Sprintf("run%03d.jsonl", index))
}

func cmdSummarize(args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: prefixbench summarize <output-dir>")
	}
	return writeSummary(args[0])
}
