package router

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestStatusErrorIncludesRuntimeDetail(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{"ollama error body", `{"error":"model requires more system memory (9.8 GiB) than is available (6.0 GiB)"}`, "returned 500: model requires more system memory (9.8 GiB) than is available (6.0 GiB)"},
		{"empty body", ``, "returned 500"},
		{"not json", `<html>boom</html>`, "returned 500"},
		{"json without error", `{"status":"x"}`, "returned 500"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := &statusError{status: 500, detail: runtimeErrorDetail(strings.NewReader(tc.body))}
			if got := err.Error(); got != tc.want {
				t.Fatalf("Error() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestTriggerWarmupSurvivesCancelledCaller guards the manual Ping button: the
// admin handler passes its request context, which is cancelled as soon as the
// response is written, and the background warmup must still complete.
func TestTriggerWarmupSurvivesCancelledCaller(t *testing.T) {
	calls := make(chan struct{}, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(100 * time.Millisecond) // still in flight when the caller cancels
		if r.Context().Err() == nil {
			calls <- struct{}{}
		}
		w.Write([]byte(`{"done":true}`))
	}))
	defer srv.Close()

	r := &Router{
		client:     &http.Client{Timeout: 5 * time.Second},
		nodes:      []*NodeState{{Name: "n1", URL: srv.URL, Healthy: true}},
		nodeWarmup: map[string]NodeWarmup{"n1": {Enabled: true, Models: []string{"llama3.2"}}},
	}
	ctx, cancel := context.WithCancel(context.Background())
	r.TriggerWarmup(ctx)
	cancel() // the handler returned
	select {
	case <-calls:
	case <-time.After(3 * time.Second):
		t.Fatal("warmup ping was aborted by the caller's cancelled context")
	}
}

func TestRuntimeErrorDetailIsBounded(t *testing.T) {
	// A huge body must not be read in full; truncated JSON is simply not parsed.
	body := `{"error":"` + strings.Repeat("x", 100000) + `"}`
	if got := runtimeErrorDetail(strings.NewReader(body)); got != "" {
		t.Fatalf("oversized body should yield no detail, got %d chars", len(got))
	}
}
