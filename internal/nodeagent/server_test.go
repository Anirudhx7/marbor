package nodeagent

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestServerTelemetryRequiresToken(t *testing.T) {
	srv := &Server{Token: "sekret", Version: "v-test"}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/telemetry", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("no-token request: status = %d, want 401", resp.StatusCode)
	}
}

func TestServerTelemetryWithValidToken(t *testing.T) {
	srv := &Server{Token: "sekret", Version: "v-test"}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/telemetry", nil)
	req.Header.Set("Authorization", "Bearer sekret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var tel Telemetry
	if err := json.NewDecoder(resp.Body).Decode(&tel); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if tel.SchemaVersion != SchemaVersion {
		t.Errorf("schema_version = %d, want %d", tel.SchemaVersion, SchemaVersion)
	}
	if tel.AgentVersion != "v-test" {
		t.Errorf("agent_version = %q, want v-test", tel.AgentVersion)
	}
}

func TestServerMetricsWithValidToken(t *testing.T) {
	srv := &Server{Token: "sekret", Version: "v-test"}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/metrics", nil)
	req.Header.Set("Authorization", "Bearer sekret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body := make([]byte, 4096)
	n, _ := resp.Body.Read(body)
	out := string(body[:n])
	if !strings.Contains(out, "nodeagent_schema_version") {
		t.Errorf("metrics output missing nodeagent_schema_version:\n%s", out)
	}
}

func TestServerMetricsRequiresToken(t *testing.T) {
	srv := &Server{Token: "sekret", Version: "v-test"}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/metrics", nil)
	req.Header.Set("Authorization", "Bearer wrong-token")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", resp.StatusCode)
	}
}
