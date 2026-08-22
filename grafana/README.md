# marbor Grafana Dashboard

One-click Grafana dashboard for marbor proxy metrics: requests, latency percentiles, warm vs cold routing ratio, and per-node active connections.

## Prerequisites

- Prometheus scraping marbor metrics endpoint at `:9090/metrics` (the repo's `prometheus/prometheus.yml` is a ready-made scrape config)
- Grafana 10.x with a Prometheus datasource configured

Example Prometheus scrape config:

```yaml
scrape_configs:
  - job_name: "marbor"
    static_configs:
      - targets: ["localhost:9090"]
```

> Shortcuts: the repo's Docker monitoring overlay (`docker compose -f docker-compose.yml -f docker-compose.monitoring.yml up -d`) provisions the datasource and this dashboard automatically - no manual import needed (Prometheus UI on host port 9091, Grafana on 3000).

## Import Instructions

1. Open Grafana and go to **Dashboards > Import**
2. Click **Upload dashboard JSON file**
3. Select [`grafana/marbor.json`](marbor.json)
4. Select your Prometheus datasource from the dropdown
5. Click **Import**

## Panels

| Row | Panel | Metric |
|-----|-------|--------|
| Overview | Total Requests | `sum(marbor_requests_total)` |
| Overview | Request Rate (1m) | `sum(rate(marbor_requests_total[1m]))` |
| Overview | Error Rate % (5xx) | `marbor_requests_total{status=~"5.."}` |
| Overview | P99 Latency | `histogram_quantile(0.99, ...)` over `marbor_request_duration_seconds_bucket` |
| Traffic | Requests/s by Model | `rate(marbor_requests_total[1m])` by `model` |
| Traffic | Requests/s by Node | `rate(marbor_requests_total[1m])` by `node` |
| Latency | Latency Percentiles by Model (P50/P95/P99) | `histogram_quantile(...)` over `marbor_request_duration_seconds_bucket` |
| Routing | Warm Routing Hit % | `cache_hits / (cache_hits + cache_misses)` rates |
| Routing | Warm Hits/s | `rate(marbor_cache_hits_total[5m])` |
| Routing | Total Active Connections | `sum(marbor_active_connections)` |
| Routing | Healthy Nodes | `sum(marbor_node_healthy)` |
| Routing | Active Connections per Node | `marbor_active_connections` by `node` |
| Routing | Tokens/s by API Key | `rate(marbor_tokens_total[5m])` by `key_name` |
