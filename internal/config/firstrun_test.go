package config

import (
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestFirstRunOllamaDetected(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/tags" {
			t.Errorf("probe path = %q, want /api/tags", r.URL.Path)
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"models":[]}`))
	}))
	defer srv.Close()

	fr, err := GenerateFirstRun(srv.URL, 2*time.Second)
	if err != nil {
		t.Fatalf("GenerateFirstRun: %v", err)
	}
	if !fr.OllamaFound {
		t.Error("OllamaFound = false, want true")
	}
	if len(fr.Config.Nodes) != 1 {
		t.Fatalf("nodes = %d, want 1", len(fr.Config.Nodes))
	}
	if fr.Config.Nodes[0].Name != "local" {
		t.Errorf("node name = %q, want local", fr.Config.Nodes[0].Name)
	}
	if fr.Config.Nodes[0].URL != srv.URL {
		t.Errorf("node url = %q, want %q", fr.Config.Nodes[0].URL, srv.URL)
	}
}

func TestFirstRunOllamaNotResponding(t *testing.T) {
	// A closed test server gives us a guaranteed-unused address that
	// refuses connections immediately.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	deadURL := srv.URL
	srv.Close()

	fr, err := GenerateFirstRun(deadURL, 2*time.Second)
	if err != nil {
		t.Fatalf("GenerateFirstRun: %v", err)
	}
	if fr.OllamaFound {
		t.Error("OllamaFound = true, want false")
	}
	if len(fr.Config.Nodes) != 0 {
		t.Errorf("nodes = %d, want 0", len(fr.Config.Nodes))
	}
	// First-run still produces a usable config even with no nodes.
	if fr.Config.Proxy.Port != FirstRunProxyPort {
		t.Errorf("port = %d, want %d", fr.Config.Proxy.Port, FirstRunProxyPort)
	}
}

func TestFirstRunGeneratedValues(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	fr, err := GenerateFirstRun(srv.URL, 2*time.Second)
	if err != nil {
		t.Fatalf("GenerateFirstRun: %v", err)
	}

	// Proxy port must be 11435: 11434 is taken by the detected Ollama.
	if fr.Config.Proxy.Port != FirstRunProxyPort {
		t.Errorf("port = %d, want %d", fr.Config.Proxy.Port, FirstRunProxyPort)
	}

	// API key: "sk-mesh-" + 24 hex chars.
	if !strings.HasPrefix(fr.APIKey, "sk-mesh-") {
		t.Errorf("api key %q missing sk-mesh- prefix", fr.APIKey)
	}
	suffix := strings.TrimPrefix(fr.APIKey, "sk-mesh-")
	if len(suffix) != 24 {
		t.Errorf("api key suffix length = %d, want 24", len(suffix))
	}
	if _, err := hex.DecodeString(suffix); err != nil {
		t.Errorf("api key suffix %q is not hex: %v", suffix, err)
	}

	// Admin token: 16 hex chars.
	if len(fr.AdminToken) != 16 {
		t.Errorf("admin token length = %d, want 16", len(fr.AdminToken))
	}
	if _, err := hex.DecodeString(fr.AdminToken); err != nil {
		t.Errorf("admin token %q is not hex: %v", fr.AdminToken, err)
	}

	// Auth must be enabled with one key named "default".
	if !fr.Config.Auth.Enabled {
		t.Error("auth not enabled in generated config")
	}
	if len(fr.Config.Auth.Keys) != 1 || fr.Config.Auth.Keys[0].Name != "default" {
		t.Errorf("auth keys = %+v, want one key named default", fr.Config.Auth.Keys)
	}
	if fr.Config.Auth.Keys[0].Key != fr.APIKey {
		t.Error("config key does not match returned APIKey")
	}
	if fr.Config.Auth.AdminToken != fr.AdminToken {
		t.Error("config admin token does not match returned AdminToken")
	}

	// Two runs must not generate identical secrets.
	fr2, err := GenerateFirstRun(srv.URL, 2*time.Second)
	if err != nil {
		t.Fatalf("second GenerateFirstRun: %v", err)
	}
	if fr2.APIKey == fr.APIKey {
		t.Error("two runs generated identical API keys")
	}
	if fr2.AdminToken == fr.AdminToken {
		t.Error("two runs generated identical admin tokens")
	}
}

func TestFirstRunConfigRoundTrip(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	fr, err := GenerateFirstRun(srv.URL, 2*time.Second)
	if err != nil {
		t.Fatalf("GenerateFirstRun: %v", err)
	}

	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := SaveConfig(path, *fr.Config); err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}

	loaded, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig after save: %v", err)
	}
	if loaded.Proxy.Port != FirstRunProxyPort {
		t.Errorf("loaded port = %d, want %d", loaded.Proxy.Port, FirstRunProxyPort)
	}
	if len(loaded.Auth.Keys) != 1 || loaded.Auth.Keys[0].Key != fr.APIKey {
		t.Errorf("loaded keys = %+v, want key %s", loaded.Auth.Keys, fr.APIKey)
	}
	if loaded.Auth.AdminToken != fr.AdminToken {
		t.Errorf("loaded admin token = %q, want %q", loaded.Auth.AdminToken, fr.AdminToken)
	}
	if len(loaded.Nodes) != 1 || loaded.Nodes[0].URL != srv.URL {
		t.Errorf("loaded nodes = %+v, want one node at %s", loaded.Nodes, srv.URL)
	}
}

// main.go relies on errors.Is(err, os.ErrNotExist) to detect first-run mode.
func TestLoadConfigMissingFileIsNotExist(t *testing.T) {
	_, err := LoadConfig(filepath.Join(t.TempDir(), "does-not-exist.yaml"))
	if err == nil {
		t.Fatal("expected error for missing config file")
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("errors.Is(err, os.ErrNotExist) = false for %v", err)
	}
}
