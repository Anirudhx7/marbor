package ha

// Monitor is a passive peer-health observer. It polls the /health endpoint of
// each configured peer and records reachability so operators running more than
// one mesh instance behind their own TCP load balancer can observe peer status.
//
// It is NOT high availability. There is no leader election, no shared state, no
// failover, and no coordination between instances. ollama-mesh is a
// single-instance control plane; this module only reports whether peers are
// reachable. Distributing or failing over traffic, if desired, is entirely the
// external load balancer's responsibility.

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/ollama-mesh/ollama-mesh/internal/config"
)

// Monitor polls each configured peer's /health endpoint on a fixed interval
// and logs reachability transitions. It is safe for concurrent use.
type Monitor struct {
	peers    []string
	client   *http.Client
	interval time.Duration
	mu       sync.RWMutex
	statuses map[string]bool // peer URL -> last known reachable
}

// New creates a Monitor from an HAConfig. Call Start to begin polling.
func New(cfg config.HAConfig) *Monitor {
	return &Monitor{
		peers:    cfg.Peers,
		client:   &http.Client{Timeout: time.Duration(cfg.PeerTimeoutMs) * time.Millisecond},
		interval: time.Duration(cfg.HeartbeatIntervalMs) * time.Millisecond,
		statuses: make(map[string]bool, len(cfg.Peers)),
	}
}

// Start runs the polling loop until ctx is cancelled. Call in a goroutine.
func (m *Monitor) Start(ctx context.Context) {
	ticker := time.NewTicker(m.interval)
	defer ticker.Stop()
	// Initial check immediately so statuses are populated before the first tick.
	m.checkAll()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.checkAll()
		}
	}
}

func (m *Monitor) checkAll() {
	for _, peer := range m.peers {
		reachable := m.probe(peer)
		m.mu.Lock()
		prev, seen := m.statuses[peer]
		changed := !seen || prev != reachable
		m.statuses[peer] = reachable
		m.mu.Unlock()
		if changed {
			if reachable {
				log.Printf("[peer-monitor] peer %s reachable", peer)
			} else {
				log.Printf("[peer-monitor] peer %s UNREACHABLE", peer)
			}
		}
	}
}

func (m *Monitor) probe(peer string) bool {
	resp, err := m.client.Get(peer + "/health")
	if err != nil {
		return false
	}
	_ = resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// PeerStatuses returns a snapshot of last known reachability per peer URL.
func (m *Monitor) PeerStatuses() map[string]bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make(map[string]bool, len(m.statuses))
	for k, v := range m.statuses {
		out[k] = v
	}
	return out
}

// allPeersUp returns true if all configured peers are reachable.
// Returns true (vacuously) when no peers are configured.
func (m *Monitor) allPeersUp() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, up := range m.statuses {
		if !up {
			return false
		}
	}
	return true
}

// PeerCount returns the number of configured peers.
// m.peers is immutable after construction  --  safe to read without a lock.
func (m *Monitor) PeerCount() int { return len(m.peers) }

// String returns a human-readable summary for startup logging.
func (m *Monitor) String() string {
	return fmt.Sprintf("peer-health monitor: %d peers, interval %s, timeout %s",
		len(m.peers), m.interval, m.client.Timeout)
}
