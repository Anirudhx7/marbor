package nodeagent

import (
	"context"
	"testing"
	"time"
)

func TestCollectorSnapshotBeforeSeedIsUnknown(t *testing.T) {
	c := NewCollector("v-test")
	snap := c.Snapshot()
	if !snap.LastUpdated.IsZero() {
		t.Errorf("LastUpdated = %v, want zero time before Seed has run", snap.LastUpdated)
	}
	if snap.GPU != nil || snap.Host != nil {
		t.Errorf("expected nil GPU/Host before any collection, got GPU=%v Host=%v", snap.GPU, snap.Host)
	}
	if snap.SchemaVersion != SchemaVersion || snap.AgentVersion != "v-test" {
		t.Errorf("schema_version/agent_version should still be set: %+v", snap)
	}
}

func TestCollectorSeedPopulatesSnapshot(t *testing.T) {
	c := NewCollector("v-test")
	c.Seed()
	snap := c.Snapshot()
	if snap.LastUpdated.IsZero() {
		t.Error("LastUpdated should be set after Seed")
	}
	if time.Since(snap.LastUpdated) > 5*time.Second {
		t.Errorf("LastUpdated = %v, expected close to now", snap.LastUpdated)
	}
}

// TestCollectorStartRefreshesInBackground verifies the background loop
// advances LastUpdated on its own tick, independent of any request - this is
// the behavior that replaces per-request nvidia-smi execution.
func TestCollectorStartRefreshesInBackground(t *testing.T) {
	c := NewCollector("v-test")
	c.Seed()
	first := c.Snapshot().LastUpdated

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.Start(ctx, 15*time.Millisecond)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if c.Snapshot().LastUpdated.After(first) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("LastUpdated never advanced after Start began ticking")
}

// TestCollectorStartStopsOnContextCancel verifies the background goroutine
// actually stops refreshing once its context is canceled, rather than
// leaking a ticker that keeps mutating the cache forever.
func TestCollectorStartStopsOnContextCancel(t *testing.T) {
	c := NewCollector("v-test")
	c.Seed()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		c.Start(ctx, 10*time.Millisecond)
		close(done)
	}()

	time.Sleep(30 * time.Millisecond) // let it tick a couple times
	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Start did not return after context cancellation")
	}
}
