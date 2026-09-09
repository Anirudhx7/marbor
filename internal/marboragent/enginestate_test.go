package marboragent

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func testClient() *http.Client { return &http.Client{Timeout: 2 * time.Second} }

func floatEq(t *testing.T, got *float64, want float64) {
	t.Helper()
	if got == nil {
		t.Fatalf("got nil, want %v", want)
	}
	if *got != want {
		t.Fatalf("got %v, want %v", *got, want)
	}
}

func intEq(t *testing.T, got *int, want int) {
	t.Helper()
	if got == nil {
		t.Fatalf("got nil, want %v", want)
	}
	if *got != want {
		t.Fatalf("got %v, want %v", *got, want)
	}
}

// TestCollectVLLMEngineState_Success covers requirement 1: a successful
// scrape populates every applicable field, including the V1-engine KV-cache
// metric name.
func TestCollectVLLMEngineState_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`# HELP vllm:num_requests_running running
# TYPE vllm:num_requests_running gauge
vllm:num_requests_running{model_name="m"} 3
vllm:num_requests_waiting{model_name="m"} 1
vllm:kv_cache_usage_perc{model_name="m"} 0.42
`))
	}))
	defer srv.Close()

	es := collectEngineState(context.Background(), testClient(), "vllm", srv.URL)
	if es == nil {
		t.Fatal("EngineState = nil, want populated")
	}
	intEq(t, es.RunningRequests, 3)
	intEq(t, es.WaitingRequests, 1)
	floatEq(t, es.KVCacheUsagePercent, 42)
}

// TestCollectVLLMEngineState_LegacyKVName covers the legacy-engine metric
// name fallback (vllm:gpu_cache_usage_perc).
func TestCollectVLLMEngineState_LegacyKVName(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("vllm:gpu_cache_usage_perc 0.5\n"))
	}))
	defer srv.Close()

	es := collectEngineState(context.Background(), testClient(), "vllm", srv.URL)
	if es == nil {
		t.Fatal("EngineState = nil, want populated")
	}
	floatEq(t, es.KVCacheUsagePercent, 50)
}

// TestCollectVLLMEngineState_RealZero covers requirement 2: a genuinely
// reported zero must come back as a non-nil pointer to 0, not nil.
func TestCollectVLLMEngineState_RealZero(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("vllm:num_requests_running 0\nvllm:num_requests_waiting 0\n"))
	}))
	defer srv.Close()

	es := collectEngineState(context.Background(), testClient(), "vllm", srv.URL)
	if es == nil {
		t.Fatal("EngineState = nil, want populated (real zeros)")
	}
	intEq(t, es.RunningRequests, 0)
	intEq(t, es.WaitingRequests, 0)
	if es.KVCacheUsagePercent != nil {
		t.Errorf("KVCacheUsagePercent = %v, want nil (metric absent)", *es.KVCacheUsagePercent)
	}
}

// TestCollectVLLMEngineState_MissingMetric covers requirement 3: a metric
// this scrape simply didn't include must be nil, never a substituted value.
func TestCollectVLLMEngineState_MissingMetric(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("vllm:num_requests_running 5\n"))
	}))
	defer srv.Close()

	es := collectEngineState(context.Background(), testClient(), "vllm", srv.URL)
	if es == nil {
		t.Fatal("EngineState = nil, want populated")
	}
	intEq(t, es.RunningRequests, 5)
	if es.WaitingRequests != nil {
		t.Errorf("WaitingRequests = %v, want nil", *es.WaitingRequests)
	}
	if es.KVCacheUsagePercent != nil {
		t.Errorf("KVCacheUsagePercent = %v, want nil", *es.KVCacheUsagePercent)
	}
}

// TestCollectVLLMEngineState_MalformedMetric covers requirement 4: a value
// that fails to parse as a float must be treated as absent, not a crash or
// a fabricated substitute.
func TestCollectVLLMEngineState_MalformedMetric(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("vllm:num_requests_running not-a-number\nvllm:num_requests_waiting 2\n"))
	}))
	defer srv.Close()

	es := collectEngineState(context.Background(), testClient(), "vllm", srv.URL)
	if es == nil {
		t.Fatal("EngineState = nil, want populated (one good metric present)")
	}
	if es.RunningRequests != nil {
		t.Errorf("RunningRequests = %v, want nil (malformed line skipped)", *es.RunningRequests)
	}
	intEq(t, es.WaitingRequests, 2)
}

// TestCollectEngineState_EndpointUnreachable covers requirement 5: a
// scrape failure (connection refused) must yield nil, not a partial or
// crashing result, and must not be treated as a telemetry-cycle failure by
// the caller (scheduler.refresh already wraps this call in its own
// independent timeout/recover).
func TestCollectEngineState_EndpointUnreachable(t *testing.T) {
	// Deliberately unroutable: a closed local server, guaranteed refused.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := srv.URL
	srv.Close()

	es := collectEngineState(context.Background(), testClient(), "vllm", url)
	if es != nil {
		t.Errorf("EngineState = %+v, want nil on unreachable endpoint", es)
	}
}

// TestCollectEngineState_UnsupportedRuntime covers requirement 7: a runtime
// with no verified native metrics source must never have anything
// fabricated for it - nil, unconditionally, with no HTTP call attempted.
func TestCollectEngineState_UnsupportedRuntime(t *testing.T) {
	for _, name := range []string{"ollama", "llamacpp", "mlx", "unknown-future-runtime"} {
		if es := collectEngineState(context.Background(), testClient(), name, "http://127.0.0.1:1"); es != nil {
			t.Errorf("runtime %q: EngineState = %+v, want nil", name, es)
		}
	}
}

// TestCollectTGIEngineState_OnlyVerifiedFieldsMapped covers requirement 8:
// TGI's running/waiting gauges are mapped by documented semantics, but the
// KV-cache field must stay nil since TGI exposes no verified equivalent -
// never estimated from an unrelated gauge like tgi_batch_current_max_tokens.
func TestCollectTGIEngineState_OnlyVerifiedFieldsMapped(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`tgi_batch_current_size 4
tgi_queue_size 2
tgi_batch_current_max_tokens 8192
`))
	}))
	defer srv.Close()

	es := collectEngineState(context.Background(), testClient(), "tgi", srv.URL)
	if es == nil {
		t.Fatal("EngineState = nil, want populated")
	}
	intEq(t, es.RunningRequests, 4)
	intEq(t, es.WaitingRequests, 2)
	if es.KVCacheUsagePercent != nil {
		t.Errorf("KVCacheUsagePercent = %v, want nil (TGI has no verified equivalent)", *es.KVCacheUsagePercent)
	}
}
