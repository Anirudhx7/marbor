package marboragent

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
	if !strings.Contains(out, "marboragent_protocol_version") {
		t.Errorf("metrics output missing marboragent_protocol_version:\n%s", out)
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

// TestServerRouteScopeGating is the P54 route-table regression: every
// registered route is exercised with both a readonly-scoped and an
// admin-scoped token. It only asserts on the scope GATE (401 for a bad
// token, 403 for insufficient scope, "neither of those" for sufficient
// scope) - not on what the underlying handler does with an empty/malformed
// body, which existing per-handler tests elsewhere already cover using a
// bare unprefixed token (itself proof that legacy, pre-P54 tokens keep
// working exactly as before, since scopeOf falls back to tierAdmin for
// them).
func TestServerRouteScopeGating(t *testing.T) {
	const readonlyToken = "readonly.Xk9fA1b2C3d4"
	const adminToken = "admin.Xk9fA1b2C3d4"

	routes := []struct {
		method           string
		path             string
		requiresOperator bool // false = readonly-tier route, true = operator-tier (today's admin-scoped test token covers both since admin >= operator >= readonly)
	}{
		{http.MethodGet, "/v1/status", false},
		{http.MethodGet, "/metrics", false},
		{http.MethodPost, "/v1/models", true},
		{http.MethodGet, "/v1/models", true},
		{http.MethodDelete, "/v1/models/some-model", true},
		{http.MethodPost, "/v1/models/some-model", true},
		{http.MethodGet, "/v1/runtime/health", true},
		{http.MethodPost, "/v1/runtime/start", true},
		{http.MethodPost, "/v1/runtime/stop", true},
		{http.MethodPost, "/v1/runtime/restart", true},
		{http.MethodPost, "/v1/runtime/logs", true},
		{http.MethodPost, "/v1/runtime/disk", true},
	}

	for _, tokenCase := range []struct {
		name  string
		token string
	}{
		{"readonly token", readonlyToken},
		{"admin token", adminToken},
	} {
		func() {
			srv := &Server{Token: tokenCase.token, Version: "v-test"}
			ts := httptest.NewServer(srv.Handler())
			defer ts.Close()

			for _, rt := range routes {
				t.Run(tokenCase.name+" "+rt.method+" "+rt.path, func(t *testing.T) {
					req, _ := http.NewRequest(rt.method, ts.URL+rt.path, nil)
					req.Header.Set("Authorization", "Bearer "+tokenCase.token)
					resp, err := http.DefaultClient.Do(req)
					if err != nil {
						t.Fatalf("request: %v", err)
					}
					defer resp.Body.Close()

					isReadonlyToken := tokenCase.token == readonlyToken
					if isReadonlyToken && rt.requiresOperator {
						if resp.StatusCode != http.StatusForbidden {
							t.Errorf("status = %d, want 403 (readonly token on an operator-tier route)", resp.StatusCode)
						}
						return
					}
					if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
						t.Errorf("status = %d, want the scope gate to pass (whatever the handler itself returns next)", resp.StatusCode)
					}
				})
			}
		}()
	}
}
