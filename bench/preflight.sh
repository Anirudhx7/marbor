#!/usr/bin/env bash
# preflight.sh - fail loudly BEFORE bench day wastes an hour on a broken setup.
#
# Checks (per BENCH-RUNBOOK.md Step 1 and Step 0's "Admin endpoints used
# throughout"): admin token works, node is reachable and healthy, model is
# pulled on that node, and model's on-disk size is under 80% of the node's
# total VRAM (Step 1's fit rule).
#
# Usage:
#   MESH_URL=http://localhost:11434 ADMIN_URL=http://localhost:8080 \
#   ADMIN_TOKEN=<token> NODE_NAME=gpu-node-01 MODEL=llama3.2:3b-q4_k_m \
#   ./bench/preflight.sh
set -uo pipefail

: "${MESH_URL:=http://localhost:11434}"
: "${ADMIN_URL:=http://localhost:8080}"
: "${ADMIN_TOKEN:?ADMIN_TOKEN is required (admin_token from config.yaml)}"
: "${NODE_NAME:?NODE_NAME is required (exact name from GET /admin/nodes)}"
: "${MODEL:?MODEL is required (exact tag, e.g. llama3.2:3b-q4_k_m)}"

fail() { echo "PREFLIGHT FAILED: $*" >&2; exit 1; }
ok() { echo "  OK: $*"; }

if ! command -v curl &>/dev/null; then
  fail "curl not found on PATH"
fi
if ! command -v python3 &>/dev/null; then
  fail "python3 not found on PATH (needed to parse JSON responses)"
fi

echo "=== [1/4] Admin token check ==="
health_resp="$(curl -sS -o /dev/null -w '%{http_code}' \
  -H "Authorization: Bearer ${ADMIN_TOKEN}" "${ADMIN_URL}/admin/nodes" 2>/dev/null)"
if [ "$health_resp" != "200" ]; then
  fail "GET ${ADMIN_URL}/admin/nodes with the given ADMIN_TOKEN returned HTTP ${health_resp} (expected 200) - check ADMIN_TOKEN matches config.yaml's admin_token"
fi
ok "admin token accepted (HTTP 200 on /admin/nodes)"

nodes_json="$(curl -sS -H "Authorization: Bearer ${ADMIN_TOKEN}" "${ADMIN_URL}/admin/nodes")"

echo "=== [2/4] Node reachable and healthy ==="
node_status="$(printf '%s' "$nodes_json" | python3 -c "
import json, sys
name = sys.argv[1]
try:
    data = json.load(sys.stdin)
except Exception as e:
    print('PARSE_ERROR', e); sys.exit(1)
nodes = data if isinstance(data, list) else data.get('nodes', data)
for n in nodes:
    if n.get('name') == name:
        print('FOUND', n.get('status', n.get('healthy', 'unknown')))
        sys.exit(0)
print('NOT_FOUND')
" "${NODE_NAME}")"

if [[ "$node_status" == NOT_FOUND* ]]; then
  fail "node '${NODE_NAME}' not found in GET /admin/nodes - check NODE_NAME matches exactly (case-sensitive)"
elif [[ "$node_status" == PARSE_ERROR* ]]; then
  fail "could not parse /admin/nodes response: ${node_status}"
fi
node_state="${node_status#FOUND }"
case "$node_state" in
  healthy|true|up|True) ok "node '${NODE_NAME}' is healthy" ;;
  *) fail "node '${NODE_NAME}' status is '${node_state}', not healthy - fix node connectivity before benchmarking" ;;
esac

echo "=== [3/4] Model pulled on node ==="
model_check="$(printf '%s' "$nodes_json" | python3 -c "
import json, sys
name = sys.argv[1]
model = sys.argv[2]
data = json.load(sys.stdin)
nodes = data if isinstance(data, list) else data.get('nodes', data)
for n in nodes:
    if n.get('name') == name:
        loaded = n.get('loadedModels') or []
        loaded_names = [m.get('name', m) if isinstance(m, dict) else m for m in loaded]
        if model in loaded_names:
            print('LOADED')
        else:
            print('NOT_LOADED', ','.join(str(x) for x in loaded_names))
        sys.exit(0)
