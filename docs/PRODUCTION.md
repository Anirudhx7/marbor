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

_Measured 2026-08-13 with `bench/loadtest` (see `bench/README.md`) against a single marbor process,
a single `cmd/mocknode` backend (warm model, `LATENCY_MS=20`), on a Windows dev workstation - not
production server hardware. Reproduced across two independent, isolated sweeps._

| Metric | Result |
|---|---|
| No drops observed | Up to 300 req/s sustained, p50 latency ~330-380ms (flat, matching the mock backend's own fixed per-response time), zero request failures. |
| Latency knee | ~400 req/s - p50 jumps from ~380ms (at 300 req/s) to ~1.7s, indicating request backlog starting to build. |
| First observed queue-full drop | ~500 req/s sustained, reproduced in both isolated test runs (`async logger: queue full`, `audit logger: queue full`, `async logger: stats queue full` all fired within the same run). |
| Node-count sensitivity | Not yet tested - this measurement used the single-node baseline only, which is the primary claim for the SQLite write path itself. A secondary multi-node run is still open (see `bench/README.md`). |

**Important caveat on what this number does and doesn't isolate:** this test setup (one marbor
process, one mock backend node) could not cleanly separate "the 5000-slot async queues filled up"
from "the single backend node's own per-request service time became the bottleneck first." Both
plausibly contribute at the ~400-500 req/s knee. Treat "~500 req/s" as *this setup's* observed
ceiling, not a proven SQLite-write-path-alone limit - a cleaner isolation would need either a
zero-latency synthetic write-only path or several concurrent mock nodes so no single backend's
service time can be the constraint. That refinement is a reasonable follow-up, not required to
have an honest number today.

Re-run `bench/loadtest` after any change to `internal/audit`, `internal/admin`'s async
queues, or `internal/store/sqlite.go`'s connection/pragma settings, or on real production
hardware, since any of those can move this number.
