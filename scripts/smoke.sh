#!/usr/bin/env bash
# smoke.sh - gates the `make demo` path with real pass/fail assertions.
# Brings up the demo stack, hits auth/routing/streaming/admin/metrics, tears down, exits 0/1.
set -uo pipefail

cd "$(dirname "${BASH_SOURCE[0]}")/.."

COMPOSE="docker compose -f docker-compose.demo.yml"
ADMIN_TOKEN="demo-admin-token"
BAD_KEY="not-a-real-key"

check_bin=""
check_db=""
cleanup() {
  echo "=== Tearing down demo stack ==="
  $COMPOSE down -v >/dev/null 2>&1 || true
  rm -f "$check_bin" "$check_db" "${check_db:+$check_db.key}"
}
trap cleanup EXIT

fail() { echo ""; echo "SMOKE FAILED: $*" >&2; exit 1; }

echo "=== [0/5] mesh.demo.db drift check (schema vs live migrate() + seed_demo.sql) ==="
if ! command -v sqlite3 &>/dev/null; then
  echo "sqlite3 not found on PATH, skipping drift check" >&2
elif ! command -v go &>/dev/null; then
  echo "go not found on PATH, skipping drift check" >&2
else
  check_bin="$(mktemp -u)"
  check_db="$(mktemp -u).db"
  go build -o "$check_bin" . || fail "build for mesh.demo.db drift check failed"
  "$check_bin" -db "$check_db" -seed-node "name=_schema_init,url=http://init,runtime=ollama" >/dev/null 2>&1 \
    || fail "seed-node step failed against freshly migrated schema"
  sqlite3 "$check_db" < scripts/seed_demo.sql || fail "seed_demo.sql failed against freshly migrated schema"
  # Compare sqlite_master DDL rows directly (one row per statement, already
  # newline-free) rather than parsing `.schema` dot-command text - immune to
  # any future sqlite3 CLI formatting changes in how it prints ".schema".
  normalize() { sqlite3 "$1" "SELECT sql FROM sqlite_master WHERE sql IS NOT NULL ORDER BY name" | tr -s ' \t'; }
  if ! diff <(normalize mesh.demo.db) <(normalize "$check_db") >/dev/null; then
    fail "mesh.demo.db schema has drifted from current migrate()/seed_demo.sql - run 'make demo-db' to regenerate and commit the result"
  fi
fi

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
echo "$metrics_body" | grep -q "ollamamesh_" || fail "metrics body missing ollamamesh_ prefix"

echo ""
echo "=== SMOKE GREEN ==="
