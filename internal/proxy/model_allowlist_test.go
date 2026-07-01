package proxy

// model_allowlist_test.go - proves per-key model allow-lists are ENFORCED.
//
// A key with a non-empty models list must be blocked (403) when it requests a
// model outside that list, and allowed (200) for a model inside it. A key with
// no models list is unrestricted (control - no regression).

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/ollama-mesh/ollama-mesh/internal/admin"
	"github.com/ollama-mesh/ollama-mesh/internal/audit"
	"github.com/ollama-mesh/ollama-mesh/internal/auth"
	"github.com/ollama-mesh/ollama-mesh/internal/config"
	"github.com/ollama-mesh/ollama-mesh/internal/router"
	"github.com/ollama-mesh/ollama-mesh/internal/store"
)

// buildAllowlistStack builds the auth->proxy chain with a single node that is
// warm for both models, and the given set of auth keys.
func buildAllowlistStack(t *testing.T, keys []config.KeyConfig) http.Handler {
	t.Helper()

	mockOllama := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"model":"x","response":"ok","done":true,"eval_count":3,"prompt_eval_count":2}` + "\n"))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}))
	t.Cleanup(mockOllama.Close)

	rtr := router.New(
		config.RoutingConfig{Strategy: "warm-first", Fallback: "least-connections"},
		[]config.NodeConfig{{Name: "gpu-0", URL: mockOllama.URL, Runtime: "ollama"}},
		nil,
	)
	// Seed both models warm so routing is never the reason for a non-200.
	nodes := rtr.Nodes()
	nodes[0].Lock()
	nodes[0].Healthy = true
	nodes[0].LoadedModels = []router.ModelInfo{
		{Name: "llama3.2:8b", SizeVRAM: 8192},
		{Name: "mistral:7b", SizeVRAM: 8192},
	}
	nodes[0].Unlock()

	cfg := config.Config{Auth: config.AuthConfig{Enabled: config.BoolPtr(true), AdminToken: "admin-tok", Keys: keys}}
	adminSrv := admin.NewServer(rtr, nil, cfg)
	tmpDB := filepath.Join(t.TempDir(), "allowlist-audit.db")
	st, err := store.Open(tmpDB)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	al := audit.New(st, true)
	t.Cleanup(func() { al.Close() })

	authMW := auth.NewMiddleware(cfg.Auth)
	return authMW.Handler(NewHandler(rtr, adminSrv, al))
}

func allowlistBody(model string) []byte {
	b, _ := json.Marshal(map[string]string{"model": model, "prompt": "hi"})
	return b
}

func doAllowlistReq(t *testing.T, h http.Handler, key, model string) int {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/generate", bytes.NewReader(allowlistBody(model)))
	req.Header.Set("Authorization", "Bearer "+key)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code
}

func TestModelAllowlistEnforced(t *testing.T) {
	const restrictedKey = "sk-restricted"
	h := buildAllowlistStack(t, []config.KeyConfig{
		{Name: "restricted", Key: restrictedKey, RateLimit: 1000, Models: []string{"llama3.2:8b"}},
	})

	t.Run("disallowed_model_is_403", func(t *testing.T) {
		if code := doAllowlistReq(t, h, restrictedKey, "mistral:7b"); code != http.StatusForbidden {
			t.Errorf("disallowed model: got %d, want 403 - allow-list not enforced", code)
		}
	})

	t.Run("allowed_model_is_200", func(t *testing.T) {
		if code := doAllowlistReq(t, h, restrictedKey, "llama3.2:8b"); code != http.StatusOK {
			t.Errorf("allowed model: got %d, want 200", code)
		}
	})
}

func TestNoAllowlistMeansUnrestricted(t *testing.T) {
	const openKey = "sk-open"
	h := buildAllowlistStack(t, []config.KeyConfig{
		{Name: "open", Key: openKey, RateLimit: 1000}, // no Models -> unrestricted
	})

	// Any model the cluster can serve must pass for an unrestricted key.
	for _, model := range []string{"llama3.2:8b", "mistral:7b"} {
		if code := doAllowlistReq(t, h, openKey, model); code != http.StatusOK {
			t.Errorf("unrestricted key, model %s: got %d, want 200", model, code)
		}
	}
}
