package metrics

import (
	"sync"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// maxModelLabels caps how many distinct model label values the metrics carry.
// Model names come from client request bodies and are otherwise unbounded, which
// would explode Prometheus time-series memory. Past the cap, unseen models
// collapse to "other".
const maxModelLabels = 256

var (
	modelLabelMu sync.Mutex
	seenModels   = make(map[string]struct{})
)

func boundModel(model string) string {
	if model == "" {
		return "unknown"
	}
	modelLabelMu.Lock()
	defer modelLabelMu.Unlock()
	if _, ok := seenModels[model]; ok {
		return model
	}
	if len(seenModels) >= maxModelLabels {
		return "other"
	}
	seenModels[model] = struct{}{}
	return model
}

var (
	requestsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "marbor_requests_total",
		Help: "Total requests proxied",
	}, []string{"key_name", "model", "node", "status"})

	requestDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "marbor_request_duration_seconds",
		Help:    "Request duration in seconds",
		Buckets: prometheus.DefBuckets,
	}, []string{"model", "node"})

	requestTTFT = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name: "marbor_request_ttft_seconds",
		Help: "Time to first response byte, from real Write() timing (cold-start vs warm-residency signal)",
		// Finer-grained than DefBuckets in the sub-second range where TTFT
		// actually differentiates warm (fast) from cold-loading (slow) nodes.
		Buckets: []float64{.05, .1, .25, .5, 1, 2.5, 5, 10, 30, 60},
	}, []string{"model", "node"})

	activeConns = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "marbor_active_connections",
		Help: "Current active connections per node",
	}, []string{"node"})

	nodeHealthy = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "marbor_node_healthy",
		Help: "Node health status (1=healthy, 0=unhealthy)",
	}, []string{"node"})

	warmupResident = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "marbor_warmup_model_resident",
		Help: "Whether a warmup-target model is currently loaded in VRAM on a node (1=resident/warm, 0=cold). Proves warmup is actually keeping models warm, not just pinging.",
	}, []string{"model", "node"})

	scheduleFires = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "marbor_schedule_fires_total",
		Help: "Scheduled actions fired, by action (warmup/drain/undrain) and node.",
	}, []string{"action", "node"})

	modelEvictions = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "marbor_model_evictions_total",
		Help: "Models unloaded from a node's VRAM (LRU headroom eviction, scheduled, or manual), by node.",
	}, []string{"node"})

	cacheHits = promauto.NewCounter(prometheus.CounterOpts{
		Name: "marbor_cache_hits_total",
		Help: "Total cache hits (warm model routing)",
	})

	cacheMisses = promauto.NewCounter(prometheus.CounterOpts{
		Name: "marbor_cache_misses_total",
		Help: "Total cache misses (cold start)",
	})

	tokensTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "marbor_tokens_total",
		Help: "Total tokens processed (best-effort from Ollama responses)",
	}, []string{"key_name", "node"})

	retriesTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "marbor_retries_total",
		Help: "Upstream failover retries (a node failed before sending bytes and another was tried)",
	}, []string{"node"})

	cloudFallbacksTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "marbor_cloud_fallbacks_total",
		Help: "Requests that overflowed to a cloud provider because no local node could serve them",
	}, []string{"provider"})

	localDegradationTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "marbor_local_degradation_total",
		Help: "Requests substituted to a declared local alternate model (opt-in) instead of falling through to cloud",
	}, []string{"from", "to"})

	quotaRejectionsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "marbor_quota_rejections_total",
		Help: "Requests rejected with 429 because a per-key daily or monthly quota was exhausted",
	}, []string{"key_name", "period"})

	panicsTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "marbor_panics_total",
		Help: "Handler panics recovered by the recovery middleware (a non-zero rate is a bug to investigate)",
	})

	queueDepth = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "marbor_queue_depth",
		Help: "Current number of requests queued waiting for a local node to become available",
	})

	queueTimeoutsTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "marbor_queue_timeouts_total",
		Help: "Requests that waited in the queue and timed out without getting a node",
	})

	warmupPingsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "marbor_warmup_pings_total",
		Help: "Proactive keepalive pings sent to prevent model eviction from VRAM",
	}, []string{"model", "node", "status"})

	predictionAccuracyRatio = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "marbor_prediction_accuracy_ratio",
		Help: "Ratio of successful prewarming predictions (0.0 to 1.0)",
	})

	prefixLocalityHitsTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "marbor_prefix_locality_hits_total",
		Help: "Requests whose prefix hash matched a recorded prefix-locality routing hint (Step 6)",
	})

	prefixLocalityMissesTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "marbor_prefix_locality_misses_total",
		Help: "Requests with no matching prefix-locality routing hint (Step 6)",
	})
)

