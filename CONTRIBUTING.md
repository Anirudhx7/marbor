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
- **Tests required** for any new feature or bug fix.
- **`go build ./...` and `go test ./...` must pass** before opening a PR.
- **`config.example.yaml` must be updated** if you add new config fields (with working defaults in `config.go Validate()`).

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
