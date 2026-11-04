package admin

import (
	"testing"
	"time"
)

// worstCaseBenchmarkRun recomputes the longest a run can take from the same
// timeout variables the job uses: N+1 evicts (initial plus one before each
// later cold sample) and 2N samples, each sample bounded by its timeout.
func worstCaseBenchmarkRun(n int) time.Duration {
	evict := nodeUnloadModelTimeout + 2*time.Second
	return time.Duration(n+1)*evict + time.Duration(2*n)*benchmarkSampleTimeout
}

func TestBenchmarkKeyTTLFor_CoversWorstCase(t *testing.T) {
	for _, n := range []int{1, 10, 50} {
		if got, need := benchmarkKeyTTLFor(n), worstCaseBenchmarkRun(n); got < need {
			t.Errorf("n=%d: ttl %v is below worst-case run %v", n, got, need)
		}
	}
}

func TestBenchmarkKeyTTLFor_MonotonicAndFloor(t *testing.T) {
	prev := time.Duration(0)
	for n := 1; n <= 50; n++ {
		got := benchmarkKeyTTLFor(n)
		if got < prev {
			t.Fatalf("n=%d: ttl %v less than n=%d ttl %v", n, got, n-1, prev)
		}
		prev = got
	}
	if benchmarkKeyTTLFor(50) <= benchmarkKeyTTLFor(1) {
		t.Errorf("ttl does not grow with n")
	}
	if benchmarkKeyTTLFor(1) < 15*time.Minute {
		t.Errorf("ttl for n=1 is below the previous 15m window")
	}
}

func TestBenchmarkKeyTTLFor_DefaultNNotExpiredAt16Minutes(t *testing.T) {
	const defaultN = 10
	expiresAt := time.Now().Add(benchmarkKeyTTLFor(defaultN))
	if !time.Now().Add(16 * time.Minute).Before(expiresAt) {
		t.Errorf("key for default n=%d would be expired at 16 minutes", defaultN)
	}
}
