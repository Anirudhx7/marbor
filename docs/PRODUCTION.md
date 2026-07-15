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

The repo ships a working `docker-compose.yml`. It mounts a single named volume for `mesh.db` - that's the only state that needs to survive a restart:

```yaml
# docker-compose.yml (reference - already in repo)
services:
  ollama-mesh:
    image: ghcr.io/ollama-mesh/ollama-mesh:latest
    ports:
      - "11434:11434"
      - "8080:8080"
      - "9090:9090"
    volumes:
      - mesh-data:/root                            # persists mesh.db across restarts
      - /var/run/docker.sock:/var/run/docker.sock  # only if Docker auto-discovery is enabled
    restart: unless-stopped

volumes:
  mesh-data:
```

The `mesh-data` volume mount is important: without it, nodes, API keys, quota counters, and every other setting reset on every container restart. First boot creates a blank-slate `mesh.db` inside the volume - configure it via the dashboard at `http://localhost:8080` (`admin`/`admin`, forced password change on first login).

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
  replicas: 1        # single instance - usage state is local file, not shared
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
          image: ghcr.io/ollama-mesh/ollama-mesh:latest
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

Note: the Kubernetes deployment above runs a single replica because `usage-state.json` is a local file. Running multiple replicas would split quota state across instances. A shared-state backend is on the Phase 3 roadmap.

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
3. Flushes `usage-state.json` one final time so quota counters are not lost.
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
| Per-key token usage, quota counters | Persisted to `auth.state_path` (default: `usage-state.json`) every 30 seconds and on clean shutdown. A crash loses at most 30 seconds of counter updates. Set `state_path: "-"` to disable. |
| API key config, node config, routing/settings | In `mesh.db` (SQLite) - the sole source of truth. Set via the dashboard or `/admin/v1/...` REST API. |
| Audit log | Append-only JSON-lines file at `audit.path` if `audit.enabled: true`. |
| Request log / analytics | In-memory only. Lost on restart by design - these are operational views, not a database. SQLite persistence is on the roadmap. |
| GPU telemetry | Live reads from nvidia-smi on the mesh host. Not persisted. |
