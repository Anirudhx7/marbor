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
- An API key whose stored `expires_at` cannot be parsed is **rejected** (treated as expired, with a rate-limited server warning naming the key, never its value) and shows as `expired` in the key list. Creating or editing a key rejects a malformed value outright; an empty `expires_at` means no expiry.
- Key **names** are logged in the audit log and request log. The key value itself is never written to any log file.
- The request audit log is **best-effort**: it records requests that reach proxy completion handling. Authentication and policy rejections that occur earlier (missing/invalid/expired key, rate limit, quota) are not persisted in it, and entries are dropped when the async write queue is full (`marbor_audit_dropped_total`). Do not rely on it as a lossless security audit trail.
- Key metadata and usage counters (token totals, quota counters) are persisted in the SQLite database (`marbor.db`).
- Keys are never echoed back through any admin API response.

### Admin dashboard login

The admin dashboard and `/admin/v1/` API are gated by username/password login, not a static token.

- Passwords are bcrypt-hashed (cost 10, the Go `bcrypt.DefaultCost`) on every path that sets or resets one: fresh install, user creation, password change and admin reset. Credentials carried over from the legacy single-admin table by very old versions may hold an iterated SHA-256 hash instead; login verifies bcrypt only, so such a hash cannot authenticate. On first start with no admin users, a bcrypt legacy hash is migrated unchanged (same username, same password). An unusable legacy hash is **not** copied: the legacy username is recreated as an active admin on the documented default password (`admin`) with a forced password change that cannot be skipped, and a startup warning names the account (never the hash). Until that administrator logs in and changes the password, anyone who can reach the dashboard can log in as them, so log in immediately. A fresh install creates a well-known `admin` / `admin` account and forces a password change (or an explicit skip) on first login. **Change it immediately in any deployment reachable beyond your own workstation.**
- A successful login issues a session token stored server-side (SQLite) and delivered to the browser as an `HttpOnly`, `SameSite=Lax` cookie - never in `localStorage`, never readable by JavaScript.
- Login is rate-limited to 5 attempts per minute per client IP; admin-triggered password resets are limited to 3 per hour per IP. Both return a generic error on lockout (never revealing whether a username exists).
- The admin server listens on `:8080` (all interfaces) by default for Docker port-mapping compatibility. On a bare-metal or VM deployment reachable from an untrusted network, set the `admin_bind_address` setting to `"127.0.0.1:8080"` via the Settings dashboard, and access it via SSH tunnel or reverse proxy instead.

---

## TLS

marbor does not terminate TLS internally by design. TLS is delegated to a reverse proxy (nginx, Caddy, Traefik, a cloud load balancer, etc.).

**For any deployment reachable from outside your local network or VPN, you must place TLS in front of port 11434 and port 8080.** Without TLS, API keys and admin tokens travel in plaintext.

See [docs/PRODUCTION.md](docs/PRODUCTION.md) for a working nginx TLS configuration snippet.

### Default admin login (current behavior and a proposed hardening)

A fresh install creates the well-known `admin` / `admin` account and requires a password change at first login. marbor logs a startup warning on every boot while that password is still active, saying the default login is live, that a change is required, and that the dashboard is plaintext HTTP. It does **not** refuse a non-loopback admin bind and does **not** generate a random first-boot password. Until you change the password, any host that can reach port 8080 can take over the control plane: change it immediately, keep the dashboard on `127.0.0.1` or put TLS in front of it (reverse proxy) for any exposed deployment.

*Proposed, not current behavior:* a generated first-boot password and/or refusing a non-loopback admin bind while the default credential is active. This changes the quickstart and the Docker/demo flow, so it is tracked as a separate item.

### marbor-agent transport

`marbor-agent service install` provisions a TLS certificate automatically, so installed agents serve HTTPS. A foreground `marbor-agent` started without `--cert`/`--key` is plaintext, and the agent's bearer token authorizes destructive operations, so it refuses to start on a non-loopback bind (the default bind is all interfaces) unless you do one of:

- serve TLS: pass both `--cert` and `--key`, or use `marbor-agent service install`;
- restrict it to the host: `--bind=127.0.0.1`;
- explicitly accept the risk on a trusted, isolated network: `--allow-insecure-plaintext`.

`--bind` takes a literal IP without a port; hostnames such as `localhost` are not trusted as loopback. Passing only one of `--cert`/`--key` is always refused. An agent service installed before TLS provisioning existed must be re-installed (`marbor-agent service install`) after upgrading, or it will refuse to start. Trust is never inferred from how the agent was deployed.

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

Operators should treat the following as potentially sensitive when forwarding logs to external systems (Loki, Datadog, Splunk and similar), because each can carry tenant, project or workload identity:

- **API key names** - often a team, customer or project name. Present in the audit log, request log and access log.
- **Model names** (including aliases) - can reveal a confidential or fine-tuned workload.
- **Node names, cloud provider identifiers and source IP addresses** - infrastructure topology and client location.
- **Request IDs** - random, safe on their own, but they correlate entries across systems.

marbor does not currently hash or redact key names in logs. Choose key names accordingly, and apply retention to forwarded copies the same way the `audit_retention_days` setting does for the local audit log.

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
