package admin

import (
	"strings"
	"testing"
	"time"

	"github.com/Anirudhx7/marbor/internal/store"
)

const (
	testSample = 5 * time.Minute
	testUnload = 30 * time.Second
)

// worstCaseByCounting walks the job's steps (see runBenchmarkJob) and sums
// each call's own bound, rather than re-using the production formula: one
// initial evict, then per cold sample an evict (all but the first) plus the
// sample, then n warm samples.
func worstCaseByCounting(n int) time.Duration {
	evicts, samples := 1, 0
	for i := 0; i < n; i++ {
		if i > 0 {
			evicts++
		}
		samples++ // cold
	}
	samples += n // warm
	return time.Duration(evicts)*(testUnload+2*time.Second) + time.Duration(samples)*testSample
}

func TestBenchmarkKeyTTLFor_CoversCountedWorstCase(t *testing.T) {
	for _, n := range []int{1, 2, 10, 50} {
		if got, need := benchmarkKeyTTLFor(n, testSample, testUnload), worstCaseByCounting(n); got < need {
			t.Errorf("n=%d: ttl %v is below counted worst-case run %v", n, got, need)
		}
	}
}

func TestBenchmarkKeyTTLFor_PinnedValues(t *testing.T) {
	// n=50: 51 evicts x 32s + 100 samples x 5m + 2m margin = 8h49m12s.
	if got, want := benchmarkKeyTTLFor(benchmarkMaxN, testSample, testUnload), 8*time.Hour+49*time.Minute+12*time.Second; got != want {
		t.Errorf("n=50 ttl = %v, want %v", got, want)
	}
	if benchmarkKeyTTLFor(benchmarkMaxN, testSample, testUnload) > benchmarkKeyTTLMax {
		t.Errorf("n=50 ttl exceeds the hard max")
	}
	// n=10 (the default): 11 x 32s + 20 x 5m + 2m = 1h47m52s.
	if got, want := benchmarkKeyTTLFor(10, testSample, testUnload), time.Hour+47*time.Minute+52*time.Second; got != want {
		t.Errorf("n=10 ttl = %v, want %v", got, want)
	}
}

func TestBenchmarkKeyTTLFor_FloorAndCeiling(t *testing.T) {
	if got := benchmarkKeyTTLFor(1, time.Second, time.Second); got != benchmarkKeyTTLMin {
		t.Errorf("tiny run ttl = %v, want floor %v", got, benchmarkKeyTTLMin)
	}
	if got := benchmarkKeyTTLFor(benchmarkMaxN, time.Hour, time.Hour); got != benchmarkKeyTTLMax {
		t.Errorf("huge timeouts ttl = %v, want ceiling %v", got, benchmarkKeyTTLMax)
	}
	prev := time.Duration(0)
	for n := 1; n <= benchmarkMaxN; n++ {
		got := benchmarkKeyTTLFor(n, testSample, testUnload)
		if got < prev {
			t.Fatalf("n=%d: ttl %v shrank from %v", n, got, prev)
		}
		prev = got
	}
}

func TestIsEphemeralProbeKey(t *testing.T) {
	exp := time.Now().Add(time.Hour).Format(time.RFC3339)
	probe := store.KeyRecord{Name: "benchmark-n-m-1-2", Models: []string{"m"}, ExpiresAt: exp, LocalOnly: true}
	if !IsEphemeralProbeKey(probe) {
		t.Error("benchmark probe key not recognised")
	}
	pv := probe
	pv.Name = pullVerifyKeyPrefix + "n-m-1"
	if !IsEphemeralProbeKey(pv) {
		t.Error("pull-verify probe key not recognised")
	}
	for name, rec := range map[string]store.KeyRecord{
		"other name":    {Name: "team", Models: []string{"m"}, ExpiresAt: exp, LocalOnly: true},
		"not local":     {Name: "benchmark-x", Models: []string{"m"}, ExpiresAt: exp},
		"revoked":       {Name: "benchmark-x", Models: []string{"m"}, ExpiresAt: exp, LocalOnly: true, Revoked: true},
		"no expiry":     {Name: "benchmark-x", Models: []string{"m"}},
		"no models":     {Name: "benchmark-x", ExpiresAt: exp, LocalOnly: true},
		"two models":    {Name: "benchmark-x", Models: []string{"a", "b"}, ExpiresAt: exp},
		"rate limited":  {Name: "benchmark-x", Models: []string{"m"}, ExpiresAt: exp, RateLimit: 5},
		"daily usd cap": {Name: "benchmark-x", Models: []string{"m"}, ExpiresAt: exp, DailyUsdCap: 1},
	} {
		if IsEphemeralProbeKey(rec) {
			t.Errorf("%s: operator key treated as probe key", name)
		}
	}
	if !strings.HasPrefix(probe.Name, benchmarkKeyPrefix) {
		t.Error("prefix constant drifted")
	}
}
