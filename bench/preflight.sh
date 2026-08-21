#!/usr/bin/env bash
# preflight.sh - fail loudly BEFORE bench day wastes an hour on a broken setup.
#
# Checks: admin login works, node is reachable and healthy, model is
# pulled on that node, and model's on-disk size is under 80% of the node's
# total VRAM (Step 1's fit rule).
#
# marbor is fully DB-based (marbor.db) - there is no config.yaml. Admin
# auth is session-based (POST /admin/login with an admin-role account's
# username/password, same as the dashboard login), not a static bearer token.
#
# Usage (all env vars optional except MODEL - see below):
#   MODEL=llama3.2:3b-q4_k_m ./bench/preflight.sh
#
# ADMIN_USERNAME/ADMIN_PASSWORD default to admin/admin (the demo-mode and
# most-common first-admin-account default) - override if your account uses
# something else. NODE_NAME is auto-detected when the mesh has exactly one
# node; set it explicitly if you have more than one.
set -uo pipefail

: "${MESH_URL:=http://localhost:11434}"
: "${ADMIN_URL:=http://localhost:8080}"
: "${ADMIN_USERNAME:=admin}"
: "${ADMIN_PASSWORD:=admin}"
: "${MODEL:?MODEL is required (exact tag, e.g. llama3.2:3b-q4_k_m)}"

fail() { echo "PREFLIGHT FAILED: $*" >&2; exit 1; }
ok() { echo "  OK: $*"; }

if ! command -v curl &>/dev/null; then
  fail "curl not found on PATH"
fi
if ! command -v python3 &>/dev/null; then
  fail "python3 not found on PATH (needed to parse JSON responses)"
fi

COOKIEJAR="$(mktemp)"
cleanup() { rm -f "$COOKIEJAR"; }
trap cleanup EXIT

echo "=== [1/5] Admin login ==="
login_status="$(curl -sS -o /dev/null -w '%{http_code}' -c "$COOKIEJAR" \
  -X POST "${ADMIN_URL}/admin/login" \
  -H "Content-Type: application/json" \
  -d "{\"username\":\"${ADMIN_USERNAME}\",\"password\":\"${ADMIN_PASSWORD}\"}" 2>/dev/null)"
if [ "$login_status" != "200" ]; then
  fail "POST ${ADMIN_URL}/admin/login returned HTTP ${login_status} (expected 200) - check ADMIN_USERNAME/ADMIN_PASSWORD and that the account has the admin role"
fi
ok "logged in as '${ADMIN_USERNAME}'"

nodes_resp="$(curl -sS -o /tmp/preflight_nodes.$$ -w '%{http_code}' -b "$COOKIEJAR" "${ADMIN_URL}/admin/nodes" 2>/dev/null)"
nodes_json="$(cat /tmp/preflight_nodes.$$ 2>/dev/null)"
rm -f /tmp/preflight_nodes.$$
if [ "$nodes_resp" != "200" ]; then
  fail "GET ${ADMIN_URL}/admin/nodes returned HTTP ${nodes_resp} after a successful login - session cookie not being sent/accepted?"
fi

if [ -z "${NODE_NAME:-}" ]; then
  node_auto="$(printf '%s' "$nodes_json" | python3 -c "
import json, sys
data = json.load(sys.stdin)
nodes = data if isinstance(data, list) else data.get('nodes', data)
names = [n.get('name') for n in nodes]
if len(names) == 1:
    print('AUTO', names[0])
elif len(names) == 0:
    print('NONE')
else:
    print('MANY', ','.join(str(x) for x in names))
")"
  case "$node_auto" in
    AUTO*)
      NODE_NAME="${node_auto#AUTO }"
      ok "NODE_NAME not set - auto-detected the mesh's only node: '${NODE_NAME}'"
      ;;
    NONE)
      fail "NODE_NAME not set and GET /admin/nodes returned no nodes - add a node first (dashboard's GPU Nodes page or POST /admin/nodes)"
      ;;
    MANY*)
      fail "NODE_NAME not set and the mesh has multiple nodes (${node_auto#MANY }) - set NODE_NAME to the one you want to benchmark"
      ;;
  esac
fi

echo "=== [2/5] Node reachable and healthy ==="
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
        print('FOUND', n.get('health', 'unknown'))
        sys.exit(0)
print('NOT_FOUND')
" "${NODE_NAME}")"

if [[ "$node_status" == NOT_FOUND* ]]; then
  fail "node '${NODE_NAME}' not found in GET /admin/nodes - check NODE_NAME matches exactly (case-sensitive)"
elif [[ "$node_status" == PARSE_ERROR* ]]; then
  fail "could not parse /admin/nodes response: ${node_status}"
fi
node_health="${node_status#FOUND }"
case "$node_health" in
  healthy) ok "node '${NODE_NAME}' is healthy" ;;
  degraded) ok "node '${NODE_NAME}' is degraded (reachable, recent failures) - usable, but expect some jitter" ;;
  *) fail "node '${NODE_NAME}' health is '${node_health}' (down/unknown) - fix node connectivity before benchmarking" ;;
esac

echo "=== [3/5] Model pulled on node ==="
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

  models_resp="$(curl -sS -o /tmp/preflight_models.$$ -w '%{http_code}' -b "$COOKIEJAR" \
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

echo "=== [4/5] Model fits under 80% of node VRAM (Step 1's rule) ==="
if [ -z "${MODEL_SIZE_GB:-}" ] || [ -z "${NODE_VRAM_GB:-}" ]; then
  echo "  SKIPPED: set MODEL_SIZE_GB and NODE_VRAM_GB env vars to enforce this check automatically."
  echo "  Manual reminder (BENCH-RUNBOOK.md Step 1): model's on-disk size + ~20% runtime overhead"
  echo "  must be under 80% of the node's total VRAM. Get total VRAM via:"
  echo "    nvidia-smi --query-gpu=memory.total --format=csv,noheader"
  echo "  (or the node's declared vramTotalMB, visible in GET /admin/nodes, for a Mac mini /"
  echo "  Apple Silicon or other declared-VRAM node - there is no config file to check by hand)."
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

echo "=== [5/5] Preflight passed ==="
echo "Safe to proceed with BENCH-RUNBOOK.md Step 2 (bench/cold-loop.sh)."
