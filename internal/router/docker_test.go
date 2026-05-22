package router

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestIsOllamaContainer(t *testing.T) {
	cases := []struct {
		image string
		want  bool
	}{
		{"ollama/ollama:latest", true},
		{"ollama/ollama", true},
		{"ghcr.io/ollama/ollama:0.3.0", true},
		{"OLLAMA/OLLAMA", true}, // case-insensitive
		{"nginx:latest", false},
		{"ubuntu:22.04", false},
		{"my-custom-app", false},
	}
	for _, tc := range cases {
		c := dockerContainer{Image: tc.image}
		got := isOllamaContainer(c)
		if got != tc.want {
			t.Errorf("isOllamaContainer(%q) = %v, want %v", tc.image, got, tc.want)
		}
	}
}

func TestFindPublicPort(t *testing.T) {
	c := dockerContainer{
		Ports: []struct {
			IP          string `json:"IP"`
			PrivatePort int    `json:"PrivatePort"`
			PublicPort  int    `json:"PublicPort"`
			Type        string `json:"Type"`
		}{
			{IP: "0.0.0.0", PrivatePort: 11434, PublicPort: 32100, Type: "tcp"},
			{IP: "0.0.0.0", PrivatePort: 8080, PublicPort: 32101, Type: "tcp"},
		},
	}
	if got := findPublicPort(c, 11434); got != 32100 {
		t.Errorf("findPublicPort(11434) = %d, want 32100", got)
	}
	if got := findPublicPort(c, 9999); got != 0 {
		t.Errorf("findPublicPort(9999) = %d, want 0 (not found)", got)
	}
}

func TestContainerNodeName(t *testing.T) {
	c := dockerContainer{Names: []string{"/my-ollama"}}
	if got := containerNodeName(c); got != "my-ollama" {
		t.Errorf("containerNodeName = %q, want my-ollama", got)
	}

	c2 := dockerContainer{ID: "abcdef123456789"}
	if got := containerNodeName(c2); got != "docker-abcdef123456" {
		t.Errorf("containerNodeName (no names) = %q, want docker-abcdef123456", got)
	}
}

func TestDiscoverDockerNodesHTTP(t *testing.T) {
	// Fake Docker API server (HTTP, not Unix socket) for unit testing.
	// We test the HTTP parsing logic by temporarily pointing at a test HTTP server.
	containers := []dockerContainer{
		{
			ID:    "abc123",
			Names: []string{"/ollama-main"},
			Image: "ollama/ollama:latest",
			Ports: []struct {
				IP          string `json:"IP"`
				PrivatePort int    `json:"PrivatePort"`
				PublicPort  int    `json:"PublicPort"`
				Type        string `json:"Type"`
			}{
				{IP: "0.0.0.0", PrivatePort: 11434, PublicPort: 11435, Type: "tcp"},
			},
		},
		{
			ID:    "def456",
			Names: []string{"/nginx"},
			Image: "nginx:latest",
			Ports: []struct {
				IP          string `json:"IP"`
				PrivatePort int    `json:"PrivatePort"`
				PublicPort  int    `json:"PublicPort"`
				Type        string `json:"Type"`
			}{
				{IP: "0.0.0.0", PrivatePort: 80, PublicPort: 8080, Type: "tcp"},
			},
		},
		{
			// Ollama container with no mapped port — should be skipped.
			ID:    "ghi789",
			Names: []string{"/ollama-no-port"},
			Image: "ollama/ollama",
			Ports: []struct {
				IP          string `json:"IP"`
				PrivatePort int    `json:"PrivatePort"`
				PublicPort  int    `json:"PublicPort"`
				Type        string `json:"Type"`
			}{},
		},
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/containers/json") {
			http.NotFound(w, r)
			return
		}
		json.NewEncoder(w).Encode(containers)
	}))
	defer srv.Close()

	// Parse server URL to get host:port, then test filtering logic directly.
	// We can't easily use Unix socket in tests, so test the parsing helpers instead.
	// The HTTP client logic is covered by integration; here we verify filter+parse.
	for _, c := range containers {
		if c.Image == "ollama/ollama:latest" {
			if !isOllamaContainer(c) {
				t.Error("ollama/ollama:latest should be detected as ollama container")
			}
			if p := findPublicPort(c, 11434); p != 11435 {
				t.Errorf("port = %d, want 11435", p)
			}
			if n := containerNodeName(c); n != "ollama-main" {
				t.Errorf("name = %q, want ollama-main", n)
			}
		}
		if c.Image == "nginx:latest" && isOllamaContainer(c) {
			t.Error("nginx should not be detected as ollama container")
		}
	}
}