func RequestsTotal(key, model, node, status string) {
	requestsTotal.WithLabelValues(key, boundModel(model), node, status).Inc()
}

func RequestDuration(model, node string, seconds float64) {
	requestDuration.WithLabelValues(boundModel(model), node).Observe(seconds)
}

// RequestTTFT records real time-to-first-byte. Callers must skip this for
// requests where no byte was ever written (immediate error, full abort) -
// there is no real TTFT to report in that case, per R1.
func RequestTTFT(model, node string, seconds float64) {
	requestTTFT.WithLabelValues(boundModel(model), node).Observe(seconds)
}

func ActiveConnections(node string, count float64) {
	activeConns.WithLabelValues(node).Set(count)
}

func NodeHealthy(node string, val float64) {
	nodeHealthy.WithLabelValues(node).Set(val)
}

func CacheHit() {
	cacheHits.Inc()
}

func CacheMiss() {
	cacheMisses.Inc()
}

func TokensTotal(key, node string, count float64) {
	tokensTotal.WithLabelValues(key, node).Add(count)
}

// Retry records that an upstream node failed before any response bytes and the
// proxy moved on to another node.
func Retry(node string) {
	retriesTotal.WithLabelValues(node).Inc()
}

// CloudFallback records a request overflowing to the named cloud provider.
func CloudFallback(provider string) {
	cloudFallbacksTotal.WithLabelValues(provider).Inc()
}

// LocalDegradation records a request substituted from its requested model to
// a declared local alternate (opt-in chain) instead of falling through to
// cloud.
func LocalDegradation(from, to string) {
	localDegradationTotal.WithLabelValues(boundModel(from), boundModel(to)).Inc()
}

// QuotaRejection records a 429 caused by an exhausted per-key quota. period is
// "daily" or "monthly".
func QuotaRejection(key, period string) {
	quotaRejectionsTotal.WithLabelValues(key, period).Inc()
}

// Panic records a recovered handler panic.
func Panic() {
	panicsTotal.Inc()
}

// QueueDepth sets the current request queue depth.
func QueueDepth(v float64) {
	queueDepth.Set(v)
}

// QueueTimeout records a queued request that timed out before getting a node.
func QueueTimeout() {
	queueTimeoutsTotal.Inc()
}

// WarmupPing records a keepalive ping. status is "ok" or "error".
func WarmupPing(model, node, status string) {
	warmupPingsTotal.WithLabelValues(boundModel(model), node, status).Inc()
}

// WarmupResident records whether a warmup-target model is currently loaded in
// VRAM on a node (true=warm/resident). This is the signal that warmup is real:
// if a targeted model reads 0 here, warmup is failing to keep it warm.
func WarmupResident(model, node string, resident bool) {
	v := 0.0
	if resident {
		v = 1.0
	}
	warmupResident.WithLabelValues(boundModel(model), node).Set(v)
}

// ScheduleFired records that a scheduled action fired.
func ScheduleFired(action, node string) {
	scheduleFires.WithLabelValues(action, node).Inc()
}

// ModelEvicted records that a model was unloaded from a node to free VRAM.
func ModelEvicted(node string) {
	modelEvictions.WithLabelValues(node).Inc()
}

// PredictionAccuracyRatio records the rolling prewarming prediction accuracy.
func PredictionAccuracyRatio(v float64) {
	predictionAccuracyRatio.Set(v)
}

// PrefixLocalityHit records a request whose prefix hash matched a recorded
// routing hint (Step 6). Does not imply that node was actually selected -
// warm residency and other higher-tier signals can still win.
func PrefixLocalityHit() {
	prefixLocalityHitsTotal.Inc()
}

// PrefixLocalityMiss records a request with no matching prefix-locality hint.
func PrefixLocalityMiss() {
	prefixLocalityMissesTotal.Inc()
}
