package proxy

// spine_integration_test.go - end-to-end spine test: auth -> router -> proxy -> admin log.
//
// Boots the real handler chain (auth middleware -> proxy handler -> reverse proxy)
// against a mock Ollama httptest.Server and asserts the full flow from an
// unauthenticated request through to token-count logging in the admin store.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ollama-mesh/ollama-mesh/internal/admin"
	"github.com/ollama-mesh/ollama-mesh/internal/audit"
	"github.com/ollama-mesh/ollama-mesh/internal/auth"
	"github.com/ollama-mesh/ollama-mesh/internal/config"
	"github.com/ollama-mesh/ollama-mesh/internal/router"
)

const (
	spineTestModel   = "llama3.2:8b"
	spineTestKeyVal  = "sk-spine-test-key-001"
	spineTestKeyName = "spine-test"
)

// spineNDJSON is a realistic multi-chunk Ollama /api/generate response.
// The final chunk carries eval_count + prompt_eval_count and "done":true.
const spineNDJSON = `{"model":"llama3.2:8b","response":"Hello","done":false}
{"model":"llama3.2:8b","response":" world","done":false}
{"model":"llama3.2:8b","response":"!","done":true,"eval_count":12,"prompt_eval_count":8}
`

// buildSpineStack constructs the full handler chain used by every Spine sub-test:
//
//	mockOllama  -- responds with spineNDJSON
//	router      -- one node (pointing at mockOllama), pre-seeded as warm for spineTestModel
//	adminSrv    -- real admin.Server for request log inspection
//	authMW      -- auth enabled with one valid key (spineTestKeyVal)
//	handler     -- the composed http.Handler: authMW.Handler(proxyHandler)
//
// The node is set warm directly (no polling) so the test never depends on
// background poll timing.
func buildSpineStack(t *testing.T) (handler http.Handler, adminSrv *admin.Server, mockOllama *httptest.Server) {
	t.Helper()

	// 1. Mock Ollama - returns streaming NDJSON.
	mockOllama = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, spineNDJSON)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}))
	t.Cleanup(mockOllama.Close)

	// 2. Router - single node pointing at the mock, no background polling.
	rtr := router.New(
		config.RoutingConfig{Strategy: "warm-first", Fallback: "least-connections"},
		[]config.NodeConfig{{Name: "gpu-0", URL: mockOllama.URL, GPUModel: "RTX 4090"}},
		nil,
	)

	// Seed the node as warm for spineTestModel so Route() picks it without polling.
	nodes := rtr.Nodes()
	nodes[0].Lock()
	nodes[0].Healthy = true
	nodes[0].LoadedModels = []router.ModelInfo{{Name: spineTestModel, SizeVRAM: 8192}}
	nodes[0].Unlock()

	// 3. Admin server.
	cfg := config.Config{
		Auth: config.AuthConfig{
			Enabled:    true,
			AdminToken: "admin-spine-token",
			Keys: []config.KeyConfig{
				{Name: spineTestKeyName, Key: spineTestKeyVal, RateLimit: 1000},
			},
		},
	}
	adminSrv = admin.NewServer(rtr, nil, cfg)

	// 4. Audit logger (temp file, auto-cleaned).
	al, err := audit.New(filepath.Join(t.TempDir(), "spine-audit.log"))
	if err != nil {
		t.Fatalf("audit.New: %v", err)
	}
	t.Cleanup(func() { al.Close() })

	// 5. Auth middleware.
	authMW := auth.NewMiddleware(cfg.Auth)

	// 6. Proxy handler + auth wrapper.
	proxyH := NewHandler(rtr, adminSrv, al)
	handler = authMW.Handler(proxyH)

	return handler, adminSrv, mockOllama
}

// spineBody builds a minimal /api/generate JSON body for spineTestModel.
func spineBody() []byte {
	b, _ := json.Marshal(map[string]string{"model": spineTestModel, "prompt": "hi"})
	return b
}

