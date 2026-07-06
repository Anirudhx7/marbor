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
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	got, reached := DetectRuntime(context.Background(), srv.URL, srv.Client())
	if got != "ollama" {
		t.Errorf("expected ollama, got %q", got)
	}
	if !reached {
		t.Error("expected reached=true")
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
