package main

// stats.go - per-run summaries, per-cell aggregation and paired comparison
// against the baseline arm. Everything here is descriptive: it reports numbers
// and their variability. It never decides that an arm "won"; a verdict only
// exists when a pre-registered criteria file is supplied (see verdict.go).

import (
	"math"
	"math/rand"
	"sort"
)

// Gate flags a run can carry. A flagged run is still reported; the flags say
// why its numbers may not mean what the arm name suggests.
const (
	flagHighErrors     = "high_errors"
	flagCollapsed      = "collapsed_placement"
	flagLowExplain     = "low_explain_coverage"
	flagNoEngineSignal = "no_engine_evidence"
)

const (
	maxErrorFrac       = 0.02
	minNodeShare       = 0.10
	minExplainCoverage = 0.95
)

type runMeta struct {
	Plan       runPlan       `json:"plan"`
	DurationMs float64       `json:"duration_ms"`
	Deltas     []engineDelta `json:"engine_deltas"`
	ExplainOK  int           `json:"explain_ok"`
	ExplainReq int           `json:"explain_requested"`
}

type runSummary struct {
	RunIndex      int            `json:"run"`
	Rep           int            `json:"rep"`
	Workload      string         `json:"workload"`
	Arm           string         `json:"arm"`
	Requests      int            `json:"requests"`
	Errors        int            `json:"errors"`
	ErrorFrac     float64        `json:"error_frac"`
	TTFTp50       float64        `json:"ttft_p50_ms"`
	TTFTp95       float64        `json:"ttft_p95_ms"`
	TTFTp99       float64        `json:"ttft_p99_ms"`
	ITLMean       float64        `json:"itl_mean_ms"`
	TotalP95      float64        `json:"total_p95_ms"`
	Throughput    float64        `json:"throughput_rps"`
	NodeCounts    map[string]int `json:"node_counts"`
	MinNodeShare  float64        `json:"min_node_share"`
	Colocation    float64        `json:"colocation_rate"`
	PrefixRate    float64        `json:"prefix_match_rate"`
	EngineCached  float64        `json:"engine_cached_frac"`
	ReportedCache float64        `json:"reported_cached_frac"`
	Flags         []string       `json:"flags"`
	ttfts         []float64
}

// percentile returns the q-quantile (0..1) of an unsorted sample using linear
// interpolation, or NaN for an empty sample.
func percentile(vals []float64, q float64) float64 {
	if len(vals) == 0 {
		return math.NaN()
	}
	s := append([]float64(nil), vals...)
	sort.Float64s(s)
	if len(s) == 1 {
		return s[0]
	}
	pos := q * float64(len(s)-1)
	lo := int(math.Floor(pos))
	hi := int(math.Ceil(pos))
	return s[lo] + (s[hi]-s[lo])*(pos-float64(lo))
}

func median(vals []float64) float64 { return percentile(vals, 0.5) }

func mean(vals []float64) float64 {
	if len(vals) == 0 {
		return math.NaN()
	}
	var s float64
	for _, v := range vals {
		s += v
	}
	return s / float64(len(vals))
}

