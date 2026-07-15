#!/usr/bin/env sh
# ollama-mesh installer
# Downloads the latest release binary from GitHub for your OS and architecture.
# Usage: curl -fsSL https://raw.githubusercontent.com/Anirudhx7/ollama-mesh/main/install.sh | sh
#
# Modes (opt in via env vars):
#   PROBE=1    scan the local subnet for GPU nodes and write config.yaml
#   START=1    start ollama-mesh in the background (nohup) after install
#   SERVICE=1  install+enable a proper OS service instead of nohup (implies
#              START=1); persists across reboots and restarts on failure.
#              Currently implemented via systemd on Linux (root/sudo
#              required); on macOS or any host without systemd it falls back
#              to a plain background process rather than failing the install.
#              Recommended for production. Example: curl ... | PROBE=1 SERVICE=1 sh
#
# Uninstall: https://raw.githubusercontent.com/Anirudhx7/ollama-mesh/main/uninstall.sh

set -e

REPO="Anirudhx7/ollama-mesh"
INSTALL_DIR="${INSTALL_DIR:-/usr/local/bin}"
BIN_NAME="ollama-mesh"

START_DAEMON=false
if [ "$START" = "1" ]; then
  START_DAEMON=true
fi
for arg in "$@"; do
  if [ "$arg" = "--start" ] || [ "$arg" = "-s" ]; then
    START_DAEMON=true
  fi
done

PROBE_NETWORK=false
if [ "$PROBE" = "1" ]; then
  PROBE_NETWORK=true
fi
for arg in "$@"; do
  if [ "$arg" = "--probe" ] || [ "$arg" = "-p" ]; then
    PROBE_NETWORK=true
  fi
done

SERVICE_MODE=false
if [ "$SERVICE" = "1" ]; then
  SERVICE_MODE=true
fi
for arg in "$@"; do
  if [ "$arg" = "--service" ]; then
    SERVICE_MODE=true
  fi
done
# A persistent service implies it should actually be running.
if [ "$SERVICE_MODE" = true ]; then
  START_DAEMON=true
fi

# Detect OS
OS="$(uname -s)"
case "$OS" in
  Linux)  OS="linux" ;;
  Darwin) OS="darwin" ;;
  *)
    echo "Unsupported OS: $OS"
    echo "Download manually from: https://github.com/$REPO/releases/latest"
    exit 1
    ;;
esac

# Detect architecture
ARCH="$(uname -m)"
case "$ARCH" in
  x86_64|amd64)  ARCH="amd64" ;;
  aarch64|arm64) ARCH="arm64" ;;
  *)
    echo "Unsupported architecture: $ARCH"
    echo "Download manually from: https://github.com/$REPO/releases/latest"
    exit 1
    ;;
esac

# Service-mode abstraction: SERVICE=1 means "run as a proper OS service",
# not "use systemd" specifically. This picks the right backend per OS so
# the same env var stays meaningful as more platforms are supported.
#   linux  -> systemd (implemented)
#   darwin -> launchd (planned, not yet implemented)
#   other  -> no known service manager
# Anything other than "systemd" falls back to a plain background process.
detect_service_manager() {
  case "$OS" in
    linux)
      if command -v systemctl >/dev/null 2>&1; then
        echo "systemd"
        return
      fi
      ;;
    darwin)
      : # launchd support not implemented yet; falls through to "none"
      ;;
  esac
  echo "none"
}

BINARY="${BIN_NAME}-${OS}-${ARCH}"
URL="https://github.com/${REPO}/releases/latest/download/${BINARY}"
BIN_PATH="$INSTALL_DIR/$BIN_NAME"

# Capture the currently-installed version (if any) before we touch anything,
# so upgrades can report old -> new instead of installing silently over it.
OLD_VERSION=""
if [ -x "$BIN_PATH" ]; then
  OLD_VERSION=$("$BIN_PATH" -version 2>/dev/null | awk '{print $2}')
