package store

import (
	"path/filepath"
	"testing"
	"time"
)

// queue_wait_ms and cloud_ms round-trip through request_log and audit_log, a
// row without them reads back nil (not 0), and reopening an already-migrated
// database is harmless.
func TestRequestTiming_RoundTripAndReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "t.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	q, c := int64(1500), int64(320)
	now := time.Now()
	if err := s.AppendRequest(RequestRecord{ID: "a", KeyName: "k", Model: "m", NodeName: "cloud:x", StatusCode: 200, LatencyMs: 1900, IsCloud: true, TS: now, RoutingReason: "saturated_sla_expired", QueueWaitMs: &q, CloudMs: &c}); err != nil {
		t.Fatal(err)
	}
	if err := s.AppendRequest(RequestRecord{ID: "b", KeyName: "k", Model: "m", NodeName: "n", StatusCode: 200, TS: now.Add(time.Second)}); err != nil {
		t.Fatal(err)
	}
	if err := s.AppendAuditLog(AuditEntry{Time: now, RequestID: "a", KeyName: "k", Model: "m", Node: "cloud:x", Status: "200", Cloud: true, RoutingReason: "saturated_sla_expired", QueueWaitMs: &q, CloudMs: &c}); err != nil {
		t.Fatal(err)
	}
	s.Close()

	s, err = Open(path) // reopen: every ALTER hits "duplicate column" and must be ignored
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s.Close()

	a, ok, err := s.GetRequest("a")
	if err != nil || !ok || a.QueueWaitMs == nil || a.CloudMs == nil || *a.QueueWaitMs != 1500 || *a.CloudMs != 320 {
		t.Fatalf("GetRequest a = %+v ok=%v err=%v, want queue 1500 cloud 320", a, ok, err)
	}
	b, _, _ := s.GetRequest("b")
	if b.QueueWaitMs != nil || b.CloudMs != nil {
		t.Errorf("row b timing = %v/%v, want nil", b.QueueWaitMs, b.CloudMs)
	}
	recs, err := s.LastRequests(10)
	if err != nil || len(recs) != 2 || recs[0].QueueWaitMs == nil || recs[1].QueueWaitMs != nil {
		t.Fatalf("LastRequests = %+v err=%v", recs, err)
	}
	ents, err := s.QueryAuditLog(AuditQuery{Limit: 10})
	if err != nil || len(ents) != 1 || ents[0].CloudMs == nil || *ents[0].CloudMs != 320 || ents[0].RoutingReason != "saturated_sla_expired" {
		t.Fatalf("audit = %+v err=%v", ents, err)
	}
}
