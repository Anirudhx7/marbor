package router

// host_evidence.go - the latest host-wide snapshot of what each polled agent
// reported about multi-host launches. NodeState only keeps the port-matched
// slice of a host's report, which is right for placement but throws away
// exactly what replica correlation needs: the reports of headless workers, the
// host's own addresses and its capability list. This file keeps that raw
// snapshot per host, in memory only, behind its own lock so it never nests
// with a node lock or the router lock.

import (
	"log"
	"strings"
	"time"

	"github.com/Anirudhx7/marbor/internal/marboragent"
)

const (
	// maxHostEvidenceAddrs and maxHostEvidenceDeployments bound how much of one
	// host's report is retained. They sit far above any realistic host (a few
	// interfaces, a handful of runtimes); only a runaway or hostile agent
	// reaches them.
	maxHostEvidenceAddrs       = 64
	maxHostEvidenceDeployments = 64

	// hostLogInterval is the minimum gap between repeats of the same
	// per-host agent warning.
	hostLogInterval = 10 * time.Minute
)

// hostLogKeyPrefixes lists every rate-limit key prefix; a key is prefix+host.
var hostLogKeyPrefixes = []string{"truncate:", "oversize:", "read:"}

// lastResortLogPrefix keys the last-resort routing warning by head name and
// host. It is not in hostLogKeyPrefixes: the head's host often has no polled
// agent, so pruning by poll group would reset the limit every pass. These keys
// are only removed once they are older than hostLogInterval.
const lastResortLogPrefix = "lastresort:"

// HostEvidence is one agent's latest host-level report. Host is the same key
// pollAgentHosts groups nodes by (the raw NodeState.Host string).
type HostEvidence struct {
	Host         string
	Hostname     string
	Addrs        []string
	Capabilities []string
	Deployments  []marboragent.DeploymentReport
	PolledAt     time.Time
}

// RecordHostEvidence stores a deep copy of ev as the latest snapshot for
// ev.Host, replacing any earlier one. It is called from the agent poll after a
// successful decode. It is exported only so tests in other packages (the admin
// API tests) can seed evidence without running a real agent poll; production
// code reaches it through the poll alone.
func (r *Router) RecordHostEvidence(ev HostEvidence) {
	if len(ev.Addrs) > maxHostEvidenceAddrs || len(ev.Deployments) > maxHostEvidenceDeployments {
		if r.allowHostLog("truncate:" + ev.Host) {
			log.Printf("router: agent on host %q reported %d addresses and %d deployments; retaining the first %d and %d, so replica detection may miss groups on this host",
				ev.Host, len(ev.Addrs), len(ev.Deployments), maxHostEvidenceAddrs, maxHostEvidenceDeployments)
		}
		ev.Addrs = ev.Addrs[:min(len(ev.Addrs), maxHostEvidenceAddrs)]
		ev.Deployments = ev.Deployments[:min(len(ev.Deployments), maxHostEvidenceDeployments)]
	}
	cp := HostEvidence{
		Host:         ev.Host,
		Hostname:     ev.Hostname,
		Addrs:        append([]string(nil), ev.Addrs...),
		Capabilities: append([]string(nil), ev.Capabilities...),
		Deployments:  copyDeployments(ev.Deployments),
		PolledAt:     ev.PolledAt,
	}
	r.hostEvidenceMu.Lock()
	if r.hostEvidence == nil {
		r.hostEvidence = make(map[string]HostEvidence)
	}
	r.hostEvidence[ev.Host] = cp
	r.hostEvidenceMu.Unlock()
}

// allowHostLog reports whether the warning for key may be logged now, and
// if so records the time. A key logs at most once per hostLogInterval. The
// rate-limit map shares hostEvidenceMu on purpose: the critical section is a
// single map lookup and write, so a separate mutex would add nothing.
func (r *Router) allowHostLog(key string) bool {
	now := time.Now()
	r.hostEvidenceMu.Lock()
	defer r.hostEvidenceMu.Unlock()
	if r.hostLogAt == nil {
		r.hostLogAt = make(map[string]time.Time)
	}
	if last, ok := r.hostLogAt[key]; ok && now.Sub(last) < hostLogInterval {
		return false
	}
	r.hostLogAt[key] = now
	return true
}

