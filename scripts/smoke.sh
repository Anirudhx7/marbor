#!/usr/bin/env bash
# smoke.sh — gates the `make demo` path with real pass/fail assertions.
# Brings up the demo stack, hits auth/routing/streaming/admin/metrics, tears down, exits 0/1.
set -uo pipefail

cd "$(dirname "${BASH_SOURCE[0]}")/.."

COMPOSE="docker compose -f docker-compose.demo.yml"
ADMIN_TOKEN="demo-admin-token"
BAD_KEY="not-a-real-key"

cleanup() {
  echo "=== Tearing down demo stack ==="
  $COMPOSE down -v >/dev/null 2>&1 || true
}
trap cleanup EXIT

fail() { echo ""; echo "SMOKE FAILED: $*" >&2; exit 1; }

echo "=== [1/5] Build + start demo stack ==="
$COMPOSE build || fail "demo-build failed"
$COMPOSE up -d ollama-node-a ollama-node-b mesh || fail "compose up failed"

echo "=== [2/5] Wait for mesh health ==="
ok=0
for i in $(seq 1 30); do
  if curl -fsS "http://localhost:8080/health" >/dev/null 2>&1; then
    ok=1
    break
  fi
  sleep 1
done
[ "$ok" = "1" ] || fail "mesh /health never became ready after 30s"

echo "=== [3/5] Auth check: bad API key must be rejected ==="
status=$(curl -s -o /dev/null -w "%{http_code}" -X POST "http://localhost:11434/api/generate" \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer $BAD_KEY" \
  -d '{"model":"llama3.2:3b","prompt":"hi","stream":false}')
case "$status" in
  401|403) ;;
  *) fail "expected 401/403 for bad API key, got $status" ;;
esac

echo "=== [4/5] Routing + streaming check: run demotraffic ==="
$COMPOSE run --rm demotraffic || fail "demotraffic reported failed requests"

echo "=== [5/5] Admin + metrics check ==="
summary_status=$(curl -s -o /dev/null -w "%{http_code}" "http://localhost:8080/admin/metrics/summary" \
  -H "Authorization: Bearer $ADMIN_TOKEN")
[ "$summary_status" = "200" ] || fail "expected 200 from /admin/metrics/summary, got $summary_status"

metrics_body=$(curl -fsS "http://localhost:9090/metrics") || fail "metrics endpoint on :9090 unreachable"
echo "$metrics_body" | grep -q "ollama_mesh_" || fail "metrics body missing ollama_mesh_ prefix"

echo ""
echo "=== SMOKE GREEN ==="