fi

echo "Downloading ollama-mesh for ${OS}/${ARCH}..."
echo "  $URL"

# Download to temp file
TMP="$(mktemp)"
DOWNLOAD_OK=true
if command -v curl > /dev/null 2>&1; then
  curl -fsSL "$URL" -o "$TMP" || DOWNLOAD_OK=false
elif command -v wget > /dev/null 2>&1; then
  wget -qO "$TMP" "$URL" || DOWNLOAD_OK=false
else
  echo "Error: curl or wget required"
  rm -f "$TMP"
  exit 1
fi

if [ "$DOWNLOAD_OK" = false ] || [ ! -s "$TMP" ]; then
  echo ""
  echo "Error: failed to download $URL"
  echo "  This usually means one of:"
  echo "    - no internet connection, or a proxy/firewall is blocking github.com"
  echo "    - no release exists yet for ${OS}/${ARCH}"
  echo "  Check releases manually: https://github.com/$REPO/releases/latest"
  rm -f "$TMP"
  exit 1
fi

chmod +x "$TMP"

# Install
if [ -w "$INSTALL_DIR" ]; then
  mv "$TMP" "$BIN_PATH"
else
  echo "No write permission to $INSTALL_DIR. Trying with sudo..."
  sudo mv "$TMP" "$BIN_PATH"
fi

NEW_VERSION=$("$BIN_PATH" -version 2>/dev/null | awk '{print $2}')
if [ -n "$OLD_VERSION" ] && [ "$OLD_VERSION" != "$NEW_VERSION" ]; then
  echo "Upgraded ollama-mesh: $OLD_VERSION -> $NEW_VERSION ($BIN_PATH)"
elif [ -n "$OLD_VERSION" ]; then
  echo "Reinstalled ollama-mesh $NEW_VERSION ($BIN_PATH, already up to date)"
else
  echo "Installed ollama-mesh $NEW_VERSION to $BIN_PATH"
fi

if [ "$START_DAEMON" = false ]; then
  echo ""
  echo "ollama-mesh successfully installed to $BIN_PATH"
  echo "Run: ollama-mesh"
  echo "Docs: https://github.com/$REPO"
  echo "Uninstall: https://raw.githubusercontent.com/$REPO/main/uninstall.sh"
  exit 0
fi

# Helper to generate random hex strings
generate_hex() {
  BYTES=$1
  if command -v openssl >/dev/null 2>&1; then
    openssl rand -hex "$BYTES"
  elif command -v python3 >/dev/null 2>&1; then
    python3 -c "import secrets; print(secrets.token_hex($BYTES))"
  elif command -v python >/dev/null 2>&1; then
    python -c "import secrets; print(secrets.token_hex($BYTES))" 2>/dev/null || python -c "import os; print(os.urandom($BYTES).hex())"
  elif [ -r /dev/urandom ]; then
    if command -v od >/dev/null 2>&1; then
      dd if=/dev/urandom bs=1 count="$BYTES" 2>/dev/null | od -An -vtx1 | tr -d ' \n'
    elif command -v hexdump >/dev/null 2>&1; then
      dd if=/dev/urandom bs=1 count="$BYTES" 2>/dev/null | hexdump -e '32/1 "%02x"'
    else
      echo "$(date +%s)$(date +%s)"
    fi
  else
    echo "$(date +%s)$(date +%s)"
  fi
}

get_primary_ip() {
  if command -v ip >/dev/null 2>&1; then
    ip route get 1.1.1.1 2>/dev/null | grep -oE 'src [0-9.]+' | cut -d' ' -f2
  elif command -v route >/dev/null 2>&1; then
    IFACE=$(route -n get default 2>/dev/null | grep interface | awk '{print $2}')
    if [ -n "$IFACE" ] && command -v ipconfig >/dev/null 2>&1; then
      ipconfig getifaddr "$IFACE"
    fi
  fi
}

