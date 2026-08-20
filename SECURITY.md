# Security Policy

## Supported Versions

| Version | Security fixes |
|---------|---------------|
| latest (main) | ✓ active |
| older tags | ✗ update to latest |

marbor follows a rolling release model. Security fixes ship in new releases; older tagged versions are not backported.

---

## Reporting a Vulnerability

Please **do not** open a public GitHub issue for security vulnerabilities.

Report privately via [GitHub Security Advisories](https://github.com/Anirudhx7/marbor/security/advisories/new). You will receive a response within 72 hours. If the issue is confirmed, a fix will be published and the advisory will be disclosed publicly after a patch is available.

---

## API Keys and Admin Token

### API keys

API keys are generated and managed through the **API Keys** page of the admin dashboard. Each key is a static Bearer token in the `Authorization: Bearer sk-marbor-...` header.

- Keys are matched by **exact string comparison** - substring matching is not used.
- Key **names** are logged in the audit log and request log. The key value itself is never written to any log file.
- Key metadata and usage counters (token totals, quota counters) are persisted in the SQLite database (`marbor.db`).
- Keys are never echoed back through any admin API response.

### Admin dashboard login

The admin dashboard and `/admin/v1/` API are gated by username/password login, not a static token.

- Passwords are bcrypt-hashed; a fresh install creates a well-known `admin` / `admin` account and forces a password change (or an explicit skip) on first login. **Change it immediately in any deployment reachable beyond your own workstation.**
- A successful login issues a session token stored server-side (SQLite) and delivered to the browser as an `HttpOnly`, `SameSite=Lax` cookie - never in `localStorage`, never readable by JavaScript.
- Login is rate-limited to 5 attempts per minute per client IP; admin-triggered password resets are limited to 3 per hour per IP. Both return a generic error on lockout (never revealing whether a username exists).
- The admin server listens on `:8080` (all interfaces) by default for Docker port-mapping compatibility. On a bare-metal or VM deployment reachable from an untrusted network, set the `admin_bind_address` setting to `"127.0.0.1:8080"` via the Settings dashboard, and access it via SSH tunnel or reverse proxy instead.

---

## TLS

marbor does not terminate TLS internally by design. TLS is delegated to a reverse proxy (nginx, Caddy, Traefik, a cloud load balancer, etc.).

**For any deployment reachable from outside your local network or VPN, you must place TLS in front of port 11434 and port 8080.** Without TLS, API keys and admin tokens travel in plaintext.

See [docs/PRODUCTION.md](docs/PRODUCTION.md) for a working nginx TLS configuration snippet.

The metrics port (9090) should not be exposed to untrusted networks. Scrape it from within your monitoring network only.

---

## What Is and Is Not Logged

| Data | Logged? |
|------|---------|
| Key name (e.g. "team-shared") | ✓ audit log and request log |
| Key value (the `sk-marbor-...` string) | ✗ never |
| Request body / prompt content | ✗ never |
| Response body | ✗ never |
| Model name, node, status, latency | ✓ audit log |
| Cloud provider used | ✓ audit log (`cloud: true`) |
| Request ID (`X-Request-ID`) | ✓ audit log |

The audit log is stored directly in SQLite (`marbor.db`). Enable it via the admin Settings dashboard. Old audit entries are pruned automatically based on your configured retention period.

---

## Cloud Provider Keys

Cloud provider API keys (OpenAI, Anthropic) are stored in the SQLite database (`marbor.db`). Protect this file:

```bash
chmod 600 /opt/marbor/marbor.db
chown marbor:marbor /opt/marbor/marbor.db
```

Cloud provider keys are never returned through any admin API endpoint (they are masked as `***` on read).

---

## Rate Limiting

Every API key has a token bucket rate limit. Requests beyond the limit return `429 Too Many Requests`. Optional hard quotas (`daily_limit`, `monthly_limit`) reset at UTC midnight and month boundary respectively. Rate limits and quotas are configured per key in the dashboard, enforced in-process, and are not a substitute for network-level rate limiting on your reverse proxy.
