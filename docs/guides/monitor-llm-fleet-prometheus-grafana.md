# Monitor a self-hosted LLM fleet with Prometheus and Grafana

Marbor exposes Prometheus metrics for the whole fleet from one place: requests per node and key, queue depth, node health, time to first token, and quota rejections. Metrics are off by default, so the first step is to turn them on, then add one scrape job and import the included Grafana dashboard.

## 1. Turn metrics on

Metrics are disabled until you enable them. Apply a partial settings file and restart Marbor (the metrics listener starts at boot, so a reload is not enough):

```bash
echo '{"metrics":{"enabled":true}}' > metrics.json
marbor settings set --file metrics.json
sudo systemctl restart marbor      # or: docker compose restart marbor
```

After the restart the log says `Metrics server listening on 127.0.0.1:9090/metrics`. Outside Docker the endpoint binds to loopback by default. If Prometheus runs on another host, set the bind address:

```bash
echo '{"metrics":{"enabled":true,"bind_address":"0.0.0.0:9090"}}' > metrics.json
marbor settings set --file metrics.json
sudo systemctl restart marbor
```

The metrics endpoint is unauthenticated, so firewall it to your Prometheus host. Inside the Docker image it already binds every interface, which the bundled monitoring overlay relies on.

Quick check:

```bash
curl -s http://127.0.0.1:9090/metrics | grep ^marbor_node_healthy
```

## 2. Scrape it

```yaml
# prometheus.yml
scrape_configs:
  - job_name: marbor
    scrape_interval: 15s
    static_configs:
      - targets: ['<marbor-host>:9090']
```

Or run the repository's ready-made stack (Marbor, Prometheus, and Grafana with the dashboard provisioned):

```bash
git clone https://github.com/Anirudhx7/marbor && cd marbor
docker compose -f docker-compose.yml -f docker-compose.monitoring.yml up -d
```

Grafana comes up on `127.0.0.1:3000` (login `admin` / `admin`; change it before exposing the port) with the Marbor dashboard from `grafana/marbor.json`. Enable metrics in Marbor as in step 1 first, or the dashboard stays empty.

## 3. The metrics worth graphing

| Metric | Use it for |
|--------|------------|
| `marbor_node_healthy{node}` | 1 or 0 per node; alert when a node drops |
| `marbor_active_connections{node}` | Concurrent requests per node |
| `marbor_queue_depth` | Requests waiting for a free slot |
| `marbor_requests_total{key_name,model,node,status}` | Traffic and errors by key, model, node and status code |
| `marbor_request_ttft_seconds` | Time to first token histogram |
| `marbor_request_duration_seconds` | End-to-end latency histogram |
| `marbor_quota_rejections_total` | Keys being turned away by their limits |
| `marbor_queue_timeouts_total` | Requests that waited out the queue and failed |
| `marbor_cloud_fallbacks_total` | Requests sent to a cloud provider |

Useful queries:

```promql
# 5xx rate by node over 5 minutes
sum by (node) (rate(marbor_requests_total{status=~"5.."}[5m]))

# p95 time to first token
histogram_quantile(0.95, sum by (le) (rate(marbor_request_ttft_seconds_bucket[5m])))

# traffic per API key
sum by (key_name) (rate(marbor_requests_total[5m]))
```

## 4. Alert on the things that hurt

```yaml
groups:
  - name: marbor
    rules:
      - alert: LLMNodeDown
        expr: marbor_node_healthy == 0
        for: 2m
      - alert: LLMQueueBacklog
        expr: marbor_queue_depth > 20
        for: 5m
      - alert: LLMRequestsFailing
        expr: sum(rate(marbor_requests_total{status=~"5.."}[5m])) > 0.1
        for: 5m
```

A ready-made rules file ships in the repository at `docs/prometheus-alerts.yaml`.

## Without Prometheus

The dashboard's **Analytics** page, `marbor metrics summary` and `marbor activity` give a fleet view from the CLI with no extra services. Per-request routing decisions are available with `marbor requests explain <request-id>`.

## What to set up next

- Turn alerts into action: [Drain a GPU node](drain-gpu-node-without-downtime.html)
- Understand queue alerts: [When every GPU is busy](when-every-gpu-is-busy.html)
- Charts per key: [Per-key quotas](per-key-quotas-self-hosted-llm.html)