get_local_subnets() {
  if command -v ip >/dev/null 2>&1; then
    ip -o -4 addr list | awk '{print $4}' | cut -d/ -f1 | grep -v '^127\.'
  elif command -v ifconfig >/dev/null 2>&1; then
    ifconfig | grep -Eo 'inet (addr:)?([0-9]*\.){3}[0-9]*' | grep -v '127.0.0.1' | awk '{print $2}' | sed 's/addr://'
  else
    hostname -I 2>/dev/null
  fi
}

verify_endpoint() {
  IP=$1
  PORT=$2
  
  if [ "$PORT" = "11434" ]; then
    if curl -fs -m 0.5 "http://$IP:11434/api/tags" >/dev/null 2>&1; then
      echo "$IP:11434:ollama"
    elif wget -T 0.5 -t 1 -qO- "http://$IP:11434/api/tags" >/dev/null 2>&1; then
      echo "$IP:11434:ollama"
    fi
  elif [ "$PORT" = "8000" ]; then
    if curl -fs -m 0.5 "http://$IP:8000/v1/models" >/dev/null 2>&1; then
      echo "$IP:8000:vllm"
    elif wget -T 0.5 -t 1 -qO- "http://$IP:8000/v1/models" >/dev/null 2>&1; then
      echo "$IP:8000:vllm"
    fi
  elif [ "$PORT" = "8080" ]; then
    HEALTH_BODY=""
    if command -v curl >/dev/null 2>&1; then
      HEALTH_BODY=$(curl -fs -m 0.5 "http://$IP:8080/health" 2>/dev/null || true)
    else
      HEALTH_BODY=$(wget -T 0.5 -t 1 -qO- "http://$IP:8080/health" 2>/dev/null || true)
    fi
    # ollama-mesh's own /health returns {"proxy_port":...}; a real TGI or
    # llama.cpp server never does. Rule this out FIRST, before the /info
    # probe below - the mesh's embedded dashboard SPA answers 200 on any
    # unmatched path (including /info), so checking status code alone would
    # misidentify a mesh instance (possibly this one) as a TGI node.
    case "$HEALTH_BODY" in
      *proxy_port*) return ;;
    esac

    # TGI: verify by content, not just HTTP status. A real TGI server's
    # /info response is JSON containing both "model_id" and
    # "max_concurrent_requests" - fields no SPA catch-all route would emit.
    INFO_BODY=""
    if command -v curl >/dev/null 2>&1; then
      INFO_BODY=$(curl -fs -m 0.5 "http://$IP:8080/info" 2>/dev/null || true)
    else
      INFO_BODY=$(wget -T 0.5 -t 1 -qO- "http://$IP:8080/info" 2>/dev/null || true)
    fi
    case "$INFO_BODY" in
      *model_id*max_concurrent_requests*|*max_concurrent_requests*model_id*)
        echo "$IP:8080:tgi"
        return
        ;;
    esac

    # llama.cpp server: /health responds with a JSON "status" field (already
    # ruled out being ollama-mesh above), not just any non-empty body.
    case "$HEALTH_BODY" in
      *'"status"'*) echo "$IP:8080:llamacpp" ;;
    esac
  fi
}

