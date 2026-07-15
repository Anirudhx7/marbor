# Contributing to ollama-mesh

## Build

**With Docker (no local Go required):**
```bash
docker run --rm -v "$(pwd):/app" -w /app -e GOFLAGS=-buildvcs=false golang:1.25 sh -c "go build ./..."
```

**Locally (requires Go 1.21+ and Node 20+):**
```bash
make build      # UI + Go binary
make backend    # Go only (fast iteration)
make ui         # UI only
```

## Test

```bash
go test ./...
```

Every new feature needs tests. Every bug fix needs a failing test that reproduces the bug, then passes with the fix.

## Dev UI

```bash
make dev-ui     # Hot-reload UI at :5173, proxies API to :8080
```

Run `./ollama-mesh` (or `make backend`) first so there is a backend to hit.

## Pull Requests

- **Conventional commits:** `feat:`, `fix:`, `chore:`, `docs:`, `refactor:`, `test:`
- **One concern per PR.** Version bumps are always standalone commits.
- **No co-author attribution lines** in commit messages.
- **Sign off every commit (DCO):** commit with `git commit -s`. See [Developer Certificate of Origin](#developer-certificate-of-origin-dco) below. A CI check rejects unsigned commits.
- **Tests required** for any new feature or bug fix.
- **`go build ./...` and `go test ./...` must pass** before opening a PR.
- **New config fields need full DB-first wiring** - there's no config file to update. Add: a default in `config.go Validate()`, a `settings` KV entry (`internal/store/settings_helpers.go`'s `GetSetting`/`SetSetting` helpers), read/write wiring in `handleSettings`/`handleUpdateSettings` (`internal/admin/admin.go`), and a control on the Settings page (`ui/src/pages/Settings.tsx`).

## Developer Certificate of Origin (DCO)

ollama-mesh uses the [Developer Certificate of Origin](https://developercertificate.org/) (DCO) rather than a CLA. The DCO is a lightweight, per-commit statement that you wrote the contribution - or otherwise have the right to submit it under the project's license (Apache-2.0).

**Every commit must be signed off.** Add the `Signed-off-by` trailer automatically with:

```bash
git commit -s -m "fix: ..."
```

which appends a line using your `git config user.name` / `user.email`:

```
Signed-off-by: Your Name <you@example.com>
```

A CI check fails any PR whose commits lack a sign-off. To fix existing commits: `git commit --amend -s` (last commit) or `git rebase --signoff <base>` (a series), then push.

By signing off, you certify the DCO, version 1.1:

```
Developer Certificate of Origin
Version 1.1

Copyright (C) 2004, 2006 The Linux Foundation and its contributors.

Everyone is permitted to copy and distribute verbatim copies of this
license document, but changing it is not allowed.


Developer's Certificate of Origin 1.1

By making a contribution to this project, I certify that:

(a) The contribution was created in whole or in part by me and I
    have the right to submit it under the open source license
    indicated in the file; or

(b) The contribution is based upon previous work that, to the best
    of my knowledge, is covered under an appropriate open source
    license and I have the right under that license to submit that
    work with modifications, whether created in whole or in part
    by me, under the same open source license (unless I am
    permitted to submit under a different license), as indicated
    in the file; or

(c) The contribution was provided directly to me by some other
    person who certified (a), (b) or (c) and I have not modified
    it.

(d) I understand and agree that this project and the contribution
    are public and that a record of the contribution (including all
    personal information I submit with it, including my sign-off) is
    maintained indefinitely and may be redistributed consistent with
    this project or the open source license(s) involved.
```

## Code Style

- **Go stdlib first.** Add a dependency only when stdlib genuinely cannot do the job.
- **No `panic()` outside `main()`.** Return errors; let callers decide.
- **Wrap errors with context:** `fmt.Errorf("poll node %s: %w", n.Name, err)`
- **Concurrent map access requires a mutex.** `auth.go` maps use `m.mu.RWMutex` - `Lock()` on writes, `RLock()` on reads.
- **No fake data in production paths.** Unknown values return `null` or a dash, never invented numbers.
- **Streaming must stay streaming.** Never buffer the proxy response path - `statusRecorder` must implement `http.Flusher`.
- **Admin token auth is exact match.** Never `strings.Contains`.

## Issues

- **Bug report:** include ollama-mesh version (`./ollama-mesh --version`), OS, config snippet (redact keys/tokens), and the exact error output.
- **Feature request:** describe the problem it solves and who it helps. A config snippet showing what the YAML would look like is a bonus.

## Security

Do not open public issues for security vulnerabilities. Open a draft private security advisory on GitHub instead.
