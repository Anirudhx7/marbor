#!/usr/bin/env bash
# gate.sh — mirrors CI exactly. Green here = green in GitHub Actions.
# Requires Docker Desktop running (Go is not installed locally).
set -euo pipefail

cd "$(dirname "${BASH_SOURCE[0]}")/.."

gorun() {
  # MSYS_NO_PATHCONV=1: disable Git Bash path conversion entirely for Docker args
  # pwd -W: explicit Windows-style path (C:/...) for the bind mount — Docker Desktop requires this
  # ollama-mesh-gomod: named volume caches Go modules across runs (no re-download each time)
  MSYS_NO_PATHCONV=1 docker run --rm \
    -v "$(pwd -W):/app" \
    -v "ollama-mesh-gomod:/root/go/pkg/mod" \
    -w /app \
    -e GOFLAGS=-buildvcs=false \
    golang:1.25 "$@"
}

fail() { echo ""; echo "GATE RED: $*" >&2; exit 1; }

echo "=== [1/5] UI: npm ci + build ==="
# Call node directly — avoids all Windows PATH/cmd.exe/npx resolution issues.
# lib/tsc.js and bin/vite.js are stable published paths for both packages.
(cd ui && npm ci && node node_modules/typescript/lib/tsc.js -b && node node_modules/vite/bin/vite.js build) || fail "UI build failed"

echo "=== [2/5] Go: vet ==="
gorun go vet ./... || fail "go vet failed"

echo "=== [3/5] Go: gofmt check ==="
unformatted=$(gorun gofmt -l .)
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
