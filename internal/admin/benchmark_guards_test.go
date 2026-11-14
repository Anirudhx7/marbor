package admin

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestBenchmarkJobContext_HasDeadlineMatchingKeyTTL(t *testing.T) {
	ctx, cancel := benchmarkJobContext(10)
	defer cancel()
	dl, ok := ctx.Deadline()
	if !ok {
		t.Fatal("benchmark job context has no deadline")
	}
	want := benchmarkKeyTTLFor(10, benchmarkSampleTimeout, nodeUnloadModelTimeout)
	if got := time.Until(dl); got > want || got < want-time.Minute {
		t.Fatalf("deadline in %v, want about %v", got, want)
	}
}

func TestRunBenchmarkJob_PanicIsRecoveredAndCleansUp(t *testing.T) {
	s := newBenchTestServer(t, "http://localhost:11434", "http://localhost:1", "llama3:8b")
	s.router = nil // forces a nil dereference inside the job
	job := &benchmarkJob{Node: "gpu-0", Model: "llama3:8b", N: 1, Phase: "evicting", keyName: "benchmark-panic"}

	s.runBenchmarkJob(t.Context(), job, "k")

	if snap := job.snapshot(); snap.Phase != "error" {
		t.Fatalf("phase = %q, want error after a recovered panic", snap.Phase)
	}
}

func TestHandleRunBenchmark_RejectsOversizedBody(t *testing.T) {
	s := newBenchTestServer(t, "http://localhost:11434", "", "llama3:8b")
	body := `{"node":"` + strings.Repeat("a", 1<<17) + `","model":"llama3:8b"}`
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, newBenchRunRequest(t, s, body))
	if c := w.Result().StatusCode; c != http.StatusBadRequest && c != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 400 or 413", c)
	}
}
