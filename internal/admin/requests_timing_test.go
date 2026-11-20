package admin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Anirudhx7/marbor/internal/config"
	"github.com/Anirudhx7/marbor/internal/router"
	"github.com/Anirudhx7/marbor/internal/store"
)

// GET /admin/requests carries the measured queue wait and cloud time for a
// cloud-routed request and omits them for one that was not measured.
func TestHandleRequests_CarriesCloudTiming(t *testing.T) {
	r := router.New(config.RoutingConfig{}, nil, nil)
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	srv := NewServer(r, nil, config.Config{}, st)
	defer srv.Shutdown()

	q, c := int64(1500), int64(0)
	dec := &router.RoutingDecision{Node: "cloud:x", Reason: "saturated_sla_expired"}
	srv.LogRequestTimed(RequestLogParams{RequestID: "req-a", KeyName: "k", SourceIP: "1.2.3.4", Model: "m", Node: "cloud:x", Status: "cloud", HTTPStatus: 200, LatencyMs: 1800, Decision: dec}, RequestTiming{QueueWaitMs: &q, CloudMs: &c})
	srv.LogRequest(RequestLogParams{RequestID: "req-b", KeyName: "k", SourceIP: "1.2.3.4", Model: "m", Node: "gpu-0", Status: "warm", HTTPStatus: 200, LatencyMs: 50})

	w := httptest.NewRecorder()
	srv.handleRequests(w, httptest.NewRequest(http.MethodGet, "/admin/requests", nil))
	var out []map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil || len(out) != 2 {
		t.Fatalf("decode: %v body %s", err, w.Body.String())
	}
	local, cloud := out[0], out[1] // newest first
	if cloud["queueWaitMs"] != float64(1500) || cloud["cloudMs"] != float64(0) {
		t.Errorf("cloud row timing = %v/%v, want 1500/0 (a real 0 is kept)", cloud["queueWaitMs"], cloud["cloudMs"])
	}
	if cloud["routingReason"] != "saturated_sla_expired" {
		t.Errorf("routingReason = %v", cloud["routingReason"])
	}
	if _, ok := local["queueWaitMs"]; ok {
		t.Errorf("local row has queueWaitMs, want it omitted")
	}
}
