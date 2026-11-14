package admin

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestBenchmarkJobContext_DeadlineJustUnderKeyTTL(t *testing.T) {
	ctx, cancel := benchmarkJobContext(10)
	defer cancel()
	dl, ok := ctx.Deadline()
	if !ok {
		t.Fatal("benchmark job context has no deadline")
	}
	ttl := benchmarkKeyTTLFor(10, benchmarkSampleTimeout, nodeUnloadModelTimeout)
	want := ttl - benchmarkJobSlack
	got := time.Until(dl)
	if got > want || got < want-time.Minute {
		t.Fatalf("deadline in %v, want about %v", got, want)
	}
	if got >= ttl {
		t.Fatalf("deadline %v must be shorter than key TTL %v", got, ttl)
	}
}

func TestSleepCtx_ReturnsEarlyOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	if sleepCtx(ctx, time.Minute) {
		t.Fatal("sleepCtx reported a full wait on a cancelled context")
	}
	if time.Since(start) > 5*time.Second {
		t.Fatal("sleepCtx did not return promptly")
	}
}

func TestRunBenchmarkJob_PanicIsRecoveredAndCleansUp(t *testing.T) {
	s := newBenchTestServer(t, "http://localhost:11434", "http://localhost:1", "llama3:8b")
	keyName := "benchmark-panic"
	if err := s.persistKey(probeKey(keyName, "secret-value", "llama3:8b", time.Hour)); err != nil {
		t.Fatalf("persistKey: %v", err)
	}
	s.router = nil // forces a nil dereference inside the job
	job := &benchmarkJob{Node: "gpu-0", Model: "llama3:8b", N: 1, Phase: "evicting", keyName: keyName}

	s.runBenchmarkJob(t.Context(), job, "k")

	if snap := job.snapshot(); snap.Phase != "error" {
		t.Fatalf("phase = %q, want error after a recovered panic", snap.Phase)
	}
	keys, err := s.st.AllKeys()
	if err != nil {
		t.Fatalf("AllKeys: %v", err)
	}
	for _, k := range keys {
		if k.Name == keyName {
			t.Fatal("benchmark key still stored after the job panicked")
		}
	}
}

func TestRunBenchmarkJob_HungNodeEndsInTimeoutError(t *testing.T) {
	release := make(chan struct{})
	hung := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(func() { close(release); hung.Close() })
	s := newBenchTestServer(t, hung.URL, hung.URL, "llama3:8b")
	job := &benchmarkJob{Node: "gpu-0", Model: "llama3:8b", N: 1, Phase: "evicting", keyName: "benchmark-hung"}
	s.benchJobs["j1"] = job

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); s.runBenchmarkJob(ctx, job, "k") }()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("job did not end after its deadline")
	}

	snap := job.snapshot()
	if snap.Phase != "error" || !strings.Contains(snap.Error, "timed out") {
		t.Fatalf("phase=%q error=%q, want terminal error mentioning timed out", snap.Phase, snap.Error)
	}
	if snap.FinishedAt.IsZero() {
		t.Fatal("FinishedAt not set")
	}
	// A terminal job no longer blocks a new run on the same node.
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, newBenchRunRequest(t, s, `{"node":"gpu-0","model":"llama3:8b","n":1}`))
	if rec.Code == http.StatusConflict {
		t.Fatal("node still conflicts after the job timed out")
	}
}

func TestPullJob_RecoverPanicMarksFailed(t *testing.T) {
	job := &pullJob{Status: "downloading"}
	func() {
		defer job.recoverPanic()
		panic("boom")
	}()
	if snap := job.snapshot(); snap.Status != "failed" {
		t.Fatalf("status = %q, want failed after a recovered panic", snap.Status)
	}
}

func TestHandleRunBenchmark_OversizedBodyIs413(t *testing.T) {
	s := newBenchTestServer(t, "http://localhost:11434", "", "llama3:8b")
	pad := strings.Repeat("a", benchmarkRunBodyLimit+1)
	body := `{"node":"gpu-0","model":"llama3:8b","pad":"` + pad + `"}`
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, newBenchRunRequest(t, s, body))
	if c := w.Result().StatusCode; c != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", c)
	}
}

func TestBenchmarkJob_SetPhaseDoesNotResurrectCancelledJob(t *testing.T) {
	s := newBenchTestServer(t, "http://localhost:11434", "", "llama3:8b")
	job := &benchmarkJob{Node: "gpu-0", Model: "llama3:8b", N: 1, Phase: "cold", cancel: func() {}}
	s.benchJobs["j1"] = job

	if !job.requestCancel() {
		t.Fatal("requestCancel reported no running job")
	}
	job.finish("cancelled", "", nil)
	job.setPhase("cold")
	job.setPhase("warm")

	if snap := job.snapshot(); snap.Phase != "cancelled" {
		t.Fatalf("phase = %q, want cancelled to stay terminal", snap.Phase)
	}
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, newBenchRunRequest(t, s, `{"node":"gpu-0","model":"llama3:8b","n":1}`))
	if rec.Code == http.StatusConflict {
		t.Fatal("node still blocked by a cancelled job")
	}
}

func TestRunBenchmarkJob_DeadlineDuringSettleSleepEndsInTimeoutError(t *testing.T) {
	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(ok.Close)
	s := newBenchTestServer(t, ok.URL, ok.URL, "llama3:8b")
	job := &benchmarkJob{Node: "gpu-0", Model: "llama3:8b", N: 2, Phase: "evicting", keyName: "benchmark-settle"}
	s.benchJobs["j1"] = job

	// The deadline is shorter than benchmarkKeySettle, so it fires while the
	// job waits between the evict and the first sample.
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	s.runBenchmarkJob(ctx, job, "k")

	if time.Since(start) >= benchmarkKeySettle {
		t.Fatalf("job ran %v, expected the deadline to cut the settle sleep short", time.Since(start))
	}
	snap := job.snapshot()
	if snap.Phase != "error" || !strings.Contains(snap.Error, "timed out") {
		t.Fatalf("phase=%q error=%q, want terminal error mentioning timed out", snap.Phase, snap.Error)
	}
}
