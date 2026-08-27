package runtime

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestDetectRuntime_Ollama(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/ps" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"models":[]}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	got, reached, confirmed := DetectRuntimeConfirmed(context.Background(), srv.URL, srv.Client())
	if got != "ollama" {
		t.Errorf("expected ollama, got %q", got)
	}
	if !reached {
		t.Error("expected reached=true")
	}
	if !confirmed {
		t.Error("expected confirmed=true for a real {\"models\":[...]} shape")
	}
}

// TestDetectRuntime_OllamaShapeMismatch is P260's regression case: a bare
// HTTP 200 on /api/ps with no {"models": [...]} body is not a real Ollama
// signature - any non-Ollama server answering 200 on that exact path must
// not be permanently misclassified "ollama" on the strength of the status
// code alone.
func TestDetectRuntime_OllamaShapeMismatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/ps" {
			// 200 with a body, but not Ollama's shape at all.
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"status":"ok"}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	got, reached, confirmed := DetectRuntimeConfirmed(context.Background(), srv.URL, srv.Client())
	if got != "ollama" {
		t.Errorf("expected fallback runtime ollama (unconfirmed), got %q", got)
	}
	if !reached {
		t.Error("expected reached=true")
	}
	if confirmed {
		t.Error("expected confirmed=false - a bare 200 with the wrong body shape must not count as a real Ollama match")
	}
}

func TestDetectRuntime_TGI(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/ps":
			w.WriteHeader(http.StatusNotFound)
		case "/info":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"model_id":"mistral"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	got, _ := DetectRuntime(context.Background(), srv.URL, srv.Client())
	if got != "tgi" {
		t.Errorf("expected tgi, got %q", got)
	}
}

func TestDetectRuntime_VLLM(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/ps":
			w.WriteHeader(http.StatusNotFound)
		case "/info":
			w.WriteHeader(http.StatusNotFound)
		case "/v1/models":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"data":[{"owned_by":"vllm"}]}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	got, _ := DetectRuntime(context.Background(), srv.URL, srv.Client())
	if got != "vllm" {
		t.Errorf("expected vllm, got %q", got)
	}
}

func TestDetectRuntime_LlamaCpp(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/ps":
			w.WriteHeader(http.StatusNotFound)
		case "/info":
			w.WriteHeader(http.StatusNotFound)
		case "/v1/models":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"data":[{"owned_by":"llama.cpp"}]}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	got, _ := DetectRuntime(context.Background(), srv.URL, srv.Client())
	if got != "llamacpp" {
		t.Errorf("expected llamacpp, got %q", got)
	}
}

func TestDetectRuntime_Fallback(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	got, reached := DetectRuntime(context.Background(), srv.URL, srv.Client())
	if got != "ollama" {
		t.Errorf("expected ollama fallback, got %q", got)
	}
	if !reached {
		t.Error("expected reached=true: node responded 404, it was contacted")
	}
}

func TestDetectRuntime_Unreachable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	url := srv.URL
	srv.Close() // closed server: connections fail at the transport level

	got, reached := DetectRuntime(context.Background(), url, srv.Client())
	if got != "ollama" {
		t.Errorf("expected ollama fallback, got %q", got)
	}
	if reached {
		t.Error("expected reached=false: node was never actually contacted")
	}
}