// DropHostEvidence forgets the snapshot for host. Exported for the same
// cross-package test reason as RecordHostEvidence.
func (r *Router) DropHostEvidence(host string) {
	r.hostEvidenceMu.Lock()
	delete(r.hostEvidence, host)
	for _, prefix := range hostLogKeyPrefixes {
		delete(r.hostLogAt, prefix+host)
	}
	r.hostEvidenceMu.Unlock()
}

// dropHostEvidenceOnly forgets the snapshot for host but leaves the log
// rate-limit keys alone, so a warning that fires on every poll of a
// persistent condition (an oversize reply) stays rate-limited.
func (r *Router) dropHostEvidenceOnly(host string) {
	r.hostEvidenceMu.Lock()
	delete(r.hostEvidence, host)
	r.hostEvidenceMu.Unlock()
}

// dropHostEvidenceNotIn forgets every snapshot whose host is not a key of keep.
// Run after each agent poll pass so a host that lost its last node row, or
// whose agent was removed or disabled, stops feeding address identity.
func (r *Router) dropHostEvidenceNotIn(keep map[string][]*NodeState) {
	r.hostEvidenceMu.Lock()
	for host := range r.hostEvidence {
		if _, ok := keep[host]; !ok {
			delete(r.hostEvidence, host)
		}
	}
	now := time.Now()
	for key, at := range r.hostLogAt {
		if strings.HasPrefix(key, lastResortLogPrefix) {
			if now.Sub(at) >= hostLogInterval {
				delete(r.hostLogAt, key)
			}
			continue
		}
		for _, prefix := range hostLogKeyPrefixes {
			if host, ok := strings.CutPrefix(key, prefix); ok {
				if _, kept := keep[host]; !kept {
					delete(r.hostLogAt, key)
				}
			}
		}
	}
	r.hostEvidenceMu.Unlock()
}

// snapshotHostEvidence returns the current snapshots. Stored values are never
// mutated after RecordHostEvidence builds them (a record replaces the whole
// entry), so sharing their slices with a read-only caller is safe.
func (r *Router) snapshotHostEvidence() map[string]HostEvidence {
	r.hostEvidenceMu.RLock()
	defer r.hostEvidenceMu.RUnlock()
	out := make(map[string]HostEvidence, len(r.hostEvidence))
	for h, ev := range r.hostEvidence {
		out[h] = ev
	}
	return out
}

func copyDeployments(in []marboragent.DeploymentReport) []marboragent.DeploymentReport {
	if in == nil {
		return nil
	}
	out := make([]marboragent.DeploymentReport, len(in))
	for i, d := range in {
		cp := d
		cp.GPUGroup = append([]int(nil), d.GPUGroup...)
		cp.GPUScope = copyGPUScope(d.GPUScope)
		if d.Parallelism != nil {
			par := *d.Parallelism
			cp.Parallelism = &par
		}
		if d.Caps != nil {
			caps := *d.Caps
			cp.Caps = &caps
		}
		cp.Topology = copyTopology(d.Topology)
		out[i] = cp
	}
	return out
}

func copyTopology(t *marboragent.Topology) *marboragent.Topology {
	if t == nil {
		return nil
	}
	cp := *t
	cp.NNodes = copyIntPtr(t.NNodes)
	cp.NodeRank = copyIntPtr(t.NodeRank)
	cp.MasterPort = copyIntPtr(t.MasterPort)
	cp.RPCServers = append([]string(nil), t.RPCServers...)
	cp.Evidence = append([]string(nil), t.Evidence...)
	return &cp
}

func copyIntPtr(p *int) *int {
	if p == nil {
		return nil
	}
	v := *p
	return &v
}