// TestSpineIntegration exercises the full assembled proxy spine end-to-end.
func TestSpineIntegration(t *testing.T) {
	handler, adminSrv, _ := buildSpineStack(t)

	// -------------------------------------------------------------------------
	// (a) No Authorization header -> 401
	// -------------------------------------------------------------------------
	t.Run("no_auth_header_is_401", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/api/generate", bytes.NewReader(spineBody()))
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("no-auth: got status %d, want 401", rec.Code)
		}
	})

	// -------------------------------------------------------------------------
	// (b) Wrong bearer token -> 401
	// -------------------------------------------------------------------------
	t.Run("wrong_token_is_401", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/api/generate", bytes.NewReader(spineBody()))
		req.Header.Set("Authorization", "Bearer sk-totally-wrong-key")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("wrong-token: got status %d, want 401", rec.Code)
		}
	})

	// -------------------------------------------------------------------------
	// (c) Valid token -> 200, body arrives intact with done marker
	// -------------------------------------------------------------------------
	t.Run("valid_token_proxied_200", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/api/generate", bytes.NewReader(spineBody()))
		req.Header.Set("Authorization", "Bearer "+spineTestKeyVal)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("valid-token: got status %d, want 200 (body: %s)", rec.Code, rec.Body.String())
		}

		body := rec.Body.String()
		if !strings.Contains(body, `"done":true`) {
			t.Errorf("valid-token: response body missing done:true marker; got: %s", body)
		}
		if !strings.Contains(body, "Hello") {
			t.Errorf("valid-token: response body missing model output; got: %s", body)
		}
	})

	// -------------------------------------------------------------------------
	// (d) After the successful request, admin log records correct model, node,
	//     status 200 ("warm"), and a non-zero token count from eval_count.
	// -------------------------------------------------------------------------
	t.Run("admin_log_records_real_tokens", func(t *testing.T) {
		// Fire a fresh valid request so we have a known entry to inspect.
		req := httptest.NewRequest(http.MethodPost, "/api/generate", bytes.NewReader(spineBody()))
		req.Header.Set("Authorization", "Bearer "+spineTestKeyVal)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("setup request failed with status %d", rec.Code)
		}

		// Read the admin log via the real /admin/requests/live endpoint.
		type reqLogEntry struct {
			Model   string `json:"model"`
			Node    string `json:"routedTo"`
			Status  string `json:"status"`
			Tokens  int64  `json:"tokens"`
			Latency int    `json:"latency"`
		}

		adminReq := httptest.NewRequest(http.MethodGet, "/admin/requests/live", nil)
		adminReq.Header.Set("Authorization", "Bearer admin-spine-token")
		adminRec := httptest.NewRecorder()
		adminSrv.Handler().ServeHTTP(adminRec, adminReq)
		if adminRec.Code != http.StatusOK {
			t.Fatalf("admin log fetch status = %d, want 200", adminRec.Code)
		}

		var entries []reqLogEntry
		if err := json.NewDecoder(adminRec.Body).Decode(&entries); err != nil {
			t.Fatalf("decode admin log: %v", err)
		}
		if len(entries) == 0 {
			t.Fatal("admin log is empty after successful request")
		}

		// The most recent entry is the one we just sent.
		last := entries[len(entries)-1]

		if last.Model != spineTestModel {
			t.Errorf("admin log model = %q, want %q", last.Model, spineTestModel)
		}
		if last.Node != "gpu-0" {
			t.Errorf("admin log node = %q, want gpu-0", last.Node)
		}
		if last.Status != "warm" {
			t.Errorf("admin log status = %q, want warm", last.Status)
		}
		if last.Tokens == 0 {
			// eval_count(12) + prompt_eval_count(8) = 20; zero means token
			// parsing through the real spine is broken.
			t.Log("BUG: admin log token count is 0 - token parsing through the real proxy spine is not working")
			t.Errorf("admin log tokens = 0, want non-zero (eval_count=12, prompt_eval_count=8 in mock response)")
		} else {
			t.Logf("admin log tokens = %d (expected 20 from eval_count+prompt_eval_count)", last.Tokens)
		}
	})

	// -------------------------------------------------------------------------
	// (e) X-Request-ID header is present on a successful response
	// -------------------------------------------------------------------------
	t.Run("x_request_id_present", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/api/generate", bytes.NewReader(spineBody()))
		req.Header.Set("Authorization", "Bearer "+spineTestKeyVal)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("x-request-id test: got status %d, want 200", rec.Code)
		}
		rid := rec.Header().Get("X-Request-ID")
		if rid == "" {
			t.Log("BUG: X-Request-ID header is missing from proxy response")
			t.Error("X-Request-ID header missing from proxy response")
		} else {
			t.Logf("X-Request-ID = %q (length %d)", rid, len(rid))
		}
	})
}