# Install a systemd unit for ollama-mesh so it persists across restarts/
# reboots, instead of the plain `nohup` background process used otherwise.
# Requires systemd + root (or sudo). Falls back to the caller starting a
# plain background process if systemd isn't available.
setup_systemd_service() {
  if ! command -v systemctl >/dev/null 2>&1; then
    echo "  [!] systemd (systemctl) not found; SERVICE=1 requires systemd."
    return 1
  fi

  # A pre-existing nohup-managed instance (tracked via pidfile) would fight
  # the new systemd unit for the same ports. Since we're switching this host
  # to service-managed mode, stop it rather than leaving the port conflict
  # for the operator to debug.
  if [ -f ollama-mesh.pid ]; then
    OLD_PID=$(cat ollama-mesh.pid 2>/dev/null || true)
    if [ -n "$OLD_PID" ] && kill -0 "$OLD_PID" 2>/dev/null; then
      echo "  Stopping existing background (nohup) instance (PID $OLD_PID)..."
      kill "$OLD_PID" 2>/dev/null || true
      sleep 1
    fi
    rm -f ollama-mesh.pid
  fi

  # Anything else already bound to the admin port (unmanaged process, another
  # tool) would still make the new unit fail to bind on restart - warn.
  if curl -fs -m 0.5 "http://localhost:8080/health" >/dev/null 2>&1; then
    echo "  [!] Something is already listening on :8080 (possibly an old"
    echo "      hand-started ollama-mesh process). Stop it first, or the"
    echo "      systemd service may fail to bind."
  fi

  UNIT_PATH="/etc/systemd/system/ollama-mesh.service"
  WORKDIR="$(pwd)"
  RUN_USER="${SERVICE_USER:-$(id -un)}"
  BIN_PATH="$INSTALL_DIR/$BIN_NAME"
  CONFIG_PATH="$WORKDIR/config.yaml"

  UNIT_CONTENT="[Unit]
Description=ollama-mesh
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=${RUN_USER}
WorkingDirectory=${WORKDIR}
ExecStart=${BIN_PATH} -config ${CONFIG_PATH}
Restart=on-failure
RestartSec=2
StandardOutput=append:${WORKDIR}/ollama-mesh.log
StandardError=append:${WORKDIR}/ollama-mesh.log

[Install]
WantedBy=multi-user.target
"

  if [ "$(id -u)" = "0" ]; then
    printf '%s' "$UNIT_CONTENT" > "$UNIT_PATH"
    if [ "$RUN_USER" != "root" ]; then
      chown "$RUN_USER" "$CONFIG_PATH" 2>/dev/null || true
    fi
    systemctl daemon-reload
    systemctl enable ollama-mesh >/dev/null 2>&1
    systemctl restart ollama-mesh
  elif command -v sudo >/dev/null 2>&1; then
    printf '%s' "$UNIT_CONTENT" | sudo tee "$UNIT_PATH" >/dev/null
    sudo systemctl daemon-reload
    sudo systemctl enable ollama-mesh >/dev/null 2>&1
    sudo systemctl restart ollama-mesh
  else
    echo "  [!] Writing $UNIT_PATH requires root, and sudo is not available."
    return 1
  fi

  sleep 1
  if systemctl is-active --quiet ollama-mesh 2>/dev/null; then
    echo "ollama-mesh installed as a systemd service and running!"
    echo "--------------------------------------------------------"
    echo "  Proxy Endpoint:   http://localhost:11435"
    echo "  Admin Dashboard:  http://localhost:8080"
    echo "  Metrics:          http://localhost:9090/metrics"
    echo "  Unit file:        $UNIT_PATH"
    echo "  Logs:             journalctl -u ollama-mesh -f  (also ${WORKDIR}/ollama-mesh.log)"
    echo "  Config:           ${CONFIG_PATH}"
    echo "--------------------------------------------------------"
    echo "Enabled - will restart on failure and on reboot."
    echo "Uninstall:        https://raw.githubusercontent.com/$REPO/main/uninstall.sh"
    return 0
  else
    echo "  [!] ollama-mesh.service failed to start. Check: journalctl -u ollama-mesh -n 50"
    return 1
  fi
}

