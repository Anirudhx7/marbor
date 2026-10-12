package admin

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestWarmupPing_WorksWithoutGlobalWarmup guards the manual Ping button: keep-warm
// is also configured per node, independently of the global warmup setting, so a
// ping must be accepted even when the global flag is off.
func TestWarmupPing_WorksWithoutGlobalWarmup(t *testing.T) {
	ollama := mockOllamaServer(t)
	defer ollama.Close()
	s := newModelFitTestServer(ollama.URL)
	if s.cfg.Warmup.Enabled {
		t.Fatal("test precondition: global warmup should be off")
	}

	req := httptest.NewRequest(http.MethodPost, "/admin/warmup/ping", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: s.AdminToken()})
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s; want 200 with global warmup off", w.Code, w.Body.String())
	}
}
