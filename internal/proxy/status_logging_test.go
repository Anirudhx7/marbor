package proxy

// status_logging_test.go -- P82 regression coverage: the request log (both
// the persisted SQLite request_log table and the live /admin/requests API)
// used to record status:200 for every real non-200 backend response.
//
// Root cause: proxy.go built a semantic label ("warm"/"loading"/"error"/
// "aborted"/"cloud") for admin.LogRequest's status parameter, and
// admin.go tried to strconv.Atoi() that label to get a numeric status code
// for the persisted store record and for /admin/requests - which always
// failed silently and defaulted to 200, discarding the real HTTP status the
// client actually received (which statusRecorder had correctly captured all
// along and used correctly for metrics/audit_log).
//
// The fix threads the real numeric status (statusRecorder.StatusCode())
// through LogRequest as its own parameter, independent of the semantic
// label, which continues to drive cold/warm counters and the
// /admin/requests/live badge exactly as before.

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/Anirudhx7/marbor/internal/admin"
	"github.com/Anirudhx7/marbor/internal/config"
	"github.com/Anirudhx7/marbor/internal/router"
	"github.com/Anirudhx7/marbor/internal/store"
)

// requestLogEntry mirrors the fields of handleRequests' int-status entry
// type that the P82 bug affected.
type requestLogEntry struct {
	Model  string `json:"model"`
	Node   string `json:"node"`
	Status int    `json:"status"`
}

