package nodeagent

import (
	"context"
	"sync/atomic"
	"time"
)

// Collector runs a background refresh loop that periodically collects a
// Telemetry snapshot (nvidia-smi + host stats) into an atomic pointer, so
// GET /telemetry and GET /metrics serve a cached reading instead of forking
// nvidia-smi and re-reading host stats on every single request. Mirrors
// internal/router/health.go's nvidiaCache/pollNvidiaAll pattern - a fixed
// background tick feeding a cache that request handlers only read - applied
// to the agent side of the same GPU-stats problem.
type Collector struct {
	version string
	snap    atomic.Pointer[Telemetry]
}

// NewCollector creates a Collector for the given agent_version string.
// Call Seed once before serving requests, then run Start in its own
// goroutine to keep the cache refreshed.
func NewCollector(version string) *Collector {
	return &Collector{version: version}
}

// Seed collects one snapshot synchronously and stores it. Intended to run
// once at startup, before the HTTP server begins accepting connections, so
// the first request never observes an empty/never-collected cache.
func (c *Collector) Seed() {
	c.refresh()
}

// Start blocks, refreshing the cached snapshot every interval until ctx is
// canceled. Run it in its own goroutine (go collector.Start(ctx, interval)).
func (c *Collector) Start(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.refresh()
		}
	}
}

// refresh collects a fresh Telemetry snapshot and atomically swaps it in.
func (c *Collector) refresh() {
	t := Collect(c.version)
	t.LastUpdated = time.Now().UTC()
	c.snap.Store(&t)
}

// Snapshot returns the most recently collected Telemetry. Before Seed has
// ever run, LastUpdated is the zero time and GPU/Host are nil - callers must
// treat that as "not collected yet," never as a real all-zero reading (R1).
// In normal operation (Seed called at startup before the server accepts
// requests) this branch is never observed by an HTTP client.
func (c *Collector) Snapshot() Telemetry {
	if p := c.snap.Load(); p != nil {
		return *p
	}
	return Telemetry{SchemaVersion: SchemaVersion, AgentVersion: c.version}
}