# Poll a URL until it responds (any HTTP status counts as "up" - we're
# checking the port is bound and serving, not asserting a particular route).
# Non-fatal to the caller: prints [ok]/[FAIL] and returns 0/1.
wait_for_http() {
  URL="$1"
  NAME="$2"
  TRIES=8
  i=0
  while [ "$i" -lt "$TRIES" ]; do
    CODE="000"
    if command -v curl >/dev/null 2>&1; then
      # curl itself prints "000" on connection failure and exits non-zero;
      # under `set -e` that non-zero status on a bare assignment would abort
      # the whole installer, so it's neutralized with `|| true`. Don't also
      # append a fallback "000" on top of curl's own output, or a real
      # failure reads as two values concatenated (e.g. "000000"), which no
      # longer matches "000" and would be misreported as reachable.
      CODE=$(curl -s -o /dev/null -w '%{http_code}' -m 2 "$URL" 2>/dev/null) || true
      CODE="${CODE:-000}"
    elif command -v wget >/dev/null 2>&1; then
      wget -T 2 -t 1 -qO- "$URL" >/dev/null 2>&1 && CODE="200"
    fi
    if [ "$CODE" != "000" ]; then
      echo "  [ok]   $NAME responding ($URL -> HTTP $CODE)"
      return 0
    fi
    i=$((i + 1))
    sleep 1
  done
  echo "  [FAIL] $NAME not responding at $URL"
  return 1
}

# Real post-install verification: config validity, the three listeners the
# binary starts, and reachability of whatever backend nodes are configured.
# Never fails the install - this is diagnostics for the operator, since a
# backend node being offline at install time is expected/acceptable.
run_health_checks() {
  CONFIG_FILE="$1"
  LOG_HINT="$2"
  HEALTH_OK=true

  echo ""
  echo "Verifying installation..."

  if [ -f "$CONFIG_FILE" ]; then
    if "$BIN_PATH" -validate -config "$CONFIG_FILE" >/dev/null 2>&1; then
      echo "  [ok]   config.yaml is valid"
    else
      echo "  [FAIL] config.yaml failed validation"
      HEALTH_OK=false
    fi
  fi

  PROXY_PORT=$(awk '/^proxy:/{f=1;next} /^[^ ]/{f=0} f && /^[[:space:]]*port:/{print $2; exit}' "$CONFIG_FILE" 2>/dev/null)
  [ -z "$PROXY_PORT" ] && PROXY_PORT=11434
  wait_for_http "http://localhost:${PROXY_PORT}/" "Proxy" || HEALTH_OK=false
  wait_for_http "http://localhost:8080/health" "Admin dashboard" || HEALTH_OK=false

  METRICS_ENABLED=$(awk '/^metrics:/{f=1;next} /^[^ ]/{f=0} f && /^[[:space:]]*enabled:/{print $2; exit}' "$CONFIG_FILE" 2>/dev/null)
  if [ "$METRICS_ENABLED" != "false" ]; then
    wait_for_http "http://localhost:9090/metrics" "Metrics" || HEALTH_OK=false
  else
    echo "  [skip] Metrics disabled in config.yaml"
  fi

  if [ -f "$CONFIG_FILE" ]; then
    NODE_URLS=$(grep -E '^[[:space:]]*url:' "$CONFIG_FILE" | awk '{print $2}')
    for u in $NODE_URLS; do
      CODE="000"
      if command -v curl >/dev/null 2>&1; then
        CODE=$(curl -s -o /dev/null -w '%{http_code}' -m 2 "$u" 2>/dev/null) || true
        CODE="${CODE:-000}"
      elif command -v wget >/dev/null 2>&1; then
        wget -T 2 -t 1 -qO- "$u" >/dev/null 2>&1 && CODE="200"
      fi
      if [ "$CODE" != "000" ]; then
        echo "  [ok]   backend node reachable: $u"
      else
        echo "  [warn] backend node not reachable: $u (mesh will retry; edit config.yaml if unexpected)"
      fi
    done
  fi

  if [ "$HEALTH_OK" = false ]; then
    echo ""
    echo "  [!] One or more core health checks failed."
    echo "      Troubleshooting:"
    echo "        - Logs:            $LOG_HINT"
    echo "        - Validate config: $BIN_PATH -validate -config $CONFIG_FILE"
    echo "        - Port conflicts:  confirm nothing else is bound to ${PROXY_PORT}, 8080, or 9090"
  fi
}