// fetchRequestEntries reads the request log through the real /admin/requests
// endpoint (the surface that used to strconv.Atoi() a semantic label and
// silently default to 200 for every non-numeric status).
func fetchRequestEntries(t *testing.T, a *admin.Server) []requestLogEntry {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/admin/requests", nil)
	req.AddCookie(&http.Cookie{Name: "marbor_session", Value: a.AdminToken()})
	rec := httptest.NewRecorder()
	a.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("/admin/requests status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var entries []requestLogEntry
	if err := json.NewDecoder(rec.Body).Decode(&entries); err != nil {
		t.Fatalf("decode /admin/requests: %v", err)
	}
	return entries
}

// TestLocalServerErrorStatusPreserved is a regression test for a local
// backend 5xx response: before the fix, /admin/requests showed 200 for a
// request that genuinely returned 500 to the client.
func TestLocalServerErrorStatusPreserved(t *testing.T) {
	node := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"error":"backend exploded"}`))
	}))
	defer node.Close()

	h, a := newStreamTestHandler(t, node.URL)
	req := httptest.NewRequest(http.MethodPost, "/api/generate", bytes.NewReader([]byte(`{"model":"mock-node","prompt":"hi"}`)))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("client-visible status = %d, want 500; body=%s", rec.Code, rec.Body.String())
	}

	entries := fetchRequestEntries(t, a)
	if len(entries) != 1 {
		t.Fatalf("got %d request-log entries, want 1", len(entries))
	}
	if entries[0].Status != http.StatusInternalServerError {
		t.Errorf("request_log status = %d, want 500 (the real backend status), not the pre-fix silent 200 default", entries[0].Status)
	}
}

// TestLocalClientErrorStatusPreserved is a regression test for a local
// backend 4xx response, distinct from the 5xx path (5xx also flips
// RecordRequestOutcome's success/failure and the UI's "instant fail"
// latency handling - 4xx does not).
func TestLocalClientErrorStatusPreserved(t *testing.T) {
	node := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`{"error":"model not found"}`))
	}))
	defer node.Close()

	h, a := newStreamTestHandler(t, node.URL)
	req := httptest.NewRequest(http.MethodPost, "/api/generate", bytes.NewReader([]byte(`{"model":"mock-node","prompt":"hi"}`)))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("client-visible status = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}

	entries := fetchRequestEntries(t, a)
	if len(entries) != 1 {
		t.Fatalf("got %d request-log entries, want 1", len(entries))
	}
	if entries[0].Status != http.StatusNotFound {
		t.Errorf("request_log status = %d, want 404 (the real backend status), not the pre-fix silent 200 default", entries[0].Status)
	}
}

// TestCloudNonOKStatusPreserved is a regression test for the cloud path
// (proxyToCloud), which builds its own "cloud"/"aborted" semantic label
// independent of the local path - confirming the same bug and the same fix
// apply there too. A single enabled provider returns a genuine 429 directly
// (not a connection failure, so no ErrorHandler/fallback chain is involved).
func TestCloudNonOKStatusPreserved(t *testing.T) {
	cloudSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		w.Write([]byte(`{"error":{"message":"rate limited"}}`))
	}))
	defer cloudSrv.Close()

	r := router.New(config.RoutingConfig{}, []config.NodeConfig{
		{Name: "gpu-0", URL: "http://localhost:1", GPUModel: "V100"},
	}, []config.CloudProvider{{
		Name:     "fake-openai",
		Provider: "openai",
		BaseURL:  cloudSrv.URL,
		APIKey:   "test-key",
		Enabled:  true,
	}})
	for _, n := range r.Nodes() {
		n.Lock()
		n.Healthy = false
		n.Unlock()
	}
	a := admin.NewServer(r, nil, config.Config{})
	h := NewHandler(r, a, nil)

	req := httptest.NewRequest(http.MethodPost, "/api/chat", bytes.NewReader([]byte(`{"model":"llama3","messages":[]}`)))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("client-visible status = %d, want 429; body=%s", rec.Code, rec.Body.String())
	}

	entries := fetchRequestEntries(t, a)
	if len(entries) != 1 {
		t.Fatalf("got %d request-log entries, want 1", len(entries))
	}
	if entries[0].Status != http.StatusTooManyRequests {
		t.Errorf("request_log status = %d, want 429 (the real cloud provider status), not the pre-fix silent 200 default", entries[0].Status)
	}
}

// TestRequestStatusPersistedToSQLite drives a real 500 response through the
// full handler wired to a real on-disk SQLite store (not just the in-memory
// s.requests slice /admin/requests reads from) and confirms the persisted
// request_log row's status_code column holds the real 500, not 200. This is
// the "persisted SQLite request history" half of P82 - handleRequests only
// exercises the in-memory copy, which shares the bug but not the same code
// path (admin.go's separate strconv.Atoi() at the LogRequest->store.RequestRecord
// boundary).
func TestRequestStatusPersistedToSQLite(t *testing.T) {
	node := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"error":"backend exploded"}`))
	}))
	defer node.Close()

	r := router.New(config.RoutingConfig{Strategy: "warm-first", Fallback: "least-connections"}, []config.NodeConfig{
		{Name: "mock-node", URL: node.URL, GPUModel: "test-gpu", Runtime: "ollama"},
	}, nil)
	n := r.Nodes()[0]
	n.Lock()
	n.Healthy = true
	n.Unlock()

	tmpDB := filepath.Join(t.TempDir(), "status-logging.db")
	st, err := store.Open(tmpDB)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	a := admin.NewServer(r, nil, config.Config{}, st)
	h := NewHandler(r, a, nil)

	req := httptest.NewRequest(http.MethodPost, "/api/generate", bytes.NewReader([]byte(`{"model":"mock-node","prompt":"hi"}`)))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("client-visible status = %d, want 500; body=%s", rec.Code, rec.Body.String())
	}

	// Shutdown drains the async SQLite logger queue synchronously so the
	// persisted row is guaranteed to exist before we read it back.
	a.Shutdown()

	recs, err := st.LastRequests(1)
	if err != nil {
		t.Fatalf("LastRequests: %v", err)
	}
	if len(recs) != 1 {
		t.Fatalf("got %d persisted request_log rows, want 1", len(recs))
	}
	if recs[0].StatusCode != http.StatusInternalServerError {
		t.Errorf("persisted request_log.status_code = %d, want 500 (the real backend status), not the pre-fix silent 200 default", recs[0].StatusCode)
	}
}
