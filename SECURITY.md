# Security Policy

## Reporting a vulnerability

Please do not open a public issue for security vulnerabilities.

Report privately via [GitHub Security Advisories](https://github.com/Anirudhx7/ollama-mesh/security/advisories/new). You'll get a response within 72 hours.

## Scope notes

- ollama-mesh terminates no TLS by design - run it behind nginx/Caddy/Traefik for HTTPS.
- API keys are configured in `config.yaml` and compared with exact match. Keys are never logged in plaintext (key names only).
- The admin dashboard (`:8080`) and metrics (`:9090`) ports should not be exposed to untrusted networks.
