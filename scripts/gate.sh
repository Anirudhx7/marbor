#!/usr/bin/env bash
# gate.sh — mirrors CI exactly. Green here = green in GitHub Actions.
# Requires Docker Desktop running (Go is not installed locally).
set -euo pipefail

cd "$(dirname "${BASH_SOURCE[0]}")/.."

gorun() {
  # MSYS_NO_PATHCONV=1: disable Git Bash path conversion entirely for Docker args
  # pwd -W: explicit Windows-style path (C:/...) for the bind mount — Docker Desktop requires this
  # ollama-mesh-gomod: named volume caches Go modules across runs (no re-download each time).
  # Mounted at /go/pkg/mod, NOT /root/go/pkg/mod — golang:1.25's default GOPATH is /go
  # (confirmed via `go env GOPATH` in-container), so /root/go/pkg/mod is never read/written
  # and every run silently redownloaded the full module graph from scratch. If this volume
  # ever needs clearing (disk pressure): `docker volume rm ollama-mesh-gomod`. Safe to
  # delete anytime — it's a pure cache, gate.sh repopulates it on the next run.
  # ollama-mesh-gobuild: named volume caching compiled build objects (GOCACHE) —
  # same rationale as ollama-mesh-gomod above. Also safe to `docker volume rm`
  # anytime; it's rebuilt from scratch on the next run.
  MSYS_NO_PATHCONV=1 docker run --rm \
    -v "$(pwd -W):/app" \
    -v "ollama-mesh-gomod:/go/pkg/mod" \
    -v "ollama-mesh-gobuild:/root/.cache/go-build" \
    -w /app \
    -e GOFLAGS=-buildvcs=false \
    golang:1.25 "$@"
}

fail() { echo ""; echo "GATE RED: $*" >&2; exit 1; }

echo "=== [1/5] UI: npm ci + build ==="
if command -v powershell.exe &>/dev/null; then
  # Windows: neither 'rd /s /q' nor 'Remove-Item -Recurse' is 100% reliable
  # alone — Windows Defender or VS Code file watchers lock different packages
  # each run. Strategy: try both methods in a retry loop until the directory
  # is gone or we give up and let npm ci handle it (npm ci also cleans first,
  # and it may succeed even if our pre-delete partially failed).
  powershell.exe -NonInteractive -NoProfile -Command "
    for (\$i = 0; \$i -lt 4; \$i++) {
      if (-not (Test-Path 'ui\node_modules')) { break }
      cmd /c 'rd /s /q ui\node_modules 2>nul'
      if (-not (Test-Path 'ui\node_modules')) { break }
      Remove-Item -Path 'ui\node_modules' -Recurse -Force -ErrorAction SilentlyContinue
      if (-not (Test-Path 'ui\node_modules')) { break }
      Start-Sleep -Milliseconds 800
    }
    Set-Location ui
    npm ci; if (\$LASTEXITCODE -ne 0) { exit \$LASTEXITCODE }
    node node_modules\typescript\lib\tsc.js -b; if (\$LASTEXITCODE -ne 0) { exit \$LASTEXITCODE }
    node node_modules\vite\bin\vite.js build; exit \$LASTEXITCODE
  " || fail "UI build failed"
else
  # Linux/macOS (CI): standard approach works fine.
  rm -rf ui/node_modules 2>/dev/null || true
  (cd ui && npm ci && node node_modules/typescript/lib/tsc.js -b && node node_modules/vite/bin/vite.js build) || fail "UI build failed"
fi

echo "=== [2/5] Go: vet ==="
gorun go vet ./... || fail "go vet failed"

echo "=== [3/5] Go: gofmt check ==="
# Exclude gitignored worktrees/backups (.claude/, .local/) — CI's clean checkout never has
# them, so scanning them here only surfaces stale local artifacts, not real formatting issues.
unformatted=$(gorun bash -c "gofmt -l . | grep -v -e '^\.claude/' -e '^\./\.claude/' -e '^\.local/' -e '^\./\.local/'" || true)
if [ -n "$unformatted" ]; then
  echo "Not gofmt-formatted:"; echo "$unformatted"
  fail "gofmt check failed"
fi

echo "=== [4/5] Go: test -race ==="
gorun go test -race -timeout 120s ./... || fail "go test failed"

echo "=== [5/5] Go: govulncheck ==="
gorun sh -c "go install golang.org/x/vuln/cmd/govulncheck@latest && govulncheck ./..." || fail "govulncheck failed"

echo ""
echo "=== GATE GREEN ==="
