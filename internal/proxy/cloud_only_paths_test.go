package proxy

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Anirudhx7/marbor/internal/auth"
)

// serveNoBody sends a request with a nil body, as a real bodyless GET arrives.
func (e *coEnv) serveNoBody(method, path string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, nil)
	req = req.WithContext(context.WithValue(req.Context(), auth.KeyNameContextKey, "k"))
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	return rec
}

// A model lookup for a listed model is a read-only call: it is served by a
// local node and never reaches the paid provider.
func TestProxy_CloudOnly_ModelLookupServedLocallyNeverCloud(t *testing.T) {
	// The trailing-slash spelling is looked up as the name "paid-model/", which
	// no node has: the local path answers 404, still without touching cloud.
	for path, wantStatus := range map[string]int{"/v1/models/paid-model": http.StatusOK, "/v1/models/paid-model/": http.StatusNotFound} {
		t.Run(path, func(t *testing.T) {
			e := newCOEnv(t, coOpts{list: []string{"paid-model"}, loaded: []string{"paid-model"}})
			rec := e.serve("GET", path, "")
			if e.cloudHits.Load() != 0 {
				t.Fatalf("GET %s reached cloud %d time(s), status %d", path, e.cloudHits.Load(), rec.Code)
			}
			if rec.Code != wantStatus || e.localHits.Load() != 1 {
				t.Errorf("GET %s: status=%d local=%d body=%s, want %d from the local path", path, rec.Code, e.localHits.Load(), rec.Body.String(), wantStatus)
			}
		})
	}
}

// The model list endpoint itself is exempt and served locally.
func TestProxy_CloudOnly_ModelsListEndpointExempt(t *testing.T) {
	e := newCOEnv(t, coOpts{list: []string{"paid-model"}, loaded: []string{"paid-model"}})
	rec := e.serve("GET", "/v1/models", "")
	if e.cloudHits.Load() != 0 {
		t.Fatalf("GET /v1/models reached cloud %d time(s)", e.cloudHits.Load())
	}
	if rec.Code != http.StatusOK || e.localHits.Load() != 1 {
		t.Errorf("GET /v1/models: status=%d local=%d body=%s, want 200 served by the local node", rec.Code, e.localHits.Load(), rec.Body.String())
	}
}

// A request that names no model, or an empty one, never matches the list and
// is routed locally as before.
func TestProxy_CloudOnly_AbsentOrEmptyModelServedLocally(t *testing.T) {
	for name, body := range map[string]string{"absent": `{"messages":[]}`, "empty": `{"model":"","messages":[]}`} {
		t.Run(name, func(t *testing.T) {
			e := newCOEnv(t, coOpts{list: []string{"paid-model", "paid-*"}, loaded: []string{"paid-model"}})
			rec := e.serve("POST", "/v1/chat/completions", body)
			if e.cloudHits.Load() != 0 {
				t.Fatalf("a request naming no model reached cloud (status %d)", rec.Code)
			}
			if rec.Code != http.StatusOK || e.localHits.Load() != 1 {
				t.Errorf("status=%d local=%d body=%s, want 200 served by the local node", rec.Code, e.localHits.Load(), rec.Body.String())
			}
		})
	}
}

// Management paths never reach cloud, with or without the management endpoints
// enabled, and a trailing slash does not change that.
func TestProxy_CloudOnly_ManagementPathVariants(t *testing.T) {
	for _, path := range []string{"/api/pull", "/api/pull/"} {
		for _, allow := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/allowMgmt=%v", path, allow), func(t *testing.T) {
				e := newCOEnv(t, coOpts{list: []string{"paid-model"}, allowMgmt: allow})
				rec := e.serve("POST", path, `{"model":"paid-model"}`)
				if e.cloudHits.Load() != 0 {
					t.Fatalf("%s (allowMgmt=%v) reached cloud %d time(s)", path, allow, e.cloudHits.Load())
				}
				if allow {
					if e.localHits.Load() != 1 || rec.Code != http.StatusOK {
						t.Errorf("allowed: status=%d local=%d, want the call passed through locally", rec.Code, e.localHits.Load())
					}
				} else if e.localHits.Load() != 0 || rec.Code < 400 {
					t.Errorf("blocked: status=%d local=%d, want a refusal and no local call", rec.Code, e.localHits.Load())
				}
			})
		}
	}
}