print('NOT_FOUND')
" "${NODE_NAME}" "${MODEL}")"

if [[ "$model_check" == LOADED* ]]; then
  ok "model '${MODEL}' currently loaded on '${NODE_NAME}'"
else
  echo "  WARN: model '${MODEL}' not currently resident (warm) on '${NODE_NAME}' (${model_check})."
  echo "  Not necessarily fatal - it may just not be warm yet. Checking whether it's at least"
  echo "  pulled, via the mesh's own GET /admin/nodes/${NODE_NAME}/models (Node Agent"
  echo "  'models.list' capability - runtime-agnostic across Ollama/vLLM/TGI/llama.cpp/MLX,"
  echo "  unlike hitting the node's raw API directly)..."

  models_resp="$(curl -sS -o /tmp/preflight_models.$$ -w '%{http_code}' \
    -H "Authorization: Bearer ${ADMIN_TOKEN}" \
    "${ADMIN_URL}/admin/nodes/${NODE_NAME}/models" 2>/dev/null)"
  models_body="$(cat /tmp/preflight_models.$$ 2>/dev/null)"
  rm -f /tmp/preflight_models.$$

  if [ "$models_resp" = "200" ]; then
    if printf '%s' "$models_body" | python3 -c "
import json, sys
model = sys.argv[1]
data = json.load(sys.stdin)
names = [m.get('name') for m in data.get('models', [])]
sys.exit(0 if model in names else 1)
" "${MODEL}"; then
      ok "model '${MODEL}' confirmed pulled on node (not yet warm) via /admin/nodes/${NODE_NAME}/models"
    else
      fail "model '${MODEL}' is not pulled on node '${NODE_NAME}' (per /admin/nodes/${NODE_NAME}/models) - pull it first"
    fi
  elif [ "$models_resp" = "501" ]; then
    echo "  SKIPPED: node '${NODE_NAME}' has no Node Agent 'models.list' capability enabled"
    echo "  (HTTP 501), so this can't be checked automatically for this runtime/setup."
    echo "  Manually confirm the model is pulled on the node before proceeding (e.g. for an"
    echo "  Ollama node: curl http://<node-ip>:11434/api/tags; for vLLM/TGI/llama.cpp/MLX,"
    echo "  check however that runtime reports its local model cache)."
  else
    fail "GET ${ADMIN_URL}/admin/nodes/${NODE_NAME}/models returned HTTP ${models_resp} - check the mesh and node are up"
  fi
fi

echo "=== [4/4] Model fits under 80% of node VRAM (Step 1's rule) ==="
if [ -z "${MODEL_SIZE_GB:-}" ] || [ -z "${NODE_VRAM_GB:-}" ]; then
  echo "  SKIPPED: set MODEL_SIZE_GB and NODE_VRAM_GB env vars to enforce this check automatically."
  echo "  Manual reminder (BENCH-RUNBOOK.md Step 1): model's on-disk size + ~20% runtime overhead"
  echo "  must be under 80% of the node's total VRAM. Get total VRAM via:"
  echo "    nvidia-smi --query-gpu=memory.total --format=csv,noheader"
  echo "  (or the node's declared vram_total_mb in config.yaml for a Mac mini / Apple Silicon node)."
else
  python3 -c "
import sys
size_gb = float(sys.argv[1])
vram_gb = float(sys.argv[2])
overhead = size_gb * 1.2
pct = overhead / vram_gb * 100
if pct >= 80:
    print(f'FAIL model footprint with ~20% overhead ({overhead:.1f}GB) is {pct:.0f}% of {vram_gb:.1f}GB VRAM - must be under 80%')
    sys.exit(1)
print(f'OK model footprint with ~20% overhead ({overhead:.1f}GB) is {pct:.0f}% of {vram_gb:.1f}GB VRAM')
" "${MODEL_SIZE_GB}" "${NODE_VRAM_GB}" || fail "model does not fit safely under 80% VRAM - pick a smaller quantization (Step 1)"
  ok "model fits safely under 80% VRAM"
fi

echo ""
echo "=== Preflight passed - safe to proceed with BENCH-RUNBOOK.md Step 2 ==="
