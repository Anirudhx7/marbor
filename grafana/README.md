# ollama-mesh Grafana Dashboard

One-click Grafana dashboard for GPU-aware Ollama proxy metrics: requests, latency percentiles, warm vs cold routing ratio, and per-node active connections.

## Prerequisites

- Prometheus scraping ollama-mesh metrics endpoint at `:9090/metrics`
- Grafana 10.x with a Prometheus datasource configured

Example Prometheus scrape config:

```yaml
scrape_configs:
  - job_name: "ollama-mesh"
    static_configs:
      - targets: ["localhost:9090"]
```

## Import Instructions

1. Open Grafana and go to **Dashboards > Import**
2. Click **Upload dashboard JSON file**
3. Select `grafana/ollama-mesh.json`
4. Select your Prometheus datasource from the dropdown
5. Click **Import**

## Panels

| Row | Panel | Metric |
|-----|-------|--------|
| Overview | Total Requests | `ollamamesh_requests_total` |
| Overview | Request Rate (1m) | `rate(ollamamesh_requests_total[1m])` |
| Overview | Error Rate % | `ollamamesh_requests_total{status=~"5.."}` |
| Overview | P99 Latency | `ollamamesh_request_duration_seconds_bucket` |
| Traffic | Requests/s by Model | `rate(ollamamesh_requests_total[1m])` by `model` |
| Traffic | Requests/s by Node | `rate(ollamamesh_requests_total[1m])` by `node` |
| Latency | P50/P95/P99 by Model | `ollamamesh_request_duration_seconds_bucket` |
| Routing | Warm Hit % | `ollamamesh_cache_hits_total` / total |
| Routing | Warm Hits/s | `ollamamesh_cache_hits_total` |
| Routing | Active Connections | `ollamamesh_active_connections` |
| Routing | Healthy Nodes | `ollamamesh_node_healthy` |
| Routing | Active Connections per Node | `ollamamesh_active_connections` by `node` |
| Routing | Tokens/s by API Key | `ollamamesh_tokens_total` by `key_name` |
