package router

import (
	"bytes"
	"fmt"
	"log"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Anirudhx7/marbor/internal/config"
	"github.com/Anirudhx7/marbor/internal/marboragent"
)

// captureLogMu serialises tests that redirect the process-wide standard
// logger. It is held from captureLog until the test ends, so two such tests
// can never interleave their swap and restore of the writer, even if one of
// them calls t.Parallel. A test must therefore call captureLog at most once,
// and not from both a parent and its subtest.
var captureLogMu sync.Mutex

// captureLog redirects the standard logger into a buffer for the test and
// restores the previous writer when the test ends. It is not parallel-safe
// by nature (the logger is global), so it takes captureLogMu for the life of
// the test; tests using it run one at a time. The buffer is not synchronised:
// read it only after the code under test has finished (for example after the
// poll's wg.Wait).
func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	captureLogMu.Lock()
	var buf bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() {
		log.SetOutput(prev)
		captureLogMu.Unlock()
	})
	return &buf
}

func evidenceOfSize(host string, addrs, deps int) HostEvidence {
	ev := HostEvidence{Host: host}
	for i := 0; i < addrs; i++ {
		ev.Addrs = append(ev.Addrs, fmt.Sprintf("10.0.%d.%d", i/200, i%200))
	}
	for i := 0; i < deps; i++ {
		ev.Deployments = append(ev.Deployments, marboragent.DeploymentReport{Port: 8000 + i})
	}
	return ev
}

func TestRecordHostEvidenceCapsAddrsAndDeploymentsAndLogsOnce(t *testing.T) {
	r := New(config.RoutingConfig{Strategy: "warm-first", PollIntervalMs: 2000}, nil, nil)

	big := evidenceOfSize("h1", maxHostEvidenceAddrs+36, maxHostEvidenceDeployments+6)
	addrs, deps := big.Addrs, big.Deployments
	logBuf := captureLog(t)

	r.RecordHostEvidence(big)
	r.RecordHostEvidence(big)

	got := r.snapshotHostEvidence()["h1"]
	if len(got.Addrs) != maxHostEvidenceAddrs {
		t.Errorf("len(Addrs) = %d, want %d", len(got.Addrs), maxHostEvidenceAddrs)
	}
	if len(got.Deployments) != maxHostEvidenceDeployments {
		t.Errorf("len(Deployments) = %d, want %d", len(got.Deployments), maxHostEvidenceDeployments)
	}
	if got.Addrs[0] != addrs[0] || got.Deployments[0].Port != 8000 {
		t.Error("truncation must keep the leading entries unchanged")
	}
	if c := strings.Count(logBuf.String(), "retaining the first"); c != 1 {
		t.Errorf("truncation logged %d times across 2 records, want 1\n%s", c, logBuf.String())
	}

	// Under the caps: untouched, nothing logged.
	logBuf.Reset()
	r.RecordHostEvidence(HostEvidence{Host: "h2", Addrs: addrs[:3], Deployments: deps[:2]})
	got = r.snapshotHostEvidence()["h2"]
	if len(got.Addrs) != 3 || len(got.Deployments) != 2 {
		t.Errorf("under-cap evidence changed: %d addrs, %d deployments", len(got.Addrs), len(got.Deployments))
	}
	if logBuf.Len() != 0 {
		t.Errorf("under-cap record logged: %s", logBuf.String())
	}
}