// summarizeRun reduces one run's raw records to the numbers and gate flags.
func summarizeRun(m runMeta, recs []reqRecord) runSummary {
	s := runSummary{
		RunIndex: m.Plan.Index, Rep: m.Plan.Rep, Workload: m.Plan.Workload.Name, Arm: m.Plan.Arm.Name,
		Requests: len(recs), NodeCounts: map[string]int{},
		Colocation: -1, PrefixRate: -1, EngineCached: -1, ReportedCache: -1, Flags: []string{},
	}
	var ttft, itl, total []float64
	var okCount int
	lastNode := map[int]string{}
	var coloc, colocN, pm, pmN int
	var repCached, repPrompt int
	sorted := append([]reqRecord(nil), recs...)
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].Conv != sorted[j].Conv {
			return sorted[i].Conv < sorted[j].Conv
		}
		return sorted[i].Turn < sorted[j].Turn
	})
	for _, r := range sorted {
		if r.Status != 200 || r.Error != "" {
			s.Errors++
			continue
		}
		okCount++
		if r.TTFTMs >= 0 {
			ttft = append(ttft, r.TTFTMs)
		}
		if r.MeanITLMs >= 0 {
			itl = append(itl, r.MeanITLMs)
		}
		total = append(total, r.TotalMs)
		if r.Node != "" {
			s.NodeCounts[r.Node]++
		}
		if r.Turn >= 2 {
			if prev, ok := lastNode[r.Conv]; ok && prev != "" && r.Node != "" {
				colocN++
				if prev == r.Node {
					coloc++
				}
			}
			if r.NodeSource == "explain" {
				pmN++
				if r.HasPrefix && r.PrefixRaw > 0 {
					pm++
				}
			}
		}
		lastNode[r.Conv] = r.Node
		if r.PromptTokens > 0 && r.CachedTokens >= 0 {
			repCached += r.CachedTokens
			repPrompt += r.PromptTokens
		}
	}
	if s.Requests > 0 {
		s.ErrorFrac = float64(s.Errors) / float64(s.Requests)
	}
	s.ttfts = ttft
	s.TTFTp50, s.TTFTp95, s.TTFTp99 = percentile(ttft, 0.5), percentile(ttft, 0.95), percentile(ttft, 0.99)
	s.ITLMean, s.TotalP95 = mean(itl), percentile(total, 0.95)
	if m.DurationMs > 0 {
		s.Throughput = float64(okCount) / (m.DurationMs / 1000)
	}
	if colocN > 0 {
		s.Colocation = float64(coloc) / float64(colocN)
	}
	if pmN > 0 {
		s.PrefixRate = float64(pm) / float64(pmN)
	}
	if repPrompt > 0 {
		s.ReportedCache = float64(repCached) / float64(repPrompt)
	}
	var dp, dc float64
	for _, d := range m.Deltas {
		if d.Valid {
			dp += d.PromptTokens
			dc += d.CachedTokens
		}
	}
	if dp > 0 {
		s.EngineCached = dc / dp
	}
	if okCount > 0 && len(s.NodeCounts) > 0 {
		least := okCount
		for _, e := range m.Deltas {
			if c := s.NodeCounts[e.Name]; c < least {
				least = c
			}
		}
		s.MinNodeShare = float64(least) / float64(okCount)
	}

	if s.ErrorFrac > maxErrorFrac {
		s.Flags = append(s.Flags, flagHighErrors)
	}
	if s.MinNodeShare < minNodeShare {
		s.Flags = append(s.Flags, flagCollapsed)
	}
	if m.ExplainReq > 0 && float64(m.ExplainOK)/float64(m.ExplainReq) < minExplainCoverage {
		s.Flags = append(s.Flags, flagLowExplain)
	}
	if s.EngineCached < 0 {
		s.Flags = append(s.Flags, flagNoEngineSignal)
	}
	// JSON cannot carry NaN; -1 already means "not observed" for these fields.
	for _, p := range []*float64{&s.TTFTp50, &s.TTFTp95, &s.TTFTp99, &s.ITLMean, &s.TotalP95} {
		if math.IsNaN(*p) {
			*p = -1
		}
	}
	return s
}

// metric describes one comparable quantity of a run summary.
type metric struct {
	Name        string
	Get         func(runSummary) float64
	LowerBetter bool
}

var comparedMetrics = []metric{
	{"ttft_p50_ms", func(s runSummary) float64 { return s.TTFTp50 }, true},
	{"ttft_p95_ms", func(s runSummary) float64 { return s.TTFTp95 }, true},
	{"ttft_p99_ms", func(s runSummary) float64 { return s.TTFTp99 }, true},
	{"itl_mean_ms", func(s runSummary) float64 { return s.ITLMean }, true},
	{"error_frac", func(s runSummary) float64 { return s.ErrorFrac }, true},
	{"throughput_rps", func(s runSummary) float64 { return s.Throughput }, false},
	{"engine_cached_frac", func(s runSummary) float64 { return s.EngineCached }, false},
	{"min_node_share", func(s runSummary) float64 { return s.MinNodeShare }, false},
}

