package router

import (
	"bytes"
	"fmt"
	"log"
	"strings"
	"testing"

	"github.com/Anirudhx7/marbor/internal/config"
	"github.com/Anirudhx7/marbor/internal/marboragent"
)

func TestRecordHostEvidenceCapsAddrsAndDeploymentsAndLogsOnce(t *testing.T) {
	r := New(config.RoutingConfig{Strategy: "warm-first", PollIntervalMs: 2000}, nil, nil)

	addrs := make([]string, maxHostEvidenceAddrs+36)
	for i := range addrs {
		addrs[i] = fmt.Sprintf("10.0.%d.%d", i/200, i%200)
	}
	deps := make([]marboragent.DeploymentReport, maxHostEvidenceDeployments+6)
	for i := range deps {
		deps[i] = marboragent.DeploymentReport{Port: 8000 + i}
	}

	var logBuf bytes.Buffer
	oldOutput := log.Writer()
	log.SetOutput(&logBuf)
	defer log.SetOutput(oldOutput)

	r.RecordHostEvidence(HostEvidence{Host: "h1", Addrs: addrs, Deployments: deps})
	r.RecordHostEvidence(HostEvidence{Host: "h1", Addrs: addrs, Deployments: deps})

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
