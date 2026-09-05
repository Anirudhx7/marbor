# Production Deployment Guide

This guide covers running marbor in a real production environment. There is no config file - marbor is DB-first (`marbor.db`, SQLite). Point it at a persistent path with `--db` (or `MARBOR_DB_PATH`), boot it once, and configure nodes/keys/routing/everything else through the admin dashboard or the `/admin/v1/...` REST API (see the [Configuration section of the README](../README.md#configuration)).

---

## Ports

| Port | Purpose |
|------|---------|
| `11434` | Ollama-compatible endpoint (control plane) - this is what clients point at |
| `8080` | Admin dashboard + REST API |
| `9090` | Prometheus metrics (optional) |

---

## systemd

Create `/etc/systemd/system/marbor.service`:

```ini
[Unit]
Description=marbor LLM control plane
After=network.target
Wants=network.target

[Service]
Type=simple
User=marbor
Group=marbor
WorkingDirectory=/opt/marbor
ExecStart=/opt/marbor/marbor --db /opt/marbor/marbor.db
Restart=on-failure
RestartSec=5

# Graceful shutdown: SIGTERM triggers a 15-second drain before exit.
# The unit will wait up to TimeoutStopSec before sending SIGKILL.
TimeoutStopSec=30

# Harden the service
NoNewPrivileges=true
PrivateTmp=true
ProtectSystem=strict
ReadWritePaths=/opt/marbor

[Install]
WantedBy=multi-user.target
```

```bash
sudo useradd -r -s /sbin/nologin marbor
sudo mkdir -p /opt/marbor
sudo cp marbor /opt/marbor/
sudo chown -R marbor:marbor /opt/marbor
sudo systemctl daemon-reload
sudo systemctl enable --now marbor
sudo journalctl -u marbor -f
```

First boot creates `/opt/marbor/marbor.db` blank-slate. Log in at `http://<host>:8080` with `admin`/`admin` (forced password change on first login) and add your nodes/API keys from the dashboard - or run `install.sh`'s network-discovery wizard beforehand to seed nodes automatically.

---

## Docker Compose

The repo ships a working `docker-compose.yml`. It mounts **two** named volumes:

```yaml
# docker-compose.yml (reference - already in repo)
services:
  marbor:
    image: ghcr.io/anirudhx7/marbor:latest
    ports:
      - "11434:11434"
      - "8080:8080"
      - "9090:9090"
    volumes:
      - marbor-data:/data       # persists marbor.db across restarts
      - marbor-backups:/backups # scheduled backups (Settings > Backup & Restore)
    environment:
      - MARBOR_DB_PATH=/data/marbor.db
      - MARBOR_BACKUP_DIR=/backups
    restart: unless-stopped

volumes:
  marbor-data:
  marbor-backups:
```

The `marbor-data` volume mount is important: without it, nodes, API keys, quota counters, and every other setting reset on every container restart. First boot creates a blank-slate `marbor.db` inside the volume - configure it via the dashboard at `http://localhost:8080` (`admin`/`admin`, forced password change on first login).

`marbor-backups` is a **separate** volume from `marbor-data`, on purpose: enable scheduled backups from the dashboard's Settings > Backup & Restore card (interval + retention count are configurable there), and a `docker volume rm marbor-data` or `docker-compose down -v` that wipes the live database still leaves every backup intact, and vice versa. For real protection against losing the whole Docker host (not just a container or a single volume), point `marbor-backups` at storage that lives elsewhere - a different physical disk, an NFS/SMB mount, or a bind mount synced off-host - rather than leaving it as a second volume on the same disk as `marbor-data`. A manual "Download Backup Now" button on the same Settings card streams an on-demand copy straight to your browser at any time. The same card also lists scheduled backups and can restore one with a click - which stops and restarts the marbor process, so it requires `restart: unless-stopped` (already set above) or an equivalent supervisor. See [`backup.md`](backup.md) for the full restore flow, the supervisor requirement, and the fully-manual fallback (not limited to files under `/backups` - any `.db` file works, on Docker or bare metal alike).

---

## Kubernetes

Minimal manifest. Adjust resource limits and image tag for your cluster. There is no ConfigMap - `marbor.db` lives on the PVC below, and nodes/keys/routing are set via the dashboard or `/admin/v1/...` REST API after the pod is up. GitOps-style operators can drive that same REST API from a post-deploy Job instead of hand-editing a file.

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: marbor
  namespace: ai-infra
spec:
  replicas: 1        # single instance - marbor.db on the PVC is not a shared datastore
  selector:
    matchLabels:
      app: marbor
  template:
    metadata:
      labels:
        app: marbor
      annotations:
        prometheus.io/scrape: "true"
        prometheus.io/port: "9090"
        prometheus.io/path: "/metrics"
    spec:
      containers:
        - name: marbor
          image: ghcr.io/anirudhx7/marbor:latest
          args: ["--db", "/data/marbor.db"]
          ports:
            - containerPort: 11434   # endpoint
            - containerPort: 8080    # admin
            - containerPort: 9090    # metrics
          livenessProbe:
            httpGet:
              path: /health
              port: 11434
            initialDelaySeconds: 5
            periodSeconds: 10
          readinessProbe:
            httpGet:
              path: /health
              port: 11434
            initialDelaySeconds: 2
            periodSeconds: 5
          resources:
            requests:
              memory: "64Mi"
              cpu: "50m"
            limits:
              memory: "256Mi"
              cpu: "500m"
          volumeMounts:
            - name: data
              mountPath: /data
      volumes:
        - name: data
          persistentVolumeClaim:
            claimName: marbor-data
---
apiVersion: v1
kind: Service
metadata:
  name: marbor
  namespace: ai-infra
spec:
  selector:
    app: marbor
  ports:
    - name: proxy
      port: 11434
      targetPort: 11434
    - name: admin
      port: 8080
      targetPort: 8080
    - name: metrics
      port: 9090
      targetPort: 9090
```

Note: the Kubernetes deployment above runs a single replica because `marbor.db` (SQLite) is the sole datastore and is not shared across instances. Running multiple replicas would split state across separate, inconsistent databases.

---

## nginx TLS Termination

marbor does not handle TLS directly - that is delegated to nginx (or any other reverse proxy). Example nginx snippet for TLS termination:

```nginx
upstream marbor_proxy {
    server 127.0.0.1:11434;
}

upstream marbor_admin {
    server 127.0.0.1:8080;
}

# Control plane endpoint - clients point OPENAI_BASE_URL here
server {
    listen 443 ssl http2;
    server_name llm.example.com;

    ssl_certificate     /etc/ssl/certs/llm.example.com.pem;
    ssl_certificate_key /etc/ssl/private/llm.example.com.key;

    location / {
        proxy_pass         http://marbor_proxy;
        proxy_http_version 1.1;
        proxy_set_header   Host $host;
        proxy_set_header   X-Real-IP $remote_addr;
        proxy_set_header   X-Forwarded-For $proxy_add_x_forwarded_for;
        proxy_set_header   Connection "";

        # Streaming: disable buffering so tokens reach clients immediately
        proxy_buffering    off;
        proxy_read_timeout 300s;
        proxy_send_timeout 300s;
    }
}

# Admin dashboard - restrict to internal network
server {
    listen 443 ssl http2;
    server_name llm-admin.example.com;

    ssl_certificate     /etc/ssl/certs/llm-admin.example.com.pem;
    ssl_certificate_key /etc/ssl/private/llm-admin.example.com.key;

    # Restrict to your VPN/office CIDR
    allow 10.0.0.0/8;
    deny  all;

    location / {
        proxy_pass http://marbor_admin;
        proxy_set_header Host $host;
    }
}
```

`proxy_buffering off` is required. Buffering the endpoint port will break streaming responses.

---

## Health Check

`GET /health` on the endpoint port (11434) returns HTTP 200 when the process is up and ready to accept connections. It requires no authentication. Use it for load balancer health checks, container liveness/readiness probes, and uptime monitors.

```bash
curl http://localhost:11434/health
# 200 OK
```

---

## Graceful Shutdown

On SIGTERM, marbor starts a 15-second graceful shutdown:

1. Stops accepting new connections on all three ports.
2. Allows in-flight requests to complete (up to the timeout).
3. Flushes in-memory per-key usage/quota counters to the `key_counters` table in `marbor.db` one final time.
4. Exits cleanly.

For systemd, the `TimeoutStopSec=30` in the unit file gives the process time to drain before systemd sends SIGKILL.

---

## Resource Sizing

marbor is intentionally lightweight. It does not buffer request/response bodies (streaming passes through directly).

| Workload | Memory | CPU |
|---------|--------|-----|
| Idle, 10 nodes | ~30-50 MB RSS | negligible |
| 50 concurrent streaming requests | ~80-120 MB RSS | 1-5% of one core |
| 200 concurrent streaming requests | ~200-300 MB RSS | 5-15% of one core |

The bottleneck at high concurrency is almost always the upstream Ollama nodes or the network, not the marbor process itself.

---

## Durability Notes

| Data | Persistence |
|------|------------|
| Per-key token usage, quota counters | In-memory, flushed to the `key_counters` table in `marbor.db` every 30 seconds and on clean shutdown. A crash loses at most 30 seconds of counter updates. |
| API key config, node config, routing/settings | In `marbor.db` (SQLite) - the sole source of truth. Set via the dashboard or `/admin/v1/...` REST API. |
| Audit log (per-request) | The `audit_log` table in `marbor.db`, written when audit logging is enabled. Retention is configurable (`audit_retention_days` setting, default 30 days; 0 keeps rows forever) and enforced by a periodic prune job. |
| Admin action trail | The separate `system_audit_log` table in `marbor.db` (who changed what in the dashboard/API). Independently retained (`audit_system_retention_days` setting, default 0 = forever) since it is lower-volume and more security-sensitive than the per-request audit log. |
| Request log / analytics | Persisted to `request_log`, `hourly_buckets`, and `model_stats` tables in `marbor.db`, not purely in-memory. |
| GPU telemetry | Live reads from `nvidia-smi` on local nodes, and via the optional marbor agent on remote GPU hosts. Not persisted. |

---

## Write-Path Capacity

The three per-request SQLite writes above (audit log, request log, hourly/model stats) are async:
each goes through a bounded 5000-slot buffered channel with drop-on-full, not the request
goroutine (`internal/audit/audit.go`, `internal/admin/admin.go`). Under sustained load, this
design absorbs a burst without adding request latency until a queue actually fills, at which
point new entries for that table are dropped (and logged) rather than blocking requests.

_Originally measured 2026-08-13 with `bench/loadtest` (see `bench/README.md`) against a single
marbor process, a single `cmd/mocknode` backend (warm model, `LATENCY_MS=20`), on a Windows dev
workstation - not production server hardware. **Re-measured 2026-09-05** after enough
write-path-adjacent code had landed since (the activity/audit merge, several `internal/store`
correctness fixes) to warrant re-checking rather than trusting the original numbers as-is._

| Metric | 2026-08-13 (original measurement) | 2026-09-05 re-measurement |
|---|---|---|
| No drops observed | Up to 300 req/s, p50 ~330-380ms flat, zero failures. | Not reproduced at the same level - see "First observed queue-full drop" below; effectively no clean no-drop plateau above ~100-200 req/s in this re-run. |
| Latency knee | ~400 req/s - p50 jumps from ~380ms to ~1.7s. | **Confirmed, reproduced across two independent isolated single-node runs** - p50 jumps from the 20-40ms baseline to 1.1-1.7s at 400 req/s in both runs. This is the most stable finding across both measurement dates. |
| First observed queue-full drop | ~500 req/s, reproduced across two isolated runs. | **As early as ~200-300 req/s** across two isolated runs (`async logger: queue full`, `audit logger: queue full`, `async logger: stats queue full` all observed) - materially earlier than the 2026-08-13 figure. Real write-path-adjacent code changes landed between the two dates and are a plausible cause; this re-measurement also ran on a shared dev workstation with other concurrent Claude Code sessions active, which is a confound this run could not rule out. Do not treat ~500 req/s as current - use ~200-300 req/s. |
| Node-count sensitivity | Not yet tested. | **Tested** - see the dedicated subsection below. Headline: adding backend nodes did not raise the ceiling. |

**Important caveat on what this number does and doesn't isolate:** this test setup (one marbor
process, one mock backend node) could not cleanly separate "the 5000-slot async queues filled up"
from "the single backend node's own per-request service time became the bottleneck first." Both
plausibly contribute at the ~300-400 req/s knee. Treat these as *this setup's* observed
ceiling, not a proven SQLite-write-path-alone limit - a cleaner isolation would need either a
zero-latency synthetic write-only path, several concurrent mock nodes so no single backend's
service time can be the constraint, or a dedicated (non-shared) test host to remove the
concurrent-session confound noted above.

### Node-count sensitivity (measured 2026-09-05)

Same `bench/loadtest` sweep methodology, same single marbor process, run against 2 and then 4
real `cmd/mocknode` backend instances (each warm, `LATENCY_MS=20`), all registered simultaneously
so requests could land on any of them.

| Node count | First queue-full drop observed | Notes |
|---|---|---|
| 1 (baseline) | ~200-300 req/s (two isolated runs) | See table above. |
| 2 | Yes, drops observed within the swept range (100-400 req/s, 10s steps). | Load generator itself saturated earlier than the 1-node baseline (down to ~130 sent req/s at a 300 req/s target, vs. ~274-287 sent req/s at 1 node). |
| 4 | **Zero** queue-full drops logged across the same swept range (100-400 req/s). | Load generator saturated even more severely (down to ~113-136 sent req/s at 200-400 req/s targets) - but marbor's own write-queue never actually filled, meaning the offered write-side load at 4 nodes never reached the level that triggers drops at 1-2 nodes. |

**Reading this honestly:** node count did not raise the SQLite write-path's real (drop-based)
ceiling in this test - if anything the 4-node run's total realized throughput was lower than the
1-node baseline's, but that appears to be a load-generator/test-machine artifact rather than a
marbor-side capacity change, since marbor's own queues logged *fewer* drops (zero) at 4 nodes than
at 1-2 nodes over the same target-rate range. This is consistent with the original single-node measurement's framing -
the SQLite write path itself, not per-node backend capacity, is the constraint - but this specific
run cannot cleanly separate that from CPU contention on the shared test machine (more concurrent
`cmd/mocknode` processes plus marbor's own per-node `/api/ps` poll loop competing with the load
generator for the same CPU). **The actionable conclusion for an operator: node count is not a
lever for raising this ceiling.** Do not expect adding GPU nodes to increase write-path throughput.
A precise linear/sub-linear/flat verdict (rather than "flat-to-not-improving, confounded by test
conditions") needs a re-run on a dedicated, uncontended host.

Re-run `bench/loadtest` after any change to `internal/audit`, `internal/admin`'s async
queues, or `internal/store/sqlite.go`'s connection/pragma settings, or on real production
hardware, since any of those can move this number.
