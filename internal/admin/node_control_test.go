package admin

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/ollama-mesh/ollama-mesh/internal/config"
	"github.com/ollama-mesh/ollama-mesh/internal/router"
	"github.com/ollama-mesh/ollama-mesh/internal/store"
)

func newTestServerWithNodeAndStore(t *testing.T) *Server {
	t.Helper()
	r := router.New(config.RoutingConfig{}, []config.NodeConfig{{Name: "gpu-1", URL: "http://10.0.0.5:11434"}}, nil)
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return NewServer(r, nil, config.Config{}, st)
}

func TestHandleGetNodeControlUnconfigured(t *testing.T) {
	s := newTestServerWithNodeAndStore(t)

	req := httptest.NewRequest(http.MethodGet, "/admin/nodes/gpu-1/control", nil)
	req.SetPathValue("name", "gpu-1")
	w := httptest.NewRecorder()
	s.handleGetNodeControl(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	var body map[string]interface{}
	if err := json.NewDecoder(w.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["configured"] != false {
		t.Errorf("configured = %v, want false for a never-configured node", body["configured"])
	}
}

func TestHandleGetNodeControlUnknownNode(t *testing.T) {
	s := newTestServerWithNodeAndStore(t)

	req := httptest.NewRequest(http.MethodGet, "/admin/nodes/nonexistent/control", nil)
	req.SetPathValue("name", "nonexistent")
	w := httptest.NewRecorder()
	s.handleGetNodeControl(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 for an unknown node", w.Code)
	}
}

func TestHandleAcceptNodeControlThenGet(t *testing.T) {
	s := newTestServerWithNodeAndStore(t)

	body, _ := json.Marshal(map[string]string{"driver": "systemd", "identifier": "ollama.service"})
	req := httptest.NewRequest(http.MethodPost, "/admin/nodes/gpu-1/control/accept", bytes.NewReader(body))
	req.SetPathValue("name", "gpu-1")
	w := httptest.NewRecorder()
	s.handleAcceptNodeControl(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("accept status = %d, want 200, body=%s", w.Code, w.Body.String())
	}

	// Persisted to the store.
	rec, found, err := s.st.GetNodeControl("gpu-1")
	if err != nil || !found {
		t.Fatalf("GetNodeControl: found=%v err=%v", found, err)
	}
	if !rec.Configured || rec.Driver != "systemd" || rec.Identifier != "ollama.service" {
		t.Fatalf("persisted record = %+v, want configured systemd/ollama.service", rec)
	}

	// Pushed to the live router cache.
	cfg, ok := s.router.NodeControlSetting("gpu-1")
	if !ok || !cfg.Configured || cfg.Driver != "systemd" {
		t.Fatalf("router.NodeControlSetting = %+v, ok=%v, want configured systemd", cfg, ok)
	}
}

func TestHandleAcceptNodeControlRejectsUnknownDriver(t *testing.T) {
	s := newTestServerWithNodeAndStore(t)

	body, _ := json.Marshal(map[string]string{"driver": "kubernetes", "identifier": "ollama"})
	req := httptest.NewRequest(http.MethodPost, "/admin/nodes/gpu-1/control/accept", bytes.NewReader(body))
	req.SetPathValue("name", "gpu-1")
	w := httptest.NewRecorder()
	s.handleAcceptNodeControl(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for an unsupported driver (kubernetes is explicitly deferred)", w.Code)
	}
}

// TestHandleAcceptNodeControlThenRescanDoesNotChangeConfigured is the admin
// API-level version of the store-layer invariant test: accepting a driver,
// then a discovery re-scan reporting something different, must never change
// what handleGetNodeControl reports as configured (section 5.6).
func TestHandleAcceptNodeControlThenRescanDoesNotChangeConfigured(t *testing.T) {
	s := newTestServerWithNodeAndStore(t)

	acceptBody, _ := json.Marshal(map[string]string{"driver": "systemd", "identifier": "ollama.service"})
	req := httptest.NewRequest(http.MethodPost, "/admin/nodes/gpu-1/control/accept", bytes.NewReader(acceptBody))
	req.SetPathValue("name", "gpu-1")
	s.handleAcceptNodeControl(httptest.NewRecorder(), req)

	// Simulate a re-scan finding Docker instead - directly at the store
	// layer, the same call path pollAgentTelemetry -> handleGetNodeControl
	// would eventually trigger once agent-side discovery reporting lands.
	if err := s.st.UpsertNodeControlDiscovered("gpu-1", "docker", "ollama", []string{"docker container \"ollama\" found"}); err != nil {
		t.Fatalf("UpsertNodeControlDiscovered: %v", err)
	}

	getReq := httptest.NewRequest(http.MethodGet, "/admin/nodes/gpu-1/control", nil)
	getReq.SetPathValue("name", "gpu-1")
	w := httptest.NewRecorder()
	s.handleGetNodeControl(w, getReq)

	var got map[string]interface{}
	if err := json.NewDecoder(w.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got["driver"] != "systemd" || got["configured"] != true {
		t.Fatalf("GET /control after re-scan = %+v, want unchanged systemd/configured=true", got)
	}
}

func TestHandleClearNodeControl(t *testing.T) {
	s := newTestServerWithNodeAndStore(t)

	acceptBody, _ := json.Marshal(map[string]string{"driver": "docker", "identifier": "ollama"})
	req := httptest.NewRequest(http.MethodPost, "/admin/nodes/gpu-1/control/accept", bytes.NewReader(acceptBody))
	req.SetPathValue("name", "gpu-1")
	s.handleAcceptNodeControl(httptest.NewRecorder(), req)

	delReq := httptest.NewRequest(http.MethodDelete, "/admin/nodes/gpu-1/control", nil)
	delReq.SetPathValue("name", "gpu-1")
	w := httptest.NewRecorder()
	s.handleClearNodeControl(w, delReq)
	if w.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", w.Code)
	}

	if _, ok := s.router.NodeControlSetting("gpu-1"); ok {
		t.Error("expected router control cache cleared after DELETE")
	}
	rec, found, err := s.st.GetNodeControl("gpu-1")
	if err != nil || !found {
		t.Fatalf("GetNodeControl: found=%v err=%v", found, err)
	}
	if rec.Configured {
		t.Error("expected Configured=false after clear")
	}
}