# Write default config.yaml
if [ ! -f config.yaml ]; then
  ADMIN_TOKEN=$(generate_hex 16)
  API_KEY="sk-mesh-$(generate_hex 32)"

  if [ "$PROBE_NETWORK" = true ]; then
    # Scan local subnets for running LLM nodes (Ollama :11434, vLLM :8000, TGI/llama.cpp :8080)
    echo "Scanning local subnet for active GPU nodes (Ollama, vLLM, TGI, llama.cpp)..."
    PRIMARY_IP=$(get_primary_ip)
    IP_LIST=""
    if [ -n "$PRIMARY_IP" ]; then
      IP_LIST="$PRIMARY_IP"
    else
      IP_LIST=$(get_local_subnets)
    fi

    # Own addresses are probed separately via the "localhost" check below;
    # skipping them here avoids discovering this same host twice under two
    # different names (once by LAN IP, once as "localhost"). get_local_subnets
    # emits one IP per line (any host with >1 interface, e.g. Docker bridges,
    # returns several lines) - join with spaces so the " $TARGET_IP " match
    # below actually matches each entry instead of only the last one.
    SELF_IPS=" $(get_local_subnets | tr '\n' ' ') "

    TEMP_FOUND="$(mktemp)"
    for ip in $IP_LIST; do
      case "$ip" in
        127.*|172.17.*|172.18.*|172.19.*) continue ;;
      esac
      PREFIX=$(echo "$ip" | cut -d. -f1-3)"."

      i=1
      while [ $i -le 254 ]; do
        TARGET_IP="${PREFIX}${i}"
        case "$SELF_IPS" in
          *" $TARGET_IP "*) i=$((i+1)); continue ;;
        esac
        verify_endpoint "$TARGET_IP" "11434" >> "$TEMP_FOUND" &
        verify_endpoint "$TARGET_IP" "8000"  >> "$TEMP_FOUND" &
        verify_endpoint "$TARGET_IP" "8080"  >> "$TEMP_FOUND" &

        if [ $((i % 15)) -eq 0 ]; then
          wait
        fi
        i=$((i+1))
      done
      wait
    done
    wait

    # Also check localhost specifically
    (
      verify_endpoint "localhost" "11434" >> "$TEMP_FOUND" &
      verify_endpoint "localhost" "8000"  >> "$TEMP_FOUND" &
      verify_endpoint "localhost" "8080"  >> "$TEMP_FOUND" &
      wait
    ) &
    wait

    FOUND_IPS=$(sort -u "$TEMP_FOUND" 2>/dev/null || cat "$TEMP_FOUND" | sort -u)
    rm -f "$TEMP_FOUND"

    echo "Writing config.yaml with discovered nodes..."
    cat <<EOF > config.yaml
proxy:
  port: 11435
auth:
  enabled: true
  admin_token: ${ADMIN_TOKEN}
  keys:
    - name: default
      key: ${API_KEY}
      rate_limit: 1000
metrics:
  enabled: true
nodes:
EOF
    if [ -n "$FOUND_IPS" ]; then
      NODE_COUNT=1
      for entry in $FOUND_IPS; do
        IP=$(echo "$entry" | cut -d: -f1)
        PORT=$(echo "$entry" | cut -d: -f2)
        RUNTIME=$(echo "$entry" | cut -d: -f3)
        echo "  - name: discovered-${RUNTIME}-${NODE_COUNT}" >> config.yaml
        echo "    url: http://${IP}:${PORT}" >> config.yaml
        echo "    runtime: ${RUNTIME}" >> config.yaml
        echo "  [ok] Discovered ${RUNTIME} node at http://${IP}:${PORT} (added to config.yaml)"
        NODE_COUNT=$((NODE_COUNT+1))
      done
    else
      cat <<EOF >> config.yaml
  - name: local
    url: http://localhost:11434
