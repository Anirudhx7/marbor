# Monitor a self-hosted LLM fleet with Prometheus and Grafana

Marbor exposes Prometheus metrics for the whole fleet from one place: requests per node and key, queue depth, node health, time to first token, and quota rejections. Metrics are off by default, so the first step is to turn them on, then add one scrape job and import the included Grafana dashboard.

> **Do step 1 first.** A fresh Marbor install does not serve metrics at all, so Prometheus has nothing to scrape and the Grafana dashboard stays empty, including when you use the bundled Docker monitoring overlay. Enabling metrics is a setting plus a restart.

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

Or run the repository's ready-made stack (Marbor, Prometheus, and Grafana with the dashboard provisioned). Metrics must already be enabled as in step 1; the overlay only adds the scraper and the dashboard, it does not turn metrics on:

```bash
git clone https://github.com/Anirudhx7/marbor && cd marbor
docker compose -f docker-compose.yml -f docker-compose.monitoring.yml up -d
```

Grafana comes up on `127.0.0.1:3000` (login `admin` / `admin`; change it before exposing the port) with the Marbor dashboard from `grafana/marbor.json`. If the dashboard is empty, metrics are not enabled yet: go back to step 1 and restart Marbor.

## 3. Scrape marbor-agent on GPU nodes (HTTPS, Bearer Token & CA Trust)

The control plane's `:9090` metrics cover gateway routing, queues, latencies, and quotas. To monitor the physical GPU nodes directly - core compute utilization, per-GPU VRAM usage, temperature, power draw, fan speed, host RAM, CPU, and disk space - Prometheus can scrape the companion `marbor-agent` binary running on each GPU host.

The agent serves Prometheus-format text metrics on `GET /metrics` (default port `:9200`). Because the agent controls model execution and host resources, its HTTP server enforces transport security and authentication by default.

### Prometheus scrape configuration

```yaml
# prometheus.yml
scrape_configs:
  - job_name: marbor
    scrape_interval: 15s
    static_configs:
      - targets: ['<marbor-host>:9090']

  - job_name: marbor-agent
    scheme: https
    scrape_interval: 15s
    authorization:
      credentials_file: /etc/prometheus/secrets/agent.token
    tls_config:
      ca_file: /etc/prometheus/certs/agent.crt
    static_configs:
      - targets:
          - 'gpu-node-1.internal:9200'
          - 'gpu-node-2.internal:9200'
```

### Transport, authentication, and trust details

- **HTTPS by default:** A service installed via `marbor-agent service install` generates a self-signed ECDSA P-256 certificate and key pair on first install, serving HTTPS. Scraping an HTTPS agent over plain HTTP triggers an immediate error:
  ```text
  server returned HTTP status 400 Bad Request: Client sent an HTTP request to an HTTPS server
  ```
  Set `scheme: https` in the scrape job whenever the agent serves TLS.
- **Bearer token authentication:** Every request to `GET /metrics` requires an exact-match bearer token:
  ```http
  Authorization: Bearer <token>
  ```
  Scraping without a token or with a mismatched secret returns `HTTP 401 Unauthorized`. The agent secret is configured via the `MARBOR_AGENT_SECRET` environment variable (persisted on Linux services in `/etc/marbor-agent.env` with `0600` permissions).
  Extract the token value into a dedicated credentials file (`authorization.credentials_file`) readable by the Prometheus process user. Avoid sharing readable permissions across untrusted local users.
- **CA trust and SAN matching:** The agent's self-signed certificate is valid for 10 years and embeds Subject Alternative Names (SANs) for `localhost`, the machine's primary hostname, and its local IPv4/IPv6 addresses. Prometheus must trust this certificate as a root CA by setting `tls_config.ca_file` to point to a copy of the agent's certificate:
  - Linux: `/etc/marbor-agent.crt`
  - macOS: `/Library/Application Support/marbor-agent/agent.crt`
  - Windows: `%ProgramData%\marbor-agent\agent.crt`
  The host name or IP address in `static_configs` targets must match one of the SAN entries on the certificate. If Prometheus targets an unlisted CNAME, DNS alias, or external NAT address, the TLS handshake fails with a certificate validation error.
  > Do not set `insecure_skip_verify: true` in production. Disabling verification forfeits man-in-the-middle protections; trust the agent's certificate explicitly via `ca_file`.
- **Certificate renewal and rotation:** Running `marbor-agent service regen-cert` forces generation of a new certificate/key pair and restarts the agent service. When rotated, the new certificate must be copied to Prometheus's `ca_file` path and Prometheus reloaded (`curl -X POST http://localhost:9090/-/reload` or `kill -HUP <prometheus-pid>`). Until Prometheus trusts the new certificate, scraping fails with:
  ```text
  x509: certificate signed by unknown authority
  ```
- **Plaintext HTTP on loopback or isolated networks:** Running `marbor-agent` in the foreground without `--cert`/`--key` on a loopback bind (`--bind=127.0.0.1`) serves plaintext HTTP. On non-loopback binds, serving plaintext requires the explicit `--allow-insecure-plaintext` flag, which emits a startup warning since bearer credentials traverse the network unencrypted.

### Agent metrics exported

| Metric | Use it for |
|--------|------------|
| `marboragent_gpu_core_percent{gpu}` | GPU compute core utilization percentage (0-100) |
| `marboragent_gpu_temperature_celsius{gpu}` | GPU thermal monitoring; pair with alerts before thermal throttling |
| `marboragent_gpu_vram_used_mb{gpu}` | Active VRAM consumed on the device in MB |
| `marboragent_gpu_vram_total_mb{gpu}` | Total physical VRAM capacity in MB |
| `marboragent_gpu_power_watts{gpu}` | Real-time power draw in watts |
| `marboragent_gpu_fan_percent{gpu}` | Cooling fan speed percentage |
| `marboragent_gpu_count` | Total number of detected GPU devices on the node |
| `marboragent_host_cpu_percent` | Host CPU utilization percentage |
| `marboragent_host_ram_used_mb` / `_total_mb` | Host system memory consumption |
| `marboragent_host_disk_free_gb` / `_total_gb` | Disk storage headroom for model weights |
| `marboragent_info{node_id, agent_version, platform, architecture, gpu_vendor, runtime, capabilities}` | Metadata series with constant value 1; filter/group fleets by GPU vendor, runtime, or OS |

## 4. The metrics worth graphing

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

## 5. Alert on the things that hurt

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
