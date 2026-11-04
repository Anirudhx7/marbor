package proxy

import (
	"net/http"
	"testing"
	"time"

	"github.com/Anirudhx7/marbor/internal/store"
)

// lastRow polls the request log until a row appears (logging is async).
func (e *fastPathEnv) lastRow(t *testing.T) store.RequestRecord {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if recs, err := e.st.LastRequests(5); err == nil && len(recs) > 0 {
			return recs[len(recs)-1]
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("no request_log row written")
	return store.RequestRecord{}
}

// Every cloud-routed request records a reason plus measured queue-wait and
// cloud time that never exceed the total latency.
func TestProxy_CloudRoute_RecordsReasonAndTiming(t *testing.T) {
	cases := []struct {
		name       string
		opts       fastPathOpts
		wantReason string
		minWaitMs  int64
	}{
		{"all nodes unavailable", fastPathOpts{state: "down", timeoutMs: 5000, slaMs: 2000}, CloudReasonAllNodesUnavailable, 0},
		{"no node serves model", fastPathOpts{runtime: "vllm", timeoutMs: 5000, slaMs: 2000}, CloudReasonNoNodeServesModel, 0},
		{"saturated sla expired", fastPathOpts{state: "saturated", timeoutMs: 5000, slaMs: 300}, CloudReasonSaturatedSLAExpired, 280},
		{"queue timeout", fastPathOpts{state: "saturated", timeoutMs: 300, slaMs: 0}, CloudReasonQueueTimeout, 280},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newFastPathEnv(t, tc.opts)
			rec, _ := e.do()
			if rec.Code != http.StatusOK || e.cloudHits.Load() != 1 {
				t.Fatalf("status=%d cloudHits=%d, want cloud", rec.Code, e.cloudHits.Load())
			}
			row := e.lastRow(t)
			if row.RoutingReason != tc.wantReason {
				t.Errorf("routing_reason = %q, want %q", row.RoutingReason, tc.wantReason)
			}
			if row.QueueWaitMs == nil || row.CloudMs == nil {
				t.Fatalf("queue_wait_ms=%v cloud_ms=%v, want both measured", row.QueueWaitMs, row.CloudMs)
			}
			if *row.QueueWaitMs < tc.minWaitMs {
				t.Errorf("queue_wait_ms = %d, want >= %d", *row.QueueWaitMs, tc.minWaitMs)
			}
			if *row.QueueWaitMs+*row.CloudMs > row.LatencyMs {
				t.Errorf("queue %d + cloud %d exceeds latency %d", *row.QueueWaitMs, *row.CloudMs, row.LatencyMs)
			}
			if row.RoutingDetail == "" {
				t.Error("routing_detail empty, want the synthesized decision for explain")
			}
		})
	}
}

// A locally served request carries no timing and no cloud reason.
func TestProxy_LocalRoute_NoCloudTiming(t *testing.T) {
	e := newFastPathEnv(t, fastPathOpts{timeoutMs: 5000, slaMs: 2000})
	if rec, _ := e.do(); rec.Code != http.StatusOK || e.localHits.Load() != 1 {
		t.Fatalf("status=%d local=%d, want served locally", rec.Code, e.localHits.Load())
	}
	row := e.lastRow(t)
	if row.QueueWaitMs != nil || row.CloudMs != nil {
		t.Errorf("queue_wait_ms=%v cloud_ms=%v, want nil for a local request", row.QueueWaitMs, row.CloudMs)
	}
}

// A local_only key refused with 503 still gets a request-log row saying why.
func TestProxy_LocalOnlyBlocked_LogsRow(t *testing.T) {
	e := newFastPathEnv(t, fastPathOpts{state: "down", localOnly: true, timeoutMs: 300, slaMs: 100})
	rec, _ := e.do()
	if rec.Code != http.StatusServiceUnavailable || e.cloudHits.Load() != 0 {
		t.Fatalf("status=%d cloudHits=%d, want 503 and no cloud", rec.Code, e.cloudHits.Load())
	}
	row := e.lastRow(t)
	if row.RoutingReason != CloudReasonLocalOnlyBlocked || row.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("row reason=%q status=%d, want local_only_blocked/503", row.RoutingReason, row.StatusCode)
	}
	if row.CloudMs != nil {
		t.Errorf("cloud_ms = %v, want nil: nothing was sent to cloud", *row.CloudMs)
	}
}
