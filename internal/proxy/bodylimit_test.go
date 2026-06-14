package proxy

// bodylimit_test.go - proves the proxy caps request body size (DoS guard).
//
// A request body larger than maxRequestBodyBytes must be rejected with 413
// before the proxy buffers it all, and a normal small body must pass the gate.

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ollama-mesh/ollama-mesh/internal/admin"
	"github.com/ollama-mesh/ollama-mesh/internal/config"
	"github.com/ollama-mesh/ollama-mesh/internal/router"
)

func newBareHandler(t *testing.T) *Handler {
	t.Helper()
	rtr := router.New(config.RoutingConfig{}, nil, nil)
	adminSrv := admin.NewServer(rtr, nil, config.Config{})
	return NewHandler(rtr, adminSrv, nil)
}

func TestRequestBodyTooLargeReturns413(t *testing.T) {
	h := newBareHandler(t)

	// One byte over the limit.
	oversized := bytes.Repeat([]byte("a"), maxRequestBodyBytes+1)
	req := httptest.NewRequest(http.MethodPost, "/api/generate", bytes.NewReader(oversized))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized body got status %d, want 413", rec.Code)
	}
}

func TestRequestBodyUnderLimitPassesGate(t *testing.T) {
	h := newBareHandler(t)

	// A small, well-formed body must clear the size gate. With no nodes
	// registered it then gets 503 (no healthy nodes) - NOT 413 - which proves
	// the body gate let it through.
	body := []byte(`{"model":"llama3.2:8b","prompt":"hi"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/generate", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code == http.StatusRequestEntityTooLarge {
		t.Fatalf("small body wrongly rejected as too large (413)")
	}
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("small body with no nodes got %d, want 503", rec.Code)
	}
}
