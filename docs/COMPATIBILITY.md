# Compatibility & Interface Standards

This document outlines Marbor's interface design principles, compatibility practices, runtime and hardware validation matrix, and prerequisites for production operations.

---

## Interface Stability and Evolution

Marbor is designed to operate safely across long-running heterogeneous fleets. Operational interfaces follow deliberate stability and evolution practices:

### 1. Admin API v1
- The `/admin/v1/*` routes constitute the canonical control-plane interface. Both the Web UI and the CLI are peer clients of the Admin API; every user-visible capability exists first in the Admin API.
- Changes are additive whenever possible. While backward compatibility is prioritized, pre-1.0 development may occasionally adjust or retire unworkable endpoints or parameters; any breaking change is called out prominently in `CHANGELOG.md` with migration context.

### 2. Node Agent Protocol v1
- The node agent communication protocol (`/v1/status` and `/metrics` between Marbor and `marbor-agent`) follows a frozen v1 wire structure.
- Designed for rolling upgrades: a Marbor control-plane instance and `marbor-agent` daemons can run across differing versions during rollout cycles without requiring lockstep simultaneous updates.
- Wire fields are additive; unknown fields are ignored on decode, and missing fields are treated as unknown rather than assumed zero-values.
- New functionality is exposed via explicit `Capabilities` strings rather than breaking wire changes. When an unavoidable wire format change occurs, it is accompanied by migration guidance.

### 3. Command Line Interface (CLI)
- CLI commands (`marbor ...`), command flags, JSON output envelopes (`--json`), and exit codes aim for consistent backward compatibility across releases. Where flags evolve or parity between the CLI and Admin API is closed, changes and deprecations are detailed in release notes.

### 4. SQLite Datastore and Migrations
- SQLite (`marbor.db`) is the datastore.
- Schema migrations run forward automatically on process startup.
- Downgrading an existing database file to an older binary version is unsupported. If you must roll back a Marbor version, restore from a database backup taken prior to the upgrade.

### 5. Scope
- These practices apply to Marbor's operational interfaces, schemas, and wire contracts.
- They do not guarantee the third-party stability of underlying inference engines (e.g. upstream vLLM, Ollama, or llama.cpp internal bugs).

---

## Supported vs. Tested Matrix

Marbor distinguishes between **Supported** and **Tested**:
- **Supported:** Code-level adapters, request/response translation, health probes, and telemetry parsers exist and pass test suites.
- **Tested (Validated):** Exercised and verified against physical GPU hardware in real production environments.

| Runtime \ Hardware | NVIDIA | AMD (ROCm) | Intel (oneAPI) | Apple Silicon (Metal) |
|---|---|---|---|---|
| **Ollama** | VALIDATED | Supported (untested on real GPU) | Supported (untested on real GPU) | Supported (untested on real GPU) |
| **vLLM** | Supported (mock-tested) | Supported (mock-tested) | Supported (mock-tested) | Unsupported upstream |
| **llama.cpp** | Supported (mock-tested) | Supported (mock-tested) | Supported (mock-tested) | Supported (mock-tested) |
| **MLX** | Unsupported upstream | Unsupported upstream | Unsupported upstream | Supported (manual registration) |
| **TGI** | Frozen legacy | Frozen legacy | Frozen legacy | Unsupported upstream |

### Notes on Hardware and Runtimes
- **MLX on Apple Silicon:** MLX (`mlx_lm.server`) runs exclusively on Apple Silicon via Metal. Because subnet scans do not probe MLX ports, MLX nodes must be registered manually via the Web UI (**GPU Nodes** page) or CLI (`marbor nodes add <name> <url> --runtime mlx`).
- **AMD and Intel GPU Telemetry:** `marbor-agent` parses `rocm-smi` and `xpu-smi` according to vendor documentation. Telemetry collection has not yet been validated against physical AMD or Intel GPU clusters.
- **TGI:** Hugging Face archived TGI in March 2026. Existing support receives bug fixes only; no new features are planned.

---

## Prerequisites and Installation Paths

### Operating Systems
- **Linux:** x86_64 (amd64) and aarch64 (arm64). Supports systemd services (`SERVICE=1`), Docker, or direct execution with nohup / process supervisors.
- **macOS:** Apple Silicon (arm64) native binary. Direct execution or homebrew-style invocation.
- **Windows:** Supported for local development.

### Network Ports
- `:8080` - Admin API and Web Dashboard
- `:11434` - OpenAI and Ollama compatible inference proxy
- `:9090` - Prometheus metrics (optional, disabled by default)
- `:9200` - `marbor-agent` telemetry daemon (on remote GPU nodes)

### First-Run Access and Authentication
- A fresh installation creates a default administrative user (`admin` / `admin`).
- The first login enforces an unskippable password change.
- The admin dashboard is never restricted to loopback by default, allowing operators to deploy on headless servers and configure from an external browser.

### Secrets and Encryption Keys
- Secrets at rest (cloud provider API keys, agent tokens, webhook secrets) are encrypted in `marbor.db` using AES-256-GCM.
- The encryption key is sourced from the `MARBOR_ENCRYPTION_KEY` environment variable or read from `marbor.db.key` (generated with 0600 permissions).
- **Back up `marbor.db.key` alongside `marbor.db`:** If the key file is deleted or lost, existing encrypted credentials cannot be decrypted and will be orphaned until manually re-entered.

---

## Docker Networking and Discovery

When deploying Marbor via Docker or Docker Compose:
- **Bridge Isolation:** Containers running on the default bridge network cannot scan or reach local subnet backends via network auto-discovery unless they are bridged or port-mapped.
- **Host Networking:** To enable subnet scanning for LAN backends, configure `network_mode: host` in your compose file (Linux hosts).
- **Manual Registration:** When using bridge networking, add backend GPU nodes manually by entering their reachable LAN IP or hostname in the **GPU Nodes** page.
- **Zero-Result Rule:** If network discovery returns 0 backends, it indicates that no reachable backends were discovered from this container's network mode. It does not mean no GPUs exist on the physical hosts.
