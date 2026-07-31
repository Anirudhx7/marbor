package cli

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestClient_Health(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/health" {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"status":"ok","version":"v0.19.0","proxy_port":11434,"uptime_seconds":42,"nodes":{"total":2,"healthy":2}}`))
	}))
	defer srv.Close()

	client := NewClient(srv.URL, "")
	health, err := client.Health()
	if err != nil {
		t.Fatalf("Health() error: %v", err)
	}
	if health.Status != "ok" || health.Version != "v0.19.0" || health.Nodes.Total != 2 || health.Nodes.Healthy != 2 {
		t.Fatalf("unexpected health response: %+v", health)
	}
}

func TestClient_Health_Unreachable(t *testing.T) {
	client := NewClient("http://127.0.0.1:1", "")
	_, err := client.Health()
	if err == nil {
		t.Fatal("expected an error for an unreachable server")
	}
	cliErr, ok := err.(*CLIError)
	if !ok || cliErr.Code != ExitServerError {
		t.Fatalf("expected ExitServerError, got %v", err)
	}
}

func TestClient_Login(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/admin/v1/login" {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		var body struct {
			Username string `json:"username"`
			Password string `json:"password"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		if body.Username != "admin" || body.Password != "admin" {
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte(`{"error":"invalid credentials"}`))
			return
		}
		http.SetCookie(w, &http.Cookie{Name: "mesh_session", Value: "test-session-token"})
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"role":"admin","username":"admin","must_change_password":false}`))
	}))
	defer srv.Close()

	client := NewClient(srv.URL, "")
	if err := client.Login("admin", "admin"); err != nil {
		t.Fatalf("Login() error: %v", err)
	}
	if client.Token != "test-session-token" {
		t.Fatalf("expected token to be captured from Set-Cookie, got %q", client.Token)
	}
}

func TestClient_Login_InvalidCredentials(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":"invalid credentials"}`))
	}))
	defer srv.Close()

	client := NewClient(srv.URL, "")
	err := client.Login("admin", "wrong")
	cliErr, ok := err.(*CLIError)
	if !ok || cliErr.Code != ExitAuthError {
		t.Fatalf("expected ExitAuthError, got %v", err)
	}
}

func TestClient_Nodes_RequiresToken(t *testing.T) {
	client := NewClient("http://example.invalid", "")
	_, err := client.Nodes()
	cliErr, ok := err.(*CLIError)
	if !ok || cliErr.Code != ExitUserError {
		t.Fatalf("expected ExitUserError for missing token, got %v", err)
	}
}

func TestClient_Nodes(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/admin/v1/nodes" {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer abc123" {
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte(`{"error":"unauthorized"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`[{"id":"gpu-0","name":"gpu-01","host":"10.0.0.1","port":11434,"gpuModel":"RTX 4090","vramTotalMB":24576,"vramUsedMB":8192,"runtime":"ollama","health":"healthy","draining":false,"loadedModels":[{"name":"llama3","size_vram":123}],"activeConns":1,"requestsTotal":10}]`))
	}))
	defer srv.Close()

	client := NewClient(srv.URL, "abc123")
	nodes, err := client.Nodes()
	if err != nil {
		t.Fatalf("Nodes() error: %v", err)
	}
	if len(nodes) != 1 || nodes[0].Name != "gpu-01" || nodes[0].Health != "healthy" || len(nodes[0].LoadedModels) != 1 {
		t.Fatalf("unexpected nodes response: %+v", nodes)
	}
}

func TestClient_Nodes_Unauthorized(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":"unauthorized"}`))
	}))
	defer srv.Close()

	client := NewClient(srv.URL, "bad-token")
	_, err := client.Nodes()
	cliErr, ok := err.(*CLIError)
	if !ok || cliErr.Code != ExitAuthError {
		t.Fatalf("expected ExitAuthError, got %v", err)
	}
}

func TestClient_Models(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/admin/v1/models" {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"models":[{"name":"llama3","size_vram":123456,"size_disk":654321,"nodes":[{"name":"gpu-01","healthy":true}],"warm_count":1,"total_nodes":2,"family":"llama","digest_mismatch":false}],"total_models":1,"total_nodes":2,"healthy_nodes":2}`))
	}))
	defer srv.Close()

	client := NewClient(srv.URL, "abc123")
	models, err := client.Models()
	if err != nil {
		t.Fatalf("Models() error: %v", err)
	}
	if models.TotalModels != 1 || len(models.Models) != 1 || models.Models[0].Name != "llama3" {
		t.Fatalf("unexpected models response: %+v", models)
	}
}

func TestClient_Models_ServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"error":"boom"}`))
	}))
	defer srv.Close()

	client := NewClient(srv.URL, "abc123")
	_, err := client.Models()
	cliErr, ok := err.(*CLIError)
	if !ok || cliErr.Code != ExitServerError {
		t.Fatalf("expected ExitServerError, got %v", err)
	}
}
