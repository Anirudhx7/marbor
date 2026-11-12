package proxy

import (
	"net/http"
	"strings"
	"testing"
)

// Every exempt path keeps today's local routing while the list is unreadable:
// the local node answers and the refusal code never appears.
func TestProxy_CloudOnly_UnreadableList_AllExemptPathsLocal(t *testing.T) {
	for _, path := range []string{"/api/show", "/api/ps", "/api/tags", "/v1/models"} {
		t.Run(path, func(t *testing.T) {
			e := newCOEnv(t, coOpts{loaded: []string{"any-model"}})
			e.r.SetCloudOnlyUnreadable()
			rec := e.serve("POST", path, `{"model":"any-model"}`)
			if strings.Contains(rec.Body.String(), "cloud_only_list_unreadable") {
				t.Fatalf("status=%d body=%s: an exempt path must not be refused", rec.Code, rec.Body.String())
			}
			if e.cloudHits.Load() != 0 {
				t.Fatalf("an exempt path reached cloud %d time(s)", e.cloudHits.Load())
			}
			// Same environment, a non-exempt path is refused: the exempt
			// result above is a real exemption, not a failure of the setup.
			if rec := e.chat("any-model"); rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "cloud_only_list_unreadable") {
				t.Fatalf("control: chat status=%d body=%s, want the unreadable refusal", rec.Code, rec.Body.String())
			}
		})
	}
}

// A request that names no model is not touched by the unreadable state.
func TestProxy_CloudOnly_UnreadableList_NoModelNameNotRefused(t *testing.T) {
	e := newCOEnv(t, coOpts{loaded: []string{"any-model"}})
	e.r.SetCloudOnlyUnreadable()
	rec := e.serve("POST", "/v1/chat/completions", `{"messages":[]}`)
	if strings.Contains(rec.Body.String(), "cloud_only_list_unreadable") {
		t.Fatalf("status=%d body=%s: a request with no model name must not be refused", rec.Code, rec.Body.String())
	}
	if e.cloudHits.Load() != 0 {
		t.Fatalf("a request with no model name reached cloud %d time(s)", e.cloudHits.Load())
	}
}

// A model on a newly set list is served by cloud once the list is restored,
// and one that is not listed runs locally again.
func TestProxy_CloudOnly_UnreadableList_RecoveryRoutesByNewList(t *testing.T) {
	e := newCOEnv(t, coOpts{loaded: []string{"local-model", "paid-model"}})
	e.r.SetCloudOnlyUnreadable()
	if rec := e.chat("paid-model"); rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "cloud_only_list_unreadable") {
		t.Fatalf("while unreadable: status=%d body=%s, want 503 cloud_only_list_unreadable", rec.Code, rec.Body.String())
	}
	if e.cloudHits.Load() != 0 || e.localHits.Load() != 0 {
		t.Fatalf("while unreadable: cloud=%d local=%d, want nothing sent anywhere", e.cloudHits.Load(), e.localHits.Load())
	}
	e.r.SetCloudOnlyModels([]string{"paid-model"})
	if rec := e.chat("paid-model"); rec.Code != http.StatusOK || e.cloudHits.Load() != 1 || e.localHits.Load() != 0 {
		t.Fatalf("listed model after recovery: status=%d cloud=%d local=%d, want served by cloud only", rec.Code, e.cloudHits.Load(), e.localHits.Load())
	}
	if rec := e.chat("local-model"); rec.Code != http.StatusOK || e.localHits.Load() != 1 {
		t.Fatalf("unlisted model after recovery: status=%d local=%d, want served locally", rec.Code, e.localHits.Load())
	}
}
