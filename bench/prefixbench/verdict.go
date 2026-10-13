package main

// verdict.go - evaluates a pre-registered criteria file against the paired
// results. The criteria are written and approved before any measured run; the
// file's sha256 is recorded in config.json so the run shows which criteria it
// was judged by. Without a criteria file the tool reports numbers only and
// never states a verdict, so no threshold can be chosen after seeing results.
//
// Outcomes: PASS (every condition met), FAIL (valid data, a condition not
// met) and INVALID (the data cannot support a verdict at all: too few pairs,
// or a validity gate fired in a run the verdict depends on).

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

type regressionLimit struct {
	Workload string  `json:"workload"`
	Arm      string  `json:"arm"`
	Metric   string  `json:"metric"`
	MaxWorse float64 `json:"max_relative_worsening"`
}

type criteria struct {
	Name                  string            `json:"name"`
	Workload              string            `json:"workload"`
	Arm                   string            `json:"arm"`
	Baseline              string            `json:"baseline"`
	Metric                string            `json:"metric"`
	MinRelImprovement     float64           `json:"min_relative_improvement"`
	MinRepsImproved       int               `json:"min_reps_improved"`
	RequireCIExcludesZero bool              `json:"require_ci_excludes_zero"`
	MinPairs              int               `json:"min_pairs"`
	NoRegression          []regressionLimit `json:"no_regression"`
}

type verdict struct {
	Outcome string   `json:"outcome"`
	Name    string   `json:"name"`
	Reasons []string `json:"reasons"`
}

func loadCriteria(path string) (*criteria, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c criteria
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("criteria file %s: %w", path, err)
	}
	if c.Workload == "" || c.Arm == "" || c.Baseline == "" || c.Metric == "" {
		return nil, fmt.Errorf("criteria file %s: workload, arm, baseline and metric are required", path)
	}
	if c.MinPairs < minReps {
		return nil, fmt.Errorf("criteria file %s: min_pairs must be at least %d", path, minReps)
	}
	return &c, nil
}

func findPaired(ps []pairedResult, workload, arm, metricName string) (pairedResult, bool) {
	for _, p := range ps {
		if p.Workload == workload && p.Arm == arm && p.Metric == metricName {
			return p, true
		}
	}
	return pairedResult{}, false
}

// improvement returns the relative change in the good direction (positive is
// better), whichever way the metric points.
func improvement(p pairedResult) float64 {
	if p.LowerBetter {
		return -p.MedianRel
	}
	return p.MedianRel
}

func flaggedRuns(sums []runSummary, workload string, arms ...string) []string {
	var out []string
	for _, s := range sums {
		if s.Workload != workload || len(s.Flags) == 0 {
			continue
		}
		for _, a := range arms {
			if s.Arm == a {
				out = append(out, fmt.Sprintf("run %d (%s, rep %d): %s", s.RunIndex, s.Arm, s.Rep, strings.Join(s.Flags, ",")))
			}
		}
	}
	return out
}

func evaluate(c *criteria, sums []runSummary, paired []pairedResult) verdict {
	v := verdict{Name: c.Name}
	invalid := func(format string, a ...any) {
		v.Reasons = append(v.Reasons, fmt.Sprintf(format, a...))
		v.Outcome = "INVALID"
	}
	p, ok := findPaired(paired, c.Workload, c.Arm, c.Metric)
	if !ok {
		invalid("no paired data for %s / %s / %s", c.Workload, c.Arm, c.Metric)
		return v
	}
	if p.Pairs < c.MinPairs {
		invalid("only %d usable pairs, criteria require %d", p.Pairs, c.MinPairs)
	}
	if fl := flaggedRuns(sums, c.Workload, c.Arm, c.Baseline); len(fl) > 0 {
		invalid("validity gate fired in runs the verdict depends on: %s", strings.Join(fl, "; "))
	}
	if v.Outcome == "INVALID" {
		return v
	}

	failed := false
	fail := func(format string, a ...any) {
		v.Reasons = append(v.Reasons, fmt.Sprintf(format, a...))
		failed = true
	}
	pass := func(format string, a ...any) { v.Reasons = append(v.Reasons, "ok: "+fmt.Sprintf(format, a...)) }

	if got := improvement(p); got >= c.MinRelImprovement {
		pass("median relative improvement %.1f%% >= %.1f%%", got*100, c.MinRelImprovement*100)
	} else {
		fail("median relative improvement %.1f%% < required %.1f%%", got*100, c.MinRelImprovement*100)
	}
	if p.Improved >= c.MinRepsImproved {
		pass("%d of %d reps improved (need %d)", p.Improved, p.Pairs, c.MinRepsImproved)
	} else {
		fail("%d of %d reps improved, need %d", p.Improved, p.Pairs, c.MinRepsImproved)
	}
	if c.RequireCIExcludesZero {
		excludes := (p.LowerBetter && p.CIHigh < 0) || (!p.LowerBetter && p.CILow > 0)
		if excludes {
			pass("95%% CI [%.3g, %.3g] excludes zero in the improving direction", p.CILow, p.CIHigh)
		} else {
			fail("95%% CI [%.3g, %.3g] does not exclude zero in the improving direction", p.CILow, p.CIHigh)
		}
	}
	for _, r := range c.NoRegression {
		rp, ok := findPaired(paired, r.Workload, r.Arm, r.Metric)
		if !ok {
			fail("no-regression check %s/%s/%s: no paired data", r.Workload, r.Arm, r.Metric)
			continue
		}
		if fl := flaggedRuns(sums, r.Workload, r.Arm, c.Baseline); len(fl) > 0 {
			fail("no-regression check %s/%s/%s: validity gate fired: %s", r.Workload, r.Arm, r.Metric, strings.Join(fl, "; "))
			continue
		}
		if worse := -improvement(rp); worse > r.MaxWorse {
			fail("no-regression check %s/%s/%s: median worsening %.1f%% > allowed %.1f%%", r.Workload, r.Arm, r.Metric, worse*100, r.MaxWorse*100)
		} else {
			pass("no-regression %s/%s/%s: median change %.1f%% (limit %.1f%% worse)", r.Workload, r.Arm, r.Metric, worse*100, r.MaxWorse*100)
		}
	}
	v.Outcome = "PASS"
	if failed {
		v.Outcome = "FAIL"
	}
	return v
}
