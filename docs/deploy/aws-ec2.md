# Deploy ollama-mesh on AWS EC2

Run one ollama-mesh endpoint in front of one or more Ollama GPU boxes on EC2.
ollama-mesh is a single static binary - no runtime, no dependencies, no config
file - so an EC2 deploy is "download, start a service, configure from the
dashboard."

This guide was validated end-to-end on AWS (multi-node deploy, warm-first routing,
node-failure route-around, and self-healing recovery all verified live).

## Architecture

```
clients ──► ollama-mesh box (cheap CPU instance, e.g. t3.small)
                 │  warm-first routing + auth + dashboard + metrics
                 ├──► GPU node 1  (g4dn/g5/g6, running Ollama)
                 └──► GPU node 2  (g4dn/g5/g6, running Ollama)
```

The mesh box does **not** need a GPU - it only routes. Put it on a small,
always-on instance; put Ollama on GPU instances you can scale or stop.

## 1. GPU instances (the Ollama nodes)

Cheapest CUDA option is `g4dn.xlarge` (NVIDIA T4 16GB). `g5.xlarge` (A10G) and
`g6.xlarge` (L4) are faster. Use a Deep Learning AMI or install the NVIDIA driver
on Amazon Linux 2023, then install Ollama and bind it to the VPC:

```bash
curl -fsSL https://ollama.com/install.sh | sh
sudo systemctl edit ollama   # add: Environment=OLLAMA_HOST=0.0.0.0:11434
sudo systemctl restart ollama
ollama pull llama3.2:3b
```

> **Quota note:** brand-new AWS accounts start with a GPU On-Demand vCPU quota of 0.
> Request an increase under Service Quotas → EC2 → "Running On-Demand G and VT
> instances" before launching. Approval can take hours to days.

## 2. The mesh box

A small CPU instance (`t3.small` is plenty). Pull the release binary and point it
at your GPU nodes' **private** IPs:

```bash
curl -fL -o /usr/local/bin/ollama-mesh \
  https://github.com/Anirudhx7/ollama-mesh/releases/latest/download/ollama-mesh-linux-amd64
chmod +x /usr/local/bin/ollama-mesh
```

systemd unit (`/etc/systemd/system/ollama-mesh.service`):

```ini
[Unit]
After=network-online.target
Wants=network-online.target
[Service]
WorkingDirectory=/opt
ExecStart=/usr/local/bin/ollama-mesh --db /opt/mesh.db
Restart=always
[Install]
WantedBy=multi-user.target
```

```bash
sudo systemctl daemon-reload && sudo systemctl enable --now ollama-mesh
```

First boot creates a blank-slate `/opt/mesh.db`. Log in at `http://<mesh>:8080`
with `admin`/`admin` (forced password change on first login - do this
immediately since the admin port is reachable from the SSH tunnel below) and,
from **Settings**, set `admin.bind_address` to `127.0.0.1:8080` (restart
required). Add your GPU nodes from the **GPU Nodes** page and an API key from
**API Keys** - or run `install.sh`'s network-discovery wizard against your VPC
beforehand to seed the nodes automatically.

## 3. Security group

- **Endpoint `:11434`** - open only to your app servers / SG, never `0.0.0.0/0`.
- **Admin `:8080`** - keep on `127.0.0.1` and reach it via SSH tunnel
  (`ssh -L 8080:localhost:8080 ...`), or a private SG. Login is username/password
  (bcrypt-hashed, default `admin`/`admin` on first run - change it immediately);
  a successful login issues an `HttpOnly` session cookie, which is what's sensitive here.
- **Node `:11434`** - open only from the mesh box's SG, not the internet.
- Terminate TLS at an ALB or nginx in front of the control plane; the binary speaks plain HTTP.

## 4. Verify

```bash
curl http://<mesh>:11434/health                          # {"status":"ok","nodes":{...}}
curl -H "Authorization: Bearer <key>" http://<mesh>:11434/api/tags
curl -H "Authorization: Bearer <key>" http://<mesh>:11434/api/generate \
     -d '{"model":"llama3.2:3b","prompt":"hi","stream":false}'
```

Point any OpenAI-compatible client at `http://<mesh>:11434/v1` with your key.

## Cost tip

Only the GPU nodes are expensive. Stop them when idle - the mesh box detects the
drop, routes around it, and auto-rejoins them when they come back (verified). Run
the mesh box 24/7 for a few dollars a month; scale GPU capacity independently.
