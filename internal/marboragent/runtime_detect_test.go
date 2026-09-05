package marboragent

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"
	"time"
)

// fakeOllamaServer answers /api/ps with 200, the same signature
// internal/runtime.DetectRuntime uses to identify Ollama - good enough to
// drive DetectAll's accumulation logic without depending on a real runtime
// being installed on the machine running the test.
func fakeOllamaServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/ps" {
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"models":[]}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func portOf(t *testing.T, rawURL string) int {
	t.Helper()
	u, err := url.Parse(rawURL)
	if err != nil {
		t.Fatalf("url.Parse(%q): %v", rawURL, err)
	}
	p, err := strconv.Atoi(u.Port())
	if err != nil {
		t.Fatalf("port of %q: %v", rawURL, err)
	}
	return p
}

// TestDetectAllAccumulatesMultipleRuntimes is the regression test for the
// reported bug's second half: two runtimes on the same host must both be
// reported, not just the first one found (the old Detect loop returned on
// the first hit).
func TestDetectAllAccumulatesMultipleRuntimes(t *testing.T) {
	s1, s2 := fakeOllamaServer(t), fakeOllamaServer(t)

	orig := localRuntimePorts
	localRuntimePorts = []string{s1.URL, s2.URL}
	t.Cleanup(func() { localRuntimePorts = orig })

	d := newLocalhostRuntimeDetector()
	all := d.DetectAll(context.Background())
	if len(all) != 2 {
		t.Fatalf("DetectAll returned %d runtimes, want 2 (got %+v)", len(all), all)
	}
	if all[0].URL != s1.URL || all[0].Port != portOf(t, s1.URL) {
		t.Errorf("all[0] = %+v, want URL=%s Port=%d", all[0], s1.URL, portOf(t, s1.URL))
	}
	if all[1].URL != s2.URL || all[1].Port != portOf(t, s2.URL) {
		t.Errorf("all[1] = %+v, want URL=%s Port=%d", all[1], s2.URL, portOf(t, s2.URL))
	}

	// Detect (the legacy single-result method) must still return the first
	// of the two - preserves the "primary runtime" back-compat contract.
	name, gotURL, found := d.Detect(context.Background())
	if !found || gotURL != s1.URL || name != "ollama" {
		t.Errorf("Detect() = (%q, %q, %v), want (ollama, %s, true)", name, gotURL, found, s1.URL)
	}
}

// TestDetectAllOmitsWhenNothingListening verifies "nothing found" stays a
// true empty result, never a fabricated entry, when every candidate
// port is unreachable.
func TestDetectAllOmitsWhenNothingListening(t *testing.T) {
	orig := localRuntimePorts
	// Ports very unlikely to have anything listening, with a short client
	// timeout so the test fails fast rather than waiting out a long dial
	// timeout on a closed port.
	localRuntimePorts = []string{"http://127.0.0.1:1", "http://127.0.0.1:2"}
	t.Cleanup(func() { localRuntimePorts = orig })

	d := localhostRuntimeDetector{client: &http.Client{Timeout: 500 * time.Millisecond}}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if all := d.DetectAll(ctx); len(all) != 0 {
		t.Errorf("DetectAll = %+v, want empty when nothing is listening", all)
	}
}
