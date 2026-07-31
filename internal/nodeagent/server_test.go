package nodeagent

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestServerStatusRequiresToken(t *testing.T) {
	srv := &Server{Token: "sekret", Version: "v-test"}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/v1/status", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("no-token request: status = %d, want 401", resp.StatusCode)
	}
}

func TestServerStatusWithValidToken(t *testing.T) {
	srv := &Server{Token: "sekret", Version: "v-test"}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/v1/status", nil)
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
	if tel.Agent.ProtocolVersion != ProtocolVersion {
		t.Errorf("agent.protocol_version = %d, want %d", tel.Agent.ProtocolVersion, ProtocolVersion)
	}
	if tel.Agent.Version != "v-test" {
		t.Errorf("agent.version = %q, want v-test", tel.Agent.Version)
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
	if !strings.Contains(out, "nodeagent_protocol_version") {
		t.Errorf("metrics output missing nodeagent_protocol_version:\n%s", out)
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

// TestServerServesCachedSnapshotBetweenRequests proves /v1/status reads the
// Scheduler's cache rather than collecting fresh on every request: with the
// background refresh loop never started, two requests in a row must report
// the exact same LastUpdated timestamp from the single Seed() collection.
func TestServerServesCachedSnapshotBetweenRequests(t *testing.T) {
	scheduler := newSchedulerWithBackends("v-test", fakeGPUCollector{}, fakeHostCollector{telemetry: &HostTelemetry{}}, noRuntimeDetector)
	scheduler.Seed()
	srv := &Server{Token: "sekret", Version: "v-test"}
	srv.SetScheduler(scheduler)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	get := func() Telemetry {
		req, _ := http.NewRequest(http.MethodGet, ts.URL+"/v1/status", nil)
		req.Header.Set("Authorization", "Bearer sekret")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("request: %v", err)
		}
		defer resp.Body.Close()
		var tel Telemetry
		if err := json.NewDecoder(resp.Body).Decode(&tel); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return tel
	}

	first := get()
	if first.LastUpdated.IsZero() {
		t.Fatal("expected LastUpdated to be set from Seed()")
	}
	time.Sleep(20 * time.Millisecond)
	second := get()
	if !second.LastUpdated.Equal(first.LastUpdated) {
		t.Errorf("LastUpdated changed between requests with no refresh running (first=%v, second=%v) - handler must be re-collecting instead of reading the cache", first.LastUpdated, second.LastUpdated)
	}
}

// TestServerFallsBackWithoutScheduler verifies a Server built without a
// Scheduler (its zero value) still serves a live reading rather than
// panicking - keeps the type usable in any test/caller that predates the
// caching change.
func TestServerFallsBackWithoutScheduler(t *testing.T) {
	srv := &Server{Token: "sekret", Version: "v-test"}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/v1/status", nil)
	req.Header.Set("Authorization", "Bearer sekret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	var tel Telemetry
	if err := json.NewDecoder(resp.Body).Decode(&tel); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if tel.LastUpdated.IsZero() {
		t.Error("expected a live LastUpdated even without a Scheduler wired up")
	}
}
