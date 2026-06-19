package ha

import (
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/ollama-mesh/ollama-mesh/internal/config"
)

const testInterval = 20 * time.Millisecond

func newMonitor(peers []string, intervalMs, timeoutMs int) *Monitor {
	return New(config.HAConfig{
		Enabled:             true,
		Peers:               peers,
		HeartbeatIntervalMs: intervalMs,
		PeerTimeoutMs:       timeoutMs,
	})
}

func TestMonitor_PeerReachable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	m := newMonitor([]string{srv.URL}, 20, 500)
	m.checkAll() // deterministic — no goroutine/timer needed

	statuses := m.PeerStatuses()
	if !statuses[srv.URL] {
		t.Errorf("expected peer %s to be reachable, got unreachable", srv.URL)
	}
	if !m.AllPeersUp() {
		t.Error("AllPeersUp() should return true when all peers respond 200")
	}
}

func TestMonitor_PeerUnreachable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	m := newMonitor([]string{srv.URL}, 20, 500)
	m.checkAll()

	statuses := m.PeerStatuses()
	if statuses[srv.URL] {
		t.Errorf("expected peer %s to be unreachable (503), got reachable", srv.URL)
	}
	if m.AllPeersUp() {
		t.Error("AllPeersUp() should return false when a peer responds non-200")
	}
}

func TestMonitor_MultiPeer(t *testing.T) {
	healthy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer healthy.Close()

	unhealthy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer unhealthy.Close()

	m := newMonitor([]string{healthy.URL, unhealthy.URL}, 20, 500)
	m.checkAll()

	statuses := m.PeerStatuses()
	if !statuses[healthy.URL] {
		t.Errorf("expected healthy peer %s to be reachable", healthy.URL)
	}
	if statuses[unhealthy.URL] {
		t.Errorf("expected unhealthy peer %s to be unreachable", unhealthy.URL)
	}
	if m.AllPeersUp() {
		t.Error("AllPeersUp() should return false when at least one peer is down")
	}
	if m.PeerCount() != 2 {
		t.Errorf("PeerCount() = %d, want 2", m.PeerCount())
	}
}

func TestAllPeersUp_NoPeers(t *testing.T) {
	m := newMonitor(nil, 20, 500)
	// No Start needed — vacuous truth requires no polling.
	if !m.AllPeersUp() {
		t.Error("AllPeersUp() should return true (vacuously) when no peers are configured")
	}
}

func TestProbeSetsInitialStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	m := newMonitor([]string{srv.URL}, 20, 500)
	// Call checkAll directly — no goroutine, no ticker, deterministic.
	m.checkAll()

	statuses := m.PeerStatuses()
	if !statuses[srv.URL] {
		t.Errorf("checkAll() should set initial status for %s to reachable", srv.URL)
	}
}

func TestMonitor_String(t *testing.T) {
	m := newMonitor([]string{"http://peer1:8080", "http://peer2:8080"}, 5000, 3000)
	s := m.String()
	if s == "" {
		t.Error("String() returned empty string")
	}
}

func TestMonitor_PeerConnectionRefused(t *testing.T) {
	// Bind a listener, capture its address, close it — guarantees no listener on that port.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()

	peerURL := "http://" + addr
	m := newMonitor([]string{peerURL}, 20, 100)
	m.checkAll()

	statuses := m.PeerStatuses()
	if statuses[peerURL] {
		t.Error("connection-refused peer should be unreachable")
	}
}
