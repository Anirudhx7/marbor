package admin

import (
	"testing"
	"time"

	"github.com/Anirudhx7/marbor/internal/config"
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

// probeShape returns a record with every field of a genuine probe key, so each
// negative case below changes exactly one field and fails only on that field.
func probeShape() store.KeyRecord {
	return store.KeyRecord{
		Name:      benchmarkKeyPrefix + "n-m-1-2",
		Models:    []string{"m"},
		ExpiresAt: time.Now().Add(time.Hour).Format(time.RFC3339),
		LocalOnly: true,
	}
}

func TestIsEphemeralProbeKey(t *testing.T) {
	for _, prefix := range []string{benchmarkKeyPrefix, pullVerifyKeyPrefix} {
		rec := probeShape()
		rec.Name = prefix + "n-m-1"
		if !IsEphemeralProbeKey(rec) {
			t.Errorf("probe key with prefix %q not recognised", prefix)
		}
	}
	for _, tc := range []struct {
		name   string
		mutate func(*store.KeyRecord)
	}{
		{"other name", func(r *store.KeyRecord) { r.Name = "team" }},
		{"prefix not at start", func(r *store.KeyRecord) { r.Name = "my-benchmark-x" }},
		{"not local", func(r *store.KeyRecord) { r.LocalOnly = false }},
		{"revoked", func(r *store.KeyRecord) { r.Revoked = true }},
		{"no expiry", func(r *store.KeyRecord) { r.ExpiresAt = "" }},
		{"no models", func(r *store.KeyRecord) { r.Models = nil }},
		{"two models", func(r *store.KeyRecord) { r.Models = []string{"a", "b"} }},
		{"rate limited", func(r *store.KeyRecord) { r.RateLimit = 5 }},
		{"daily limit", func(r *store.KeyRecord) { r.DailyLimit = 5 }},
		{"monthly limit", func(r *store.KeyRecord) { r.MonthlyLimit = 5 }},
		{"daily usd cap", func(r *store.KeyRecord) { r.DailyUsdCap = 1 }},
		{"monthly usd cap", func(r *store.KeyRecord) { r.MonthlyUsdCap = 1 }},
		{"allow local degradation", func(r *store.KeyRecord) { r.AllowLocalDegradation = true }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := probeShape()
			tc.mutate(&rec)
			if IsEphemeralProbeKey(rec) {
				t.Errorf("operator key treated as probe key: %+v", rec)
			}
		})
	}
	if probeShape().Name[:len("benchmark-")] != "benchmark-" {
		t.Error("probeShape no longer starts with the literal benchmark- prefix")
	}
}

// The predicate must match the keys the two probe creation sites really
// build, or the boot-time sweep silently stops cleaning up after a crash. The
// keys go through the real persistKey and are read back from the store.
func TestIsEphemeralProbeKey_MatchesKeysBuiltByProbeSites(t *testing.T) {
	s, _, fs := newFlakyKeyServer(t)
	for _, k := range []config.KeyConfig{
		benchmarkProbeKey(benchmarkKeyPrefix+"n1-m-1-1", "sk-b", "m", time.Hour),
		pullVerifyProbeKey(pullVerifyKeyPrefix+"n1-m-1", "sk-p", "m"),
	} {
		if err := s.persistKey(k); err != nil {
			t.Fatalf("persistKey(%s): %v", k.Name, err)
		}
	}
	rows, err := fs.AllKeys()
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("stored %d rows, want 2", len(rows))
	}
	for _, r := range rows {
		if !IsEphemeralProbeKey(r) {
			t.Errorf("stored probe key %+v not matched by IsEphemeralProbeKey", r)
		}
	}
}