EOF
      echo "  [!] No active LLM nodes found on subnet. Defaulted config.yaml to http://localhost:11434."
    fi

  else
    # No probe - write a minimal default config pointing at localhost
    echo "Writing default config.yaml (run with PROBE=1 to auto-discover network nodes)..."
    cat <<EOF > config.yaml
proxy:
  port: 11435
auth:
  enabled: true
  admin_token: ${ADMIN_TOKEN}
  keys:
    - name: default
      key: ${API_KEY}
      rate_limit: 1000
metrics:
  enabled: true
nodes:
  - name: local
    url: http://localhost:11434
EOF
    echo "  Edit config.yaml to add your GPU nodes, then run: ollama-mesh"
  fi
else
  echo "config.yaml already exists, using existing configuration."
fi

PIDFILE="ollama-mesh.pid"

if [ "$SERVICE_MODE" = true ]; then
  echo ""
  SVC_MANAGER=$(detect_service_manager)
  case "$SVC_MANAGER" in
    systemd)
      echo "Setting up ollama-mesh as a systemd service..."
      if setup_systemd_service; then
        run_health_checks config.yaml "journalctl -u ollama-mesh -f  (also $(pwd)/ollama-mesh.log)"
        exit 0
      fi
      ;;
    *)
      if [ "$OS" = "darwin" ]; then
        echo "  [!] launchd service support isn't implemented yet on macOS (SERVICE=1 currently supports systemd on Linux)."
      else
        echo "  [!] No supported service manager found for $OS."
      fi
      ;;
  esac
  echo "  Falling back to a plain background start (not persistent across reboots)."
fi

# Idempotency: if a previous nohup-managed instance is still running, don't
# start a second one competing for the same ports - report it and verify
# health instead. A stale pidfile (process no longer running) is cleaned up
# and a fresh instance is started normally.
if [ -f "$PIDFILE" ]; then
  EXISTING_PID=$(cat "$PIDFILE" 2>/dev/null || true)
  if [ -n "$EXISTING_PID" ] && kill -0 "$EXISTING_PID" 2>/dev/null; then
    echo ""
    echo "ollama-mesh is already running in the background (PID $EXISTING_PID)."
    echo "Skipping duplicate start. To restart: kill $EXISTING_PID and re-run this installer,"
    echo "or re-run with SERVICE=1 to switch to a managed systemd service."
    if [ -n "$OLD_VERSION" ] && [ "$OLD_VERSION" != "$NEW_VERSION" ]; then
      echo "Note: the binary on disk was just upgraded to $NEW_VERSION, but the running"
      echo "process (PID $EXISTING_PID) is still $OLD_VERSION until it's restarted."
    fi
    run_health_checks config.yaml "$(pwd)/ollama-mesh.log"
    exit 0
  fi
  rm -f "$PIDFILE"
fi

echo ""
echo "Starting ollama-mesh in the background..."
nohup "$INSTALL_DIR/$BIN_NAME" > ollama-mesh.log 2>&1 &
PID=$!
echo "$PID" > "$PIDFILE"

sleep 2

if kill -0 $PID >/dev/null 2>&1; then
  echo "ollama-mesh successfully started (PID: $PID)!"
  echo "--------------------------------------------------------"
  echo "  Proxy Endpoint:   http://localhost:11435"
  echo "  Admin Dashboard:  http://localhost:8080"
  echo "  Metrics:          http://localhost:9090/metrics"
  echo "  Logs:             ollama-mesh.log"
  if [ -f config.yaml ]; then
    echo "  Config:           config.yaml"
  fi
  echo "--------------------------------------------------------"
  run_health_checks config.yaml "$(pwd)/ollama-mesh.log"
  echo ""
  echo "Uninstall: https://raw.githubusercontent.com/$REPO/main/uninstall.sh"
else
  rm -f "$PIDFILE"
  echo "Error: ollama-mesh failed to start. Check ollama-mesh.log for details."
  if [ -f ollama-mesh.log ]; then
    cat ollama-mesh.log
  fi
  exit 1
fi

