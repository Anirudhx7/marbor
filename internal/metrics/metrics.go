package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	requestsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "ollamamesh_requests_total",
		Help: "Total requests proxied",
	}, []string{"key_name", "model", "node", "status"})

	requestDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "ollamamesh_request_duration_seconds",
		Help:    "Request duration in seconds",
		Buckets: prometheus.DefBuckets,
	}, []string{"model", "node"})

	activeConns = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "ollamamesh_active_connections",
		Help: "Current active connections per node",
	}, []string{"node"})

	nodeHealthy = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "ollamamesh_node_healthy",
		Help: "Node health status (1=healthy, 0=unhealthy)",
	}, []string{"node"})

	cacheHits = promauto.NewCounter(prometheus.CounterOpts{
		Name: "ollamamesh_cache_hits_total",
		Help: "Total cache hits (warm model routing)",
	})

	cacheMisses = promauto.NewCounter(prometheus.CounterOpts{
		Name: "ollamamesh_cache_misses_total",
		Help: "Total cache misses (cold start)",
	})

	tokensTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "ollamamesh_tokens_total",
		Help: "Total tokens processed (best-effort from Ollama responses)",
	}, []string{"key_name", "node"})
)

func RequestsTotal(key, model, node, status string) {
	requestsTotal.WithLabelValues(key, model, node, status).Inc()
}

func RequestDuration(model, node string, seconds float64) {
	requestDuration.WithLabelValues(model, node).Observe(seconds)
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