// TestRecordHostEvidenceCapBoundary pins the off-by-one: exactly the cap is
// kept whole and silent, one over truncates and logs. Addrs and Deployments are
// checked independently (one at the boundary, the other under or over).
func TestRecordHostEvidenceCapBoundary(t *testing.T) {
	const a, d = maxHostEvidenceAddrs, maxHostEvidenceDeployments
	cases := []struct {
		name         string
		addrs, deps  int
		wantA, wantD int
		wantLog      bool
	}{
		{"both exactly at cap", a, d, a, d, false},
		{"addrs exactly at cap, deployments under", a, 2, a, 2, false},
		{"addrs under, deployments exactly at cap", 2, d, 2, d, false},
		{"both one over", a + 1, d + 1, a, d, true},
		{"addrs one over, deployments at cap", a + 1, d, a, d, true},
		{"addrs at cap, deployments one over", a, d + 1, a, d, true},
		{"addrs one over, deployments under", a + 1, 2, a, 2, true},
		{"addrs under, deployments one over", 2, d + 1, 2, d, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := New(config.RoutingConfig{Strategy: "warm-first", PollIntervalMs: 2000}, nil, nil)
			logBuf := captureLog(t)
			r.RecordHostEvidence(evidenceOfSize("h", tc.addrs, tc.deps))
			got := r.snapshotHostEvidence()["h"]
			if len(got.Addrs) != tc.wantA || len(got.Deployments) != tc.wantD {
				t.Errorf("kept %d addrs / %d deployments, want %d / %d", len(got.Addrs), len(got.Deployments), tc.wantA, tc.wantD)
			}
			if logged := strings.Contains(logBuf.String(), "retaining the first"); logged != tc.wantLog {
				t.Errorf("logged = %v, want %v\n%s", logged, tc.wantLog, logBuf.String())
			}
		})
	}
}

func TestAllowHostLogIsPerKeyAndRefiresAfterInterval(t *testing.T) {
	r := New(config.RoutingConfig{Strategy: "warm-first", PollIntervalMs: 2000}, nil, nil)

	// Two hosts each log once.
	if !r.allowHostLog("truncate:h1") || !r.allowHostLog("truncate:h2") {
		t.Fatal("first warning for each host must be allowed")
	}
	if r.allowHostLog("truncate:h1") || r.allowHostLog("truncate:h2") {
		t.Error("repeat within the interval must be suppressed per host")
	}

	// Different warning kinds on the same host do not suppress each other.
	if !r.allowHostLog("oversize:h1") {
		t.Error("oversize: must not be suppressed by truncate: for the same host")
	}
	if !r.allowHostLog("read:h1") {
		t.Error("read: must not be suppressed by other keys for the same host")
	}
	if r.allowHostLog("oversize:h1") {
		t.Error("oversize: repeat within the interval must be suppressed")
	}

	// After the interval the key fires again.
	r.hostEvidenceMu.Lock()
	r.hostLogAt["truncate:h1"] = time.Now().Add(-hostLogInterval - time.Second)
	r.hostEvidenceMu.Unlock()
	if !r.allowHostLog("truncate:h1") {
		t.Error("warning must fire again once the interval has elapsed")
	}
	if r.allowHostLog("truncate:h2") {
		t.Error("an unexpired key must stay suppressed")
	}
}

func TestDropHostEvidenceForgetsRateLimitKeys(t *testing.T) {
	r := New(config.RoutingConfig{Strategy: "warm-first", PollIntervalMs: 2000}, nil, nil)
	seed := func(hosts ...string) {
		for _, h := range hosts {
			for _, p := range hostLogKeyPrefixes {
				r.allowHostLog(p + h)
			}
		}
	}
	count := func() int {
		r.hostEvidenceMu.RLock()
		defer r.hostEvidenceMu.RUnlock()
		return len(r.hostLogAt)
	}

	seed("gone", "kept")
	r.DropHostEvidence("gone")
	if want := len(hostLogKeyPrefixes); count() != want {
		t.Errorf("after DropHostEvidence: %d keys, want %d (only the kept host)", count(), want)
	}
	if !r.allowHostLog("oversize:gone") {
		t.Error("a dropped host's warning must be allowed again")
	}

	seed("gone2")
	r.dropHostEvidenceNotIn(map[string][]*NodeState{"kept": nil})
	if want := len(hostLogKeyPrefixes); count() != want {
		t.Errorf("after dropHostEvidenceNotIn: %d keys, want %d (only the kept host)", count(), want)
	}
	if r.allowHostLog("truncate:kept") {
		t.Error("the kept host's key must survive the drop")
	}
}
