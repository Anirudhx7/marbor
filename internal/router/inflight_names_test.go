package router

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// A request for a bare name and a resident reported with ":latest" (and the
// reverse) are the same model, so an in-flight request must protect it.
func TestEvictForHeadroom_InFlightProtectsEquivalentName(t *testing.T) {
	cases := []struct {
		name, inFlightKey, resident string
	}{
		{"bare request, tagged resident", "protected", "protected:latest"},
		{"tagged request, bare resident", "protected:latest", "protected"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var mu sync.Mutex
			var unloaded []string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				b, _ := io.ReadAll(req.Body)
				mu.Lock()
				unloaded = append(unloaded, string(b))
				mu.Unlock()
				w.Write([]byte(`{"done":true}`))
			}))
			defer srv.Close()
			r := &Router{
				nodes: []*NodeState{{
					Name: "n1", URL: srv.URL, Healthy: true, VRAMTotalMB: 120,
					LoadedModels: []ModelInfo{
						{Name: tc.resident, SizeVRAM: 60 * mib},
						{Name: "idle", SizeVRAM: 40 * mib},
					},
					modelInFlight: map[string]int32{tc.inFlightKey: 1},
				}},
				lastUsed: map[string]time.Time{},
				pinned:   map[string]map[string]bool{},
			}
			// The in-flight model is the coldest; without protection it goes first.
			r.lastUsed[modelKey("n1", tc.resident)] = time.Now().Add(-3 * time.Hour)
			r.lastUsed[modelKey("n1", "idle")] = time.Now().Add(-time.Minute)
			r.EvictForHeadroom(context.Background(), "n1", "newmodel", 30*mib)
			mu.Lock()
			defer mu.Unlock()
			for _, b := range unloaded {
				if strings.Contains(b, `"`+tc.resident+`"`) {
					t.Fatalf("in-flight model %q was unloaded: %s", tc.resident, b)
				}
			}
			if len(unloaded) == 0 {
				t.Fatal("expected the idle model to be evicted instead")
			}
		})
	}
}

// A model that starts serving after the victim snapshot was taken, while an
// earlier unload was still running, must not be unloaded.
func TestEvictForHeadroom_SkipsVictimThatGainedInFlightAfterSnapshot(t *testing.T) {
	var mu sync.Mutex
	var unloaded []string
	var r *Router
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		b, _ := io.ReadAll(req.Body)
		mu.Lock()
		unloaded = append(unloaded, string(b))
		mu.Unlock()
		if strings.Contains(string(b), `"first"`) {
			// While the first unload is running, a request for "second" arrives.
			r.IncrModelInFlight(r.nodes[0], "second")
		}
		w.Write([]byte(`{"done":true}`))
	}))
	defer srv.Close()
	r = &Router{
		nodes: []*NodeState{{
			Name: "n1", URL: srv.URL, Healthy: true, VRAMTotalMB: 200,
			LoadedModels: []ModelInfo{
				{Name: "first", SizeVRAM: 40 * mib},
				{Name: "second:latest", SizeVRAM: 40 * mib},
				{Name: "third", SizeVRAM: 40 * mib},
			},
		}},
		lastUsed: map[string]time.Time{},
		pinned:   map[string]map[string]bool{},
	}
	now := time.Now()
	r.lastUsed[modelKey("n1", "first")] = now.Add(-3 * time.Hour)
	r.lastUsed[modelKey("n1", "second:latest")] = now.Add(-2 * time.Hour)
	r.lastUsed[modelKey("n1", "third")] = now.Add(-time.Hour)
	r.EvictForHeadroom(context.Background(), "n1", "newmodel", 120*mib)
	mu.Lock()
	defer mu.Unlock()
	for _, b := range unloaded {
		if strings.Contains(b, "second") {
			t.Fatalf("model that gained an in-flight request was unloaded: %s", b)
		}
	}
	found := false
	for _, b := range unloaded {
		if strings.Contains(b, `"third"`) {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected selection to continue to the next victim, unloads: %v", unloaded)
	}
}