// pairedResult compares one arm with the baseline arm over matched
// repetitions of the same workload.
type pairedResult struct {
	Workload    string    `json:"workload"`
	Arm         string    `json:"arm"`
	Baseline    string    `json:"baseline"`
	Metric      string    `json:"metric"`
	LowerBetter bool      `json:"lower_better"`
	Pairs       int       `json:"pairs"`
	Deltas      []float64 `json:"deltas"`
	MedianDelta float64   `json:"median_delta"`
	MedianRel   float64   `json:"median_relative_delta"`
	CILow       float64   `json:"ci95_low"`
	CIHigh      float64   `json:"ci95_high"`
	Improved    int       `json:"reps_improved"`
	Worsened    int       `json:"reps_worsened"`
	SignP       float64   `json:"sign_test_p"`
}

// comparePaired builds paired comparisons for every non-baseline arm. Pairs
// where either run produced no value for the metric are skipped.
func comparePaired(sums []runSummary, baseline string, seed int64) []pairedResult {
	type key struct {
		w, a string
		rep  int
	}
	by := map[key]runSummary{}
	arms, wls := map[string]bool{}, map[string]bool{}
	maxRep := 0
	for _, s := range sums {
		by[key{s.Workload, s.Arm, s.Rep}] = s
		arms[s.Arm], wls[s.Workload] = true, true
		if s.Rep > maxRep {
			maxRep = s.Rep
		}
	}
	var out []pairedResult
	for _, w := range sortedKeys(wls) {
		for _, a := range sortedKeys(arms) {
			if a == baseline {
				continue
			}
			for _, m := range comparedMetrics {
				pr := pairedResult{Workload: w, Arm: a, Baseline: baseline, Metric: m.Name, LowerBetter: m.LowerBetter}
				var rel []float64
				for rep := 1; rep <= maxRep; rep++ {
					x, okx := by[key{w, a, rep}]
					b, okb := by[key{w, baseline, rep}]
					if !okx || !okb {
						continue
					}
					xv, bv := m.Get(x), m.Get(b)
					if math.IsNaN(xv) || math.IsNaN(bv) || xv < 0 || bv < 0 {
						continue
					}
					d := xv - bv
					pr.Deltas = append(pr.Deltas, d)
					if bv != 0 {
						rel = append(rel, d/bv)
					}
					switch {
					case d == 0:
					case (d < 0) == m.LowerBetter:
						pr.Improved++
					default:
						pr.Worsened++
					}
				}
				pr.Pairs = len(pr.Deltas)
				if pr.Pairs == 0 {
					continue
				}
				pr.MedianDelta = median(pr.Deltas)
				if len(rel) > 0 {
					pr.MedianRel = median(rel)
				}
				pr.CILow, pr.CIHigh = bootstrapMedianCI(pr.Deltas, seed)
				pr.SignP = signTestP(pr.Improved, pr.Worsened)
				out = append(out, pr)
			}
		}
	}
	return out
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// bootstrapMedianCI returns a 95% percentile-bootstrap interval for the median
// of vals. With ten pairs it is a coarse indication, not a precise bound.
func bootstrapMedianCI(vals []float64, seed int64) (float64, float64) {
	if len(vals) == 0 {
		return math.NaN(), math.NaN()
	}
	rng := rand.New(rand.NewSource(seed))
	const resamples = 2000
	meds := make([]float64, resamples)
	buf := make([]float64, len(vals))
	for i := range meds {
		for j := range buf {
			buf[j] = vals[rng.Intn(len(vals))]
		}
		meds[i] = median(buf)
	}
	return percentile(meds, 0.025), percentile(meds, 0.975)
}

// signTestP is the two-sided exact sign-test p-value for k "improved" against
// m "worsened" pairs (ties dropped): the chance of a split at least this
// lopsided if improvement and regression were equally likely.
func signTestP(improved, worsened int) float64 {
	n := improved + worsened
	if n == 0 {
		return 1
	}
	k := improved
	if worsened < k {
		k = worsened
	}
	var tail float64
	for i := 0; i <= k; i++ {
		tail += binomPMF(n, i)
	}
	return math.Min(1, 2*tail)
}

func binomPMF(n, k int) float64 {
	lg := func(x int) float64 { v, _ := math.Lgamma(float64(x + 1)); return v }
	return math.Exp(lg(n) - lg(k) - lg(n-k) + float64(n)*math.Log(0.5))
}
