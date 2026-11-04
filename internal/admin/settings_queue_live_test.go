package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/Anirudhx7/marbor/internal/auth"
	"github.com/Anirudhx7/marbor/internal/config"
	"github.com/Anirudhx7/marbor/internal/router"
	"github.com/Anirudhx7/marbor/internal/store"
)

// queueSettingsServer returns a server whose router has one unhealthy node
// (so every WaitForNode has to wait) and a deliberately long queue timeout.
func queueSettingsServer(t *testing.T, st ...store.Store) (*Server, *router.Router) {
	t.Helper()
	rc := config.RoutingConfig{
		PollIntervalMs: 2000, UpstreamTimeoutMs: 120000, MaxRetries: 2,
		SessionAffinityTTL: "10m", NvidiaPollIntervalMs: 30000,
		QueueMaxDepth: 10, QueueTimeoutMs: 5000,
	}
	r := router.New(rc, []config.NodeConfig{{Name: "n1", URL: "http://localhost:11434"}}, nil)
	r.Nodes()[0].Healthy = false
	cfg := config.Config{}
	cfg.Routing = rc
	return NewServer(r, auth.NewMiddleware(cfg.Auth), cfg, st...), r
}

func putRoutingSettings(t *testing.T, s *Server, routing map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	m := getSettingsMap(t, s)
	var current map[string]any
	if err := json.Unmarshal(m["routing"], &current); err != nil {
		t.Fatal(err)
	}
	for k, v := range routing {
		current[k] = v
	}
	raw, _ := json.Marshal(current)
	m["routing"] = raw
	body, _ := json.Marshal(m)
	req := httptest.NewRequest(http.MethodPut, "/admin/settings", bytes.NewReader(body))
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: s.AdminToken()})
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec
}

// waitDurationCloud waits as a request that may use cloud, against a healthy
// node that is full (the SLA only caps a wait when cloud can take the request).
func waitDurationCloud(r *router.Router) time.Duration {
	n := r.Nodes()[0]
	n.Lock()
	n.Healthy = true
	n.MaxInFlight = 1
	n.Unlock()
	if n.ActiveConns == 0 {
		r.IncrConn(n)
	}
	start := time.Now()
	r.WaitForNodeWithPrefixOpts(context.Background(), "llama3.2", "", "", "", router.WaitOpts{CloudUsable: true})
	return time.Since(start)
}

func waitDuration(r *router.Router) time.Duration {
	start := time.Now()
	r.WaitForNode(context.Background(), "llama3.2", "", "")
	return time.Since(start)
}

// Saving the overflow SLA through the settings API must change how long the
// next request waits, with no restart.
func TestAdmin_SettingsSaveAppliesOverflowSLALive(t *testing.T) {
	s, r := queueSettingsServer(t)
	rec := putRoutingSettings(t, s, map[string]any{"overflow_sla_ms": 150})
	if rec.Code >= 300 {
		t.Fatalf("PUT settings = %d: %s", rec.Code, rec.Body.String())
	}
	if d := waitDurationCloud(r); d < 100*time.Millisecond || d > 1500*time.Millisecond {
		t.Fatalf("waited %v after saving overflow_sla_ms=150, want about 150ms", d)
	}
}

// Saving the queue timeout and depth also takes effect on the next request.
func TestAdmin_SettingsSaveAppliesQueueTimeoutAndDepthLive(t *testing.T) {
	s, r := queueSettingsServer(t)
	if rec := putRoutingSettings(t, s, map[string]any{"queue_timeout_ms": 150}); rec.Code >= 300 {
		t.Fatalf("PUT settings = %d: %s", rec.Code, rec.Body.String())
	}
	if d := waitDuration(r); d < 100*time.Millisecond || d > 1500*time.Millisecond {
		t.Fatalf("waited %v after saving queue_timeout_ms=150, want about 150ms", d)
	}
	// Depth 0 is normalised to the default by validation, so use a real value:
	// a negative depth must be refused rather than silently stored.
	if rec := putRoutingSettings(t, s, map[string]any{"queue_max_depth": -1}); rec.Code != http.StatusBadRequest {
		t.Fatalf("negative queue_max_depth = %d, want 400", rec.Code)
	}
}

func TestAdmin_SettingsRejectsNegativeQueueValues(t *testing.T) {
	for _, key := range []string{"queue_timeout_ms", "queue_max_depth", "overflow_sla_ms"} {
		s, r := queueSettingsServer(t)
		rec := putRoutingSettings(t, s, map[string]any{key: -5})
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s=-5: status %d, want 400", key, rec.Code)
		}
		// A refused save must leave the live wait untouched (still the 5s
		// timeout, so a 300ms probe context ends first).
		ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
		start := time.Now()
		r.WaitForNode(ctx, "llama3.2", "", "")
		cancel()
		if d := time.Since(start); d < 250*time.Millisecond {
			t.Errorf("%s=-5: wait ended after %v, live settings changed by a refused save", key, d)
		}
	}
}

// A reload re-applies the configured values and keeps them live.
func TestAdmin_ReloadFromStoreKeepsQueueSettingsApplied(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "reload-queue.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	s, r := queueSettingsServer(t, st)
	s.mu.Lock()
	s.cfg.Routing.QueueTimeoutMs = 150
	s.mu.Unlock()
	if _, _, _, _, err := s.ReloadFromStore(); err != nil {
		t.Fatalf("ReloadFromStore: %v", err)
	}
	if d := waitDuration(r); d < 100*time.Millisecond || d > 1500*time.Millisecond {
		t.Fatalf("waited %v after reload with queue timeout 150, want about 150ms", d)
	}
}
