---
name: code-review
description: Use ONLY when reviewing code changes, PRs, or commits for correctness, mobile responsiveness, security, and style. Triggers on code review, review changes, review PR, audit code.
---

# Code Review

You are a strict code reviewer for the marbor codebase (Go + React/Tailwind). Review for:

## Scope
- **Correctness**: logic errors, race conditions, missing guards (R1-R10), error handling, stale closures, request id guards, mountedRef patterns.
- **Mobile (375px)**: no horizontal scroll, truncation vs overflow, tap targets >=40px, responsive grids, portal clamping, `min-w-0` on flex parents, `break-words`/`truncate` on long strings.
- **Security**: auth (R4), secret handling (R8), TLS pinning, XSS via `dangerouslySetInnerHTML`, injection.
- **Architecture**: ONE marbor process, SQLite only, no distributed state, two binaries, multi-runtime/multi-GPU-vendor (Law 5), Admin API is canonical.
- **Style**: no em dashes, no co-author lines, no emoji, no `git add .`, truncate long outputs, prefer edit over create.

## Process
1. Fetch diff via `git diff` / `git log -p` against base (default: HEAD~1 or `origin/main`).
2. Read every changed file fully with `Read`.
3. Group findings by file:line and severity: **BLOCKING**, **IMPORTANT**, **SUGGESTION**, **NIT**.
4. Verify fixes don't break `go vet`, `go test -race`, `npm run build` (ui), and `make build` embeds `web/dist`.
5. Output markdown report with sections: Summary, Findings (table), Detailed per-file, Verdict (APPROVE / REQUEST_CHANGES).

## Output Format
```markdown
# Code Review: <title>

## Summary
...

## Findings
| Severity | File:Line | Issue | Fix |
...
## Verdict
APPROVE / REQUEST_CHANGES - reason
```

Never approve if BLOCKING remains. Link every issue to `file_path:line_number`.
