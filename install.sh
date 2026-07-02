#!/usr/bin/env sh
# ollama-mesh installer
# Downloads the latest release binary from GitHub for your OS and architecture.
# Usage: curl -fsSL https://raw.githubusercontent.com/Anirudhx7/ollama-mesh/main/install.sh | sh

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

BINARY="${BIN_NAME}-${OS}-${ARCH}"
URL="https://github.com/${REPO}/releases/latest/download/${BINARY}"

echo "Downloading ollama-mesh for ${OS}/${ARCH}..."
echo "  $URL"

# Download to temp file
TMP="$(mktemp)"
if command -v curl > /dev/null 2>&1; then
  curl -fsSL "$URL" -o "$TMP"
elif command -v wget > /dev/null 2>&1; then
  wget -qO "$TMP" "$URL"
else
  echo "Error: curl or wget required"
  exit 1
fi

chmod +x "$TMP"

# Install
if [ -w "$INSTALL_DIR" ]; then
  mv "$TMP" "$INSTALL_DIR/$BIN_NAME"
  echo "Installed to $INSTALL_DIR/$BIN_NAME"
else
  echo "No write permission to $INSTALL_DIR. Trying with sudo..."
  sudo mv "$TMP" "$INSTALL_DIR/$BIN_NAME"
  echo "Installed to $INSTALL_DIR/$BIN_NAME"
fi

if [ "$START_DAEMON" = false ]; then
  echo ""
  echo "ollama-mesh successfully installed to $INSTALL_DIR/$BIN_NAME"
  echo "Run: ollama-mesh"
  echo "Docs: https://github.com/$REPO"
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
    IS_TGI=false
    if curl -fs -m 0.5 "http://$IP:8080/info" >/dev/null 2>&1; then
      IS_TGI=true
    elif wget -T 0.5 -t 1 -qO- "http://$IP:8080/info" >/dev/null 2>&1; then
      IS_TGI=true
    fi
    
    if [ "$IS_TGI" = true ]; then
      echo "$IP:8080:tgi"
    else
      IS_LLAMACPP=false
      if curl -fs -m 0.5 "http://$IP:8080/health" >/dev/null 2>&1; then
        IS_LLAMACPP=true
      elif wget -T 0.5 -t 1 -qO- "http://$IP:8080/health" >/dev/null 2>&1; then
        IS_LLAMACPP=true
      fi
      if [ "$IS_LLAMACPP" = true ]; then
        echo "$IP:8080:llamacpp"
      fi
    fi
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

    TEMP_FOUND="$(mktemp)"
    for ip in $IP_LIST; do
      case "$ip" in
        127.*|172.17.*|172.18.*|172.19.*) continue ;;
      esac
      PREFIX=$(echo "$ip" | cut -d. -f1-3)"."

      i=1
      while [ $i -le 254 ]; do
        (
          TARGET_IP="${PREFIX}${i}"
          verify_endpoint "$TARGET_IP" "11434" >> "$TEMP_FOUND" &
          verify_endpoint "$TARGET_IP" "8000"  >> "$TEMP_FOUND" &
          verify_endpoint "$TARGET_IP" "8080"  >> "$TEMP_FOUND" &
          wait
        ) &
        i=$((i+1))
      done
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
    # No probe — write a minimal default config pointing at localhost
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

echo ""
echo "Starting ollama-mesh in the background..."
nohup "$INSTALL_DIR/$BIN_NAME" > ollama-mesh.log 2>&1 &
PID=$!

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
else
  echo "Error: ollama-mesh failed to start. Check ollama-mesh.log for details."
  if [ -f ollama-mesh.log ]; then
    cat ollama-mesh.log
  fi
  exit 1
fi

