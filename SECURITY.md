# Security Policy

## Supported Versions

| Version | Security fixes |
|---------|---------------|
| latest (main) | ✓ active |
| older tags | ✗ update to latest |

ollama-mesh follows a rolling release model. Security fixes ship in new releases; older tagged versions are not backported.

---

## Reporting a Vulnerability

Please **do not** open a public GitHub issue for security vulnerabilities.

Report privately via [GitHub Security Advisories](https://github.com/Anirudhx7/ollama-mesh/security/advisories/new). You will receive a response within 72 hours. If the issue is confirmed, a fix will be published and the advisory will be disclosed publicly after a patch is available.

---

## API Keys and Admin Token

### API keys

API keys are defined in `config.yaml` under `auth.keys`. Each key is a static Bearer token in the `Authorization: Bearer sk-mesh-...` header.

- Keys are matched by **exact string comparison** - substring matching is not used.
- Key **names** are logged in the audit log and request log. The key value itself is never written to any log file.
- The `usage-state.json` file stores per-key counters (token totals, quota counters). It does not store key values.
- Keys are never echoed back through any admin API response.

### Admin token

The admin dashboard and `/admin/v1/` API require a separate Bearer token configured at `auth.admin_token`.

**Set this explicitly.** If `admin_token` is left blank, the process falls back to your first API key, which grants dashboard access to every holder of that key. The config comment warns about this directly.

```yaml
auth:
  admin_token: sk-admin-change-me   # set a strong, unique value
```

Generate a token with at least 32 bytes of entropy:

```bash
openssl rand -hex 32
```

---

## TLS

ollama-mesh does not terminate TLS internally by design. TLS is delegated to a reverse proxy (nginx, Caddy, Traefik, a cloud load balancer, etc.).

**For any deployment reachable from outside your local network or VPN, you must place TLS in front of port 11434 and port 8080.** Without TLS, API keys and admin tokens travel in plaintext.

See [website/PRODUCTION.md](website/PRODUCTION.md) for a working nginx TLS configuration snippet.

The metrics port (9090) should not be exposed to untrusted networks. Scrape it from within your monitoring network only.

---

## What Is and Is Not Logged

| Data | Logged? |
|------|---------|
| Key name (e.g. "team-shared") | ✓ audit log and request log |
| Key value (the `sk-mesh-...` string) | ✗ never |
| Request body / prompt content | ✗ never |
| Response body | ✗ never |
| Model name, node, status, latency | ✓ audit log |
| Cloud provider used | ✓ audit log (`cloud: true`) |
| Request ID (`X-Request-ID`) | ✓ audit log |

The audit log is an append-only JSON-lines file. Enable it in config:

```yaml
audit:
  enabled: true
  path: /var/log/ollama-mesh/audit.log
```

Protect the audit log with appropriate filesystem permissions - it contains request metadata (model names, key names, timestamps) that may be operationally sensitive.

---

## Cloud Provider Keys

Cloud provider API keys (OpenAI, Anthropic) are stored in `config.yaml`. Protect this file:

```bash
chmod 600 /opt/ollama-mesh/config.yaml
chown ollama-mesh:ollama-mesh /opt/ollama-mesh/config.yaml
```

Cloud provider keys are never returned through any admin API endpoint.

---

## Rate Limiting

Every API key has a per-hour token bucket rate limit (`rate_limit` in config). Requests beyond the limit return `429 Too Many Requests`. Optional hard quotas (`daily_limit`, `monthly_limit`) reset at UTC midnight and month boundary respectively. This is enforced in-process and is not a substitute for network-level rate limiting on your reverse proxy.
