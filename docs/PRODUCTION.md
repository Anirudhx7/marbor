# Production Deployment Guide

This guide covers running ollama-mesh in a real production environment. It assumes you have already configured `config.yaml` from `config.example.yaml`.

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
ExecStart=/opt/ollama-mesh/ollama-mesh
Restart=on-failure
RestartSec=5
# Config path defaults to ./config.yaml; override if needed:
# Environment=CONFIG_PATH=/etc/ollama-mesh/config.yaml

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
sudo cp config.yaml /opt/ollama-mesh/
sudo chown -R ollama-mesh:ollama-mesh /opt/ollama-mesh
sudo systemctl daemon-reload
sudo systemctl enable --now ollama-mesh
sudo journalctl -u ollama-mesh -f
```

---

## Docker Compose

The repo ships a working `docker-compose.yml`. For production, the key fields to set in your environment or a `.env` file:

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
      - ./config.yaml:/app/config.yaml:ro
      - ./usage-state.json:/app/usage-state.json   # persist quota state
      - ./audit.log:/app/audit.log                 # persist audit log
      - /var/run/docker.sock:/var/run/docker.sock  # only if docker.enabled: true
    restart: unless-stopped
    environment:
      - CONFIG_PATH=/app/config.yaml
```

The `usage-state.json` volume mount is important: without it, per-key quota counters reset on every container restart.

---

## Kubernetes

Minimal manifest. Adjust resource limits and image tag for your cluster.

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: ollama-mesh-config
  namespace: ai-infra
data:
  config.yaml: |
    proxy:
      port: 11434
      log_level: info
    auth:
      enabled: true
      admin_token: sk-admin-change-me   # override via Secret in real deployments
      state_path: /data/usage-state.json
    nodes:
      - name: gpu-0
        url: http://ollama-gpu-0.ai-infra.svc.cluster.local:11434
    routing:
      strategy: warm-first
      poll_interval_ms: 2000
      fallback: least-connections
      max_retries: 2
    metrics:
      enabled: true
      port: 9090
---
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
          env:
            - name: CONFIG_PATH
              value: /config/config.yaml
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
            - name: config
              mountPath: /config
              readOnly: true
            - name: data
              mountPath: /data
      volumes:
        - name: config
          configMap:
            name: ollama-mesh-config
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
| API key config, node config | In `config.yaml` - your source of truth. |
| Audit log | Append-only JSON-lines file at `audit.path` if `audit.enabled: true`. |
| Request log / analytics | In-memory only. Lost on restart by design - these are operational views, not a database. SQLite persistence is on the roadmap. |
| GPU telemetry | Live reads from nvidia-smi on the mesh host. Not persisted. |