// A non-cloud-capable path fails closed with or without a trailing slash, and a
// cloud-capable path keeps going to cloud with one.
func TestProxy_CloudOnly_TrailingSlashPaths(t *testing.T) {
	e := newCOEnv(t, coOpts{list: []string{"paid-model"}, loaded: []string{"paid-model"}})
	rec := e.serve("POST", "/v1/rerank/", `{"model":"paid-model","input":"x"}`)
	if rec.Code != http.StatusServiceUnavailable || errCode(t, rec) != "cloud_only_unsupported_path" {
		t.Fatalf("/v1/rerank/: status=%d body=%s, want 503 cloud_only_unsupported_path", rec.Code, rec.Body.String())
	}
	if e.cloudHits.Load() != 0 || e.localHits.Load() != 0 {
		t.Errorf("/v1/rerank/: cloud=%d local=%d, want neither contacted", e.cloudHits.Load(), e.localHits.Load())
	}

	e = newCOEnv(t, coOpts{list: []string{"paid-model"}, loaded: []string{"paid-model"}})
	rec = e.serve("POST", "/v1/chat/completions/", `{"model":"paid-model","messages":[]}`)
	if rec.Code != http.StatusOK || e.cloudHits.Load() != 1 || e.localHits.Load() != 0 {
		t.Errorf("/v1/chat/completions/: status=%d cloud=%d local=%d, want served by cloud only", rec.Code, e.cloudHits.Load(), e.localHits.Load())
	}
}

// Read-only exempt paths work as bodyless GETs.
func TestProxy_CloudOnly_ExemptBodylessGets(t *testing.T) {
	for _, path := range []string{"/api/ps", "/api/tags"} {
		t.Run("GET "+path, func(t *testing.T) {
			e := newCOEnv(t, coOpts{list: []string{"paid-model"}, loaded: []string{"paid-model"}})
			rec := e.serveNoBody("GET", path)
			if e.cloudHits.Load() != 0 {
				t.Fatalf("GET %s reached cloud %d time(s), status %d", path, e.cloudHits.Load(), rec.Code)
			}
			if rec.Code != http.StatusOK || e.localHits.Load() != 1 {
				t.Errorf("GET %s: status=%d local=%d body=%s, want 200 served by the local node", path, rec.Code, e.localHits.Load(), rec.Body.String())
			}
		})
	}
}

// On a read-only exempt path the chain walk still skips a listed substitute
// and never reaches cloud.
func TestProxy_CloudOnly_ChainSkipsListedOnExemptPath(t *testing.T) {
	e := newCOEnv(t, coOpts{
		list:     []string{"listed-q4"},
		vramMB:   8192,
		fallback: map[string][]string{"big": {"listed-q4", "other-q4"}},
		tags: `{"models":[
			{"name":"big","size":41943040000},
			{"name":"listed-q4","size":4194304000},
			{"name":"other-q4","size":4194304000}]}`,
	})
	e.serve("POST", "/api/show", `{"model":"big"}`)
	if e.cloudHits.Load() != 0 {
		t.Fatalf("an exempt path reached cloud %d time(s)", e.cloudHits.Load())
	}
	if e.localHits.Load() == 0 {
		t.Fatal("no local call happened, so the chain walk was never exercised")
	}
	for _, m := range e.localModels() {
		if m == "listed-q4" {
			t.Fatal("a cloud-only model was used as a chain substitute on an exempt path")
		}
	}
	if got := e.localModels(); len(got) != 1 || got[0] != "other-q4" {
		t.Errorf("local models = %v, want the chain to pick the non-listed other-q4", got)
	}
}

// Paths are case-sensitive, as on the backends: a different-case spelling of an
// exempt path is a different, unknown path, so it fails closed - never cloud,
// never a local node.
func TestProxy_CloudOnly_CaseVariantOfExemptPathFailsClosed(t *testing.T) {
	e := newCOEnv(t, coOpts{list: []string{"paid-model"}, loaded: []string{"paid-model"}})
	rec := e.serve("POST", "/API/SHOW", `{"model":"paid-model"}`)
	if rec.Code != http.StatusServiceUnavailable || errCode(t, rec) != "cloud_only_unsupported_path" {
		t.Fatalf("status=%d body=%s, want 503 cloud_only_unsupported_path", rec.Code, rec.Body.String())
	}
	if e.cloudHits.Load() != 0 || e.localHits.Load() != 0 {
		t.Errorf("cloud=%d local=%d, want neither contacted", e.cloudHits.Load(), e.localHits.Load())
	}
}
