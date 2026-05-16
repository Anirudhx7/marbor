# Ollama Mesh

> Multi-user reverse proxy and load balancer for Ollama with GPU-aware routing and a built-in React dashboard.

[![Build Status](https://github.com/ollama-mesh/ollama-mesh/actions/workflows/ci.yml/badge.svg)](https://github.com/ollama-mesh/ollama-mesh/actions/workflows/ci.yml)
[![Go Version](https://img.shields.io/github/go-mod/go-version/ollama-mesh/ollama-mesh)](https://golang.org/doc/go1.22)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](https://opensource.org/licenses/MIT)

## Architecture

```text
┌─────────┐   ┌──────────────┐   ┌─────────────┐   ┌──────────┐
│  Client │──▶│ ollama-mesh  │──▶│ Auth Layer  │──▶│  Router  │
└─────────┘   │   :11434     │   └─────────────┘   └────┬─────┘
              └──────────────┘                          │
                                     ┌──────────────────┼──────────────────┐
                                     ▼                  ▼                  ▼
                              ┌────────────┐   ┌────────────┐   ┌────────────┐
                              │ Ollama :11435│   │ Ollama :11436│   │ Ollama :11437│
                              └────────────┘   └────────────┘   └────────────┘
```

## Quick Start

Get up and running in exactly 3 commands:

```bash
cp config.example.yaml config.yaml
make build
./ollama-mesh
```

## Dashboard

The project includes a built-in, production-grade React dashboard served directly from the Go binary. 

[View Dashboard Screenshot](docs/dashboard-screenshot.png) *(Placeholder link)*

Access the dashboard at: `http://localhost:8080`

## Configuration Reference (`config.yaml`)

| Field | Default | Description |
|---|---|---|
| `proxy.port` | `11434` | Proxy listen port |
| `auth.enabled` | `false` | Require API key auth |
| `auth.keys[].rate_limit` | — | Requests per hour |
| `auth.keys[].models` | `all` | Allowed models (optional) |
| `auth.keys[].expires_at` | — | Expiry date YYYY-MM-DD (optional) |
| `nodes[].gpu_model` | — | GPU model name for dashboard |
| `routing.strategy` | `warm-first` | `warm-first` or `round-robin` |
| `routing.poll_interval_ms` | `2000` | `/api/ps` poll interval |
| `routing.fallback` | `least-connections` | Fallback when no warm node |
| `metrics.enabled` | `true` | Prometheus metrics server |
| `metrics.port` | `9090` | Metrics listen port |
| `litellm.enabled` | `false` | Enable LiteLLM middleware |
| `litellm.url` | — | LiteLLM base URL |

## Contributing

We welcome contributions! Please see our [CONTRIBUTING.md](CONTRIBUTING.md) for details on how to run the project locally, execute tests, and submit pull requests.
