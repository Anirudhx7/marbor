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
- An API key whose stored `expires_at` cannot be parsed is **rejected** (treated as expired, with a rate-limited server warning naming the key, never its value) and shows as `expired` in the key list. Creating or editing a key rejects a malformed value outright; an empty `expires_at` means no expiry. The first time a given key carries a given malformed value (at startup, reload, or key create/update), a system-audit event `api_key_expiry_malformed` is recorded naming the key, never its value or the malformed string; it is not repeated per request.
- Key **names** are logged in the audit log and request log. The key value itself is never written to any log file.
- The request audit log is **best-effort**: it records requests that reach proxy completion handling. Authentication and policy rejections that occur earlier (missing/invalid/expired key, rate limit, quota) are not persisted in it, and entries are dropped when the async write queue is full (`marbor_audit_dropped_total`). Do not rely on it as a lossless security audit trail.
- Key metadata and usage counters (token totals, quota counters) are persisted in the SQLite database (`marbor.db`).
- Keys are never echoed back through any admin API response.

### Admin dashboard login

The admin dashboard and `/admin/v1/` API are gated by username/password login, not a static token.

- Passwords are bcrypt-hashed (cost 10, the Go `bcrypt.DefaultCost`) on every path that sets or resets one: fresh install, user creation, password change and admin reset. Credentials carried over from the legacy single-admin table by very old versions may hold an iterated SHA-256 hash instead; login verifies bcrypt only, so such a hash cannot authenticate. On first start with no admin users, a bcrypt legacy hash is migrated unchanged (same username, same password). An unusable legacy hash is **not** copied: the legacy username is recreated as an active admin on the documented default password (`admin`) with a forced password change that cannot be skipped, and a startup warning names the account (never the hash). Until that administrator logs in and changes the password, anyone who can reach the dashboard can log in as them, so log in immediately. A fresh install no longer creates a well-known account: see [Default admin login](#default-admin-login) below.
- A successful login issues a session token stored server-side (SQLite) and delivered to the browser as an `HttpOnly`, `SameSite=Lax` cookie - never in `localStorage`, never readable by JavaScript.
- Login is rate-limited to 5 attempts per minute per client IP; admin-triggered password resets are limited to 3 per hour per IP. Both return a generic error on lockout (never revealing whether a username exists).
- The admin server listens on `:8080` (all interfaces) by default for Docker port-mapping compatibility. On a bare-metal or VM deployment reachable from an untrusted network, set the `admin_bind_address` setting to `"127.0.0.1:8080"` via the Settings dashboard, and access it via SSH tunnel or reverse proxy instead.

---

## TLS

marbor does not terminate TLS internally by design. TLS is delegated to a reverse proxy (nginx, Caddy, Traefik, a cloud load balancer, etc.).

**For any deployment reachable from outside your local network or VPN, you must place TLS in front of port 11434 and port 8080.** Without TLS, API keys and admin tokens travel in plaintext.

See [docs/PRODUCTION.md](docs/PRODUCTION.md) for a working nginx TLS configuration snippet.

### Default admin login

**Fresh installs have no default password.** On first start with no administrator, marbor creates the user `admin` with a random password (24 or more characters from `crypto/rand`) and forces a change at first login that cannot be skipped. The password is written once to `initial-admin-password` in the data directory (next to `marbor.db`), created with mode 0600 and exclusive-create semantics (on Windows the file is restricted to the service account and the restriction is re-verified with `icacls` on every later start, failing closed with the fix command if another principal has access). The startup banner prints that path, never the password. The password is not written to stdout, logs or the journal, and is never accepted as a command-line argument. It does not rotate on restart while it is pending. After the first successful password change the file is overwritten, synced and removed (best effort on journaling filesystems); a stale file found at boot is removed with a warning.

To choose the initial password instead of generating one, set one of these before the first start (read only when the first administrator is created, and they win over generation):

- `MARBOR_ADMIN_PASSWORD_FILE`: path to a file containing the password. Preferred: it works with Docker and Compose secrets (`/run/secrets/...`), and the environment carries a path, not the secret.
- `MARBOR_ADMIN_PASSWORD`: the password itself. Accepted but weaker: it is visible through `docker inspect` and process environment listings. No file shipped in this repository sets it. After reading it marbor unsets the variable in its own process, which only stops child processes it starts from inheriting it. That does not clear `/proc/<pid>/environ`, `docker inspect` or the container or unit definition, so prefer a secrets file, or remove the variable from the container definition after the first boot.

A supplied password is still an initial password: the forced change applies, and the refusal below applies until it is changed. A supplied password is never regenerated or overwritten: if you remove the variable or unmount the secret before changing the password, marbor does not invent a new one, and you must restore the same value. The minimum length (12) counts characters, not bytes, and the new password must differ from the one it replaces.

**Where the sign-in notice points.** The login page, `marbor status` and `GET /health` say where the pending password comes from: `file` (a generated password in `initial-admin-password`), `supplied` (the one you configured), or `default` (the public `admin` credentials of an upgraded or legacy-recovered install). The `/health` fields `bootstrap_password_pending` and `bootstrap_password_source` are shown only to a direct loopback caller: a request with a non-loopback peer address, or carrying an `X-Forwarded-For`, `Forwarded`, `X-Real-IP`, `CF-Connecting-IP`, `True-Client-IP`, `X-Client-IP`, `Via`, `X-Forwarded-Host` or `Forwarded-Host` header, gets neither. A reverse proxy on the same host that forwards to a loopback admin bind makes every remote caller arrive from loopback, so it exposes the login itself by design; the pending flag is merely hidden when the proxy adds its headers. Do not forward a public proxy to a loopback-only admin bind while a password is pending.

**Refusal on non-loopback binds.** While an initial or default admin password is active, marbor refuses to start the admin dashboard on a non-loopback bind. Only the admin listener is refused: the proxy and metrics listeners start normally, and the log says why. Loopback is decided from the address the listener actually bound, never from the configured string: `127.0.0.0/8` and `::1` are loopback; `0.0.0.0`, `::`, an empty host, a LAN address, and a `localhost` that resolves to anything non-loopback are not. If the password file cannot be created or has unsafe permissions (a symlink, wrong owner, wrong mode), the admin listener also does not start and nothing falls back to a default password.

**Recovery without a login.** Start marbor with `MARBOR_ADMIN_BIND_ADDRESS=127.0.0.1:8080` (any `host:port` that is loopback). This environment-only override takes precedence over the stored `admin_bind_address` setting for that process and is never written to the database. Reach the dashboard locally or through an SSH tunnel, change the password, then restart without the override. Marbor provides no offline password-reset command and no unauthenticated reset: recovery from a locked-out deployment requires trusted administration of the host, and editing `marbor.db` by hand is not a supported recovery method. If no administrator exists at boot (possible only through a host-level change, because the API refuses to delete or demote the last admin), marbor treats the database as a fresh install and generates a new initial password.

**Docker.** The shipped image and compose files set no password and do not disable the refusal. The container binds the admin dashboard to all interfaces, so an unmodified container with a generated or supplied initial password has its admin dashboard refused until the password is changed. Supported ways to get the first login done: mount a secret and set `MARBOR_ADMIN_PASSWORD_FILE` to its path, then make the first login over loopback (`MARBOR_ADMIN_BIND_ADDRESS=127.0.0.1:8080` with `docker exec` or host networking); or read the generated file once with `docker exec <container> cat /data/initial-admin-password` and do the same. See [docs/PRODUCTION.md](docs/PRODUCTION.md).

**Explicit escape hatch.** `MARBOR_ALLOW_INSECURE_DEFAULT_ADMIN=true` (exactly `true`; any other value is off; environment variable only, not a flag) lets the admin listener start on a non-loopback bind with an initial password active. Every boot it is in effect logs a warning and records a system audit entry. It serves a plaintext dashboard with a known-weak credential state to anyone who can reach it. No shipped file sets it. Prefer the loopback recovery path.

**Upgrades.** An existing database that is still on the factory `admin` / `admin` password gets one release of warn-only grace: through v0.25.x marbor logs a warning on every boot and still serves the dashboard, and from v0.26.0 it is refused on a non-loopback bind like any other initial password. A source or development build (a version that is not a release tag) has no grace and enforces the refusal at once. A legacy-recovery account (the administrator recreated on the default password with the skip disabled, described above) is enforced immediately with no grace. Change the password now, or use the loopback recovery path above.

**When the check runs.** The refusal is decided once, at startup, from the state at that moment. Changing the password later does not start a dashboard that was refused, and saving a non-loopback `admin_bind_address` while a password is pending only takes effect at the next restart, when the dashboard would then be refused: the Settings page and `marbor settings set` warn about exactly that when the save is accepted.

**Demo.** The bundled demo stacks keep the public `admin` / `admin` login on a seeded database and are exempt only through the explicit `MARBOR_DEMO_MODE=true` setting (exactly `true`), only while an administrator really is on the public default password and never for a freshly generated or supplied initial password, and every start that relies on it writes a system audit entry; never because a bind address happens to be local. The demo setting must not be used on a real deployment; it logs a warning on every boot. In particular, `MARBOR_DEMO_MODE=true` on a real database that is still on `admin` / `admin` lets a non-loopback bind serve that public login (the documented demo contract, audit-logged), so never copy the demo compose environment into a production file.

**Threat model.** In scope: a remote, unauthenticated attacker with network reach to the admin port before the first login, and anyone scraping logs. Out of scope: anyone who can read the data directory or `marbor.db` (they can already read the password file and the database) and root or administrator on the host. The password file is only as private as its operating-system permissions. The dashboard is plaintext HTTP: put TLS in front of any exposed deployment regardless of this check.

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
| Source IP | Admin login and admin-change trail (`system_audit_log`) only. Per-request client IPs are held in memory for recent-request views and are not written to the audit log, request log or access log. |

Operators should treat the following as potentially sensitive when forwarding logs to external systems (Loki, Datadog, Splunk and similar), because each can carry tenant, project or workload identity:

- **API key names** - often a team, customer or project name. Present in the audit log, request log and access log.
- **Model names** (including aliases) - can reveal a confidential or fine-tuned workload.
- **Node names, cloud provider identifiers and source IP addresses** - infrastructure topology and client location.
- **Request IDs** - random, safe on their own, but they correlate entries across systems.

marbor does not currently hash or redact any of these fields (key names, model names and aliases, node names, cloud provider names, source IPs, request IDs) in logs. Choose key names accordingly, and apply retention to forwarded copies the same way the `audit_retention_days` setting does for the local audit log.

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
