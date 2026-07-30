# Production Deployment Guide

This guide covers running ollama-mesh in a real production environment. There is no config file - ollama-mesh is DB-first (`mesh.db`, SQLite). Point it at a persistent path with `--db` (or `MESH_DB_PATH`), boot it once, and configure nodes/keys/routing/everything else through the admin dashboard or the `/admin/v1/...` REST API (see the [Configuration section of the README](../README.md#configuration)).

---

## Ports

| Port | Purpose |
|------|---------|
| `11434` | Ollama-compatible endpoint (control plane) - this is what clients point at |
| `8080` | Admin dashboard + REST API |
| `9090` | Prometheus metrics (optional) |

---

## systemd

Create `/etc/systemd/system/ollama-mesh.service`:

```ini
[Unit]
Description=ollama-mesh LLM control plane
After=network.target
Wants=network.target

[Service]
Type=simple
User=ollama-mesh
Group=ollama-mesh
WorkingDirectory=/opt/ollama-mesh
ExecStart=/opt/ollama-mesh/ollama-mesh --db /opt/ollama-mesh/mesh.db
Restart=on-failure
RestartSec=5

# Graceful shutdown: SIGTERM triggers a 15-second drain before exit.
# The unit will wait up to TimeoutStopSec before sending SIGKILL.
TimeoutStopSec=30

# Harden the service
NoNewPrivileges=true
PrivateTmp=true
ProtectSystem=strict
ReadWritePaths=/opt/ollama-mesh

[Install]
WantedBy=multi-user.target
```

```bash
sudo useradd -r -s /sbin/nologin ollama-mesh
sudo mkdir -p /opt/ollama-mesh
sudo cp ollama-mesh /opt/ollama-mesh/
sudo chown -R ollama-mesh:ollama-mesh /opt/ollama-mesh
sudo systemctl daemon-reload
sudo systemctl enable --now ollama-mesh
sudo journalctl -u ollama-mesh -f
```

First boot creates `/opt/ollama-mesh/mesh.db` blank-slate. Log in at `http://<host>:8080` with `admin`/`admin` (forced password change on first login) and add your nodes/API keys from the dashboard - or run `install.sh`'s network-discovery wizard beforehand to seed nodes automatically.

---

## Docker Compose

The repo ships a working `docker-compose.yml`. It mounts **two** named volumes:

```yaml
# docker-compose.yml (reference - already in repo)
services:
  ollama-mesh:
    image: ghcr.io/anirudhx7/ollama-mesh:latest
    ports:
      - "11434:11434"
      - "8080:8080"
      - "9090:9090"
    volumes:
      - mesh-data:/data       # persists mesh.db across restarts
      - mesh-backups:/backups # scheduled backups (Settings > Backup & Restore)
    environment:
      - MESH_DB_PATH=/data/mesh.db
      - MESH_BACKUP_DIR=/backups
    restart: unless-stopped

volumes:
  mesh-data:
  mesh-backups:
```

The `mesh-data` volume mount is important: without it, nodes, API keys, quota counters, and every other setting reset on every container restart. First boot creates a blank-slate `mesh.db` inside the volume - configure it via the dashboard at `http://localhost:8080` (`admin`/`admin`, forced password change on first login).

`mesh-backups` is a **separate** volume from `mesh-data`, on purpose: enable scheduled backups from the dashboard's Settings > Backup & Restore card (interval + retention count are configurable there), and a `docker volume rm mesh-data` or `docker-compose down -v` that wipes the live database still leaves every backup intact, and vice versa. For real protection against losing the whole Docker host (not just a container or a single volume), point `mesh-backups` at storage that lives elsewhere - a different physical disk, an NFS/SMB mount, or a bind mount synced off-host - rather than leaving it as a second volume on the same disk as `mesh-data`. A manual "Download Backup Now" button on the same Settings card streams an on-demand copy straight to your browser at any time. See [`backup.md`](backup.md) for how to actually restore from one of these files - restore is a manual procedure and is **not** limited to files under `/backups`; any `.db` file you point it at works, on Docker or bare metal alike.

---

## Kubernetes

Minimal manifest. Adjust resource limits and image tag for your cluster. There is no ConfigMap - `mesh.db` lives on the PVC below, and nodes/keys/routing are set via the dashboard or `/admin/v1/...` REST API after the pod is up. GitOps-style operators can drive that same REST API from a post-deploy Job instead of hand-editing a file.

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: ollama-mesh
  namespace: ai-infra
spec:
  replicas: 1        # single instance - mesh.db on the PVC is not a shared datastore
  selector:
    matchLabels:
      app: ollama-mesh
  template:
    metadata:
      labels:
        app: ollama-mesh
      annotations:
        prometheus.io/scrape: "true"
        prometheus.io/port: "9090"
        prometheus.io/path: "/metrics"
    spec:
      containers:
        - name: ollama-mesh
          image: ghcr.io/anirudhx7/ollama-mesh:latest
          args: ["--db", "/data/mesh.db"]
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
            claimName: ollama-mesh-data
---
apiVersion: v1
kind: Service
metadata:
  name: ollama-mesh
  namespace: ai-infra
spec:
  selector:
    app: ollama-mesh
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

Note: the Kubernetes deployment above runs a single replica because `mesh.db` (SQLite) is the sole datastore and is not shared across instances. Running multiple replicas would split state across separate, inconsistent databases.

---

## nginx TLS Termination

ollama-mesh does not handle TLS directly - that is delegated to nginx (or any other reverse proxy). Example nginx snippet for TLS termination:

```nginx
upstream ollama_mesh_proxy {
    server 127.0.0.1:11434;
}

upstream ollama_mesh_admin {
    server 127.0.0.1:8080;
}

# Control plane endpoint - clients point OPENAI_BASE_URL here
server {
    listen 443 ssl http2;
    server_name llm.example.com;

    ssl_certificate     /etc/ssl/certs/llm.example.com.pem;
    ssl_certificate_key /etc/ssl/private/llm.example.com.key;

    location / {
        proxy_pass         http://ollama_mesh_proxy;
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
        proxy_pass http://ollama_mesh_admin;
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

On SIGTERM, ollama-mesh starts a 15-second graceful shutdown:

1. Stops accepting new connections on all three ports.
2. Allows in-flight requests to complete (up to the timeout).
3. Flushes in-memory per-key usage/quota counters to the `key_counters` table in `mesh.db` one final time.
4. Exits cleanly.

For systemd, the `TimeoutStopSec=30` in the unit file gives the process time to drain before systemd sends SIGKILL.

---

## Resource Sizing

ollama-mesh is intentionally lightweight. It does not buffer request/response bodies (streaming passes through directly).

| Workload | Memory | CPU |
|---------|--------|-----|
| Idle, 10 nodes | ~30-50 MB RSS | negligible |
| 50 concurrent streaming requests | ~80-120 MB RSS | 1-5% of one core |
| 200 concurrent streaming requests | ~200-300 MB RSS | 5-15% of one core |

The bottleneck at high concurrency is almost always the upstream Ollama nodes or the network, not the mesh process itself.

---

## Durability Notes

| Data | Persistence |
|------|------------|
| Per-key token usage, quota counters | In-memory, flushed to the `key_counters` table in `mesh.db` every 30 seconds and on clean shutdown. A crash loses at most 30 seconds of counter updates. |
| API key config, node config, routing/settings | In `mesh.db` (SQLite) - the sole source of truth. Set via the dashboard or `/admin/v1/...` REST API. |
| Audit log (per-request) | The `audit_log` table in `mesh.db`, written when audit logging is enabled. Retention is configurable (`audit_retention_days` setting, default 30 days; 0 keeps rows forever) and enforced by a periodic prune job. |
| Admin action trail | The separate `system_audit_log` table in `mesh.db` (who changed what in the dashboard/API). Independently retained (`audit_system_retention_days` setting, default 0 = forever) since it is lower-volume and more security-sensitive than the per-request audit log. |
| Request log / analytics | Persisted to `request_log`, `hourly_buckets`, and `model_stats` tables in `mesh.db`, not purely in-memory. |
| GPU telemetry | Live reads from `nvidia-smi` on local nodes, and via the optional Node Agent on remote GPU hosts. Not persisted. |
