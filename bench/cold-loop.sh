#!/usr/bin/env bash
# cold-loop.sh - BENCH-RUNBOOK.md Step 2: n>=10 cold TTFT samples, evicting the
# model from VRAM before every single sample (bench/ttft.go's own -n loop only
# evicts before sample #1, not before each one).
#
# Requires bench/ttft already built (see BENCH-RUNBOOK.md Step 2's docker build
# command, or `go build -o bench/ttft ./bench` if Go is installed locally).
#
# ollama-mesh is fully DB-based (mesh.db) - there is no config.yaml. Admin
# auth is session-based (POST /admin/login with an admin-role account's
# username/password, same as the dashboard login), not a static bearer token.
#
# Usage:
#   MESH_URL=http://localhost:11434 ADMIN_URL=http://localhost:8080 \
#   ADMIN_USERNAME=admin ADMIN_PASSWORD=<password> NODE_NAME=gpu-node-01 \
#   MODEL=llama3.2:3b-q4_k_m API_KEY=<key> ./bench/cold-loop.sh [n]
#
# n defaults to 10. Prints p50/min/max at the end and writes raw samples to
# cold_samples.log (overwritten each run) in the current directory.
set -uo pipefail

: "${MESH_URL:=http://localhost:11434}"
: "${ADMIN_URL:=http://localhost:8080}"
: "${ADMIN_USERNAME:?ADMIN_USERNAME is required (an admin-role account's username)}"
: "${ADMIN_PASSWORD:?ADMIN_PASSWORD is required (that account's password)}"
: "${NODE_NAME:?NODE_NAME is required (exact name from GET /admin/nodes)}"
: "${MODEL:?MODEL is required (exact tag)}"
: "${API_KEY:?API_KEY is required (a valid client API key)}"

N="${1:-10}"
TTFT_BIN="$(dirname "${BASH_SOURCE[0]}")/ttft"
LOG_FILE="cold_samples.log"
COOKIEJAR="$(mktemp)"
cleanup() { rm -f "$COOKIEJAR"; }
trap cleanup EXIT

if [ ! -x "$TTFT_BIN" ]; then
  echo "cold-loop.sh: ${TTFT_BIN} not found or not executable." >&2
  echo "Build it first (BENCH-RUNBOOK.md Step 2), e.g.:" >&2
  echo "  docker run --rm -v \"\${PWD}:/app\" -w /app -e GOFLAGS=-buildvcs=false golang:1.25.12 go build -o bench/ttft ./bench" >&2
  exit 1
fi

echo "=== Admin login ==="
login_status="$(curl -sS -o /dev/null -w '%{http_code}' -c "$COOKIEJAR" \
  -X POST "${ADMIN_URL}/admin/login" \
  -H "Content-Type: application/json" \
  -d "{\"username\":\"${ADMIN_USERNAME}\",\"password\":\"${ADMIN_PASSWORD}\"}")"
if [ "$login_status" != "200" ]; then
  echo "cold-loop.sh: POST ${ADMIN_URL}/admin/login returned HTTP ${login_status} (expected 200) - check ADMIN_USERNAME/ADMIN_PASSWORD" >&2
  exit 1
fi

: > "$LOG_FILE"

echo "=== Cold TTFT loop: n=${N}, evicting '${MODEL}' on '${NODE_NAME}' before every sample ==="

for i in $(seq 1 "$N"); do
  echo "--- cold sample ${i}/${N} ---"

  unload_status="$(curl -sS -o /dev/null -w '%{http_code}' -X POST \
    "${ADMIN_URL}/admin/nodes/${NODE_NAME}/unload" \
    -b "$COOKIEJAR" \
    -H "Content-Type: application/json" \
    -d "{\"model\":\"${MODEL}\"}")"

  if [ "$unload_status" != "200" ] && [ "$unload_status" != "204" ]; then
    echo "cold-loop.sh: unload request for sample ${i} returned HTTP ${unload_status} - aborting" >&2
    exit 1
  fi

  sleep 2  # let the unload actually land before firing the request

  "$TTFT_BIN" -url "${MESH_URL}" -model "${MODEL}" -n 1 -api-key "${API_KEY}" \
    | tee -a "$LOG_FILE"
done

echo ""
echo "=== Aggregating ${N} cold samples from ${LOG_FILE} ==="

python3 -c "
import re, sys
vals = []
with open('$LOG_FILE') as f:
    for line in f:
        m = re.match(r'\s*\d+\s+([\d.]+)\s+HTTP', line)
        if m:
            vals.append(float(m.group(1)))
if not vals:
    print('No successful samples parsed from ${LOG_FILE} - check the log for errors.', file=sys.stderr)
    sys.exit(1)
vals.sort()
n = len(vals)
p50 = vals[(n - 1) // 2] if n % 2 else (vals[n // 2 - 1] + vals[n // 2]) / 2
print(f'n={n}  p50={p50:.1f} ms  min={vals[0]:.1f} ms  max={vals[-1]:.1f} ms')
if n < int('$N'):
    print(f'WARNING: only {n}/${N} samples succeeded - see ${LOG_FILE} for errors', file=sys.stderr)
"
