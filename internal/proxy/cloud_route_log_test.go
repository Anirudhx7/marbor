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
			return recs[0]
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
		{"queue disabled", fastPathOpts{state: "saturated", timeoutMs: 0, slaMs: 0}, CloudReasonQueueFull, 0},
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
	if row.NodeName != "none" || row.QueueWaitMs == nil {
		t.Errorf("node=%q queue_wait_ms=%v, want node none and a measured wait", row.NodeName, row.QueueWaitMs)
	}
	if row.CloudMs != nil {
		t.Errorf("cloud_ms = %v, want nil: nothing was sent to cloud", *row.CloudMs)
	}
}

// A local node that was chosen and then failed sends the request to cloud with
// its own reason, and the failed node's exclusion list is not carried over.
func TestProxy_LocalUpstreamFailed_Reason(t *testing.T) {
	e := newFastPathEnv(t, fastPathOpts{timeoutMs: 5000, slaMs: 2000})
	e.localSrv.Close() // node still looks healthy, so it is chosen and the dial fails
	rec, _ := e.do()
	if rec.Code != http.StatusOK || e.cloudHits.Load() != 1 {
		t.Fatalf("status=%d cloudHits=%d, want cloud after the local failure", rec.Code, e.cloudHits.Load())
	}
	row := e.lastRow(t)
	if row.RoutingReason != CloudReasonLocalUpstreamFailed {
		t.Errorf("routing_reason = %q, want %q", row.RoutingReason, CloudReasonLocalUpstreamFailed)
	}
}

// The split never exceeds the total, whatever the clock reads were.
func TestCloudRoute_SplitBounded(t *testing.T) {
	c := &cloudRoute{waited: 900 * time.Millisecond}
	q, cl := c.split(1000*time.Millisecond, 400*time.Millisecond)
	if q != 900 || cl != 100 {
		t.Errorf("split = %d/%d, want 900/100 (cloud clamped to the remainder)", q, cl)
	}
	q, cl = c.split(500*time.Millisecond, 400*time.Millisecond)
	if q != 500 || cl != 0 {
		t.Errorf("split = %d/%d, want 500/0 (wait clamped to the total)", q, cl)
	}
}
