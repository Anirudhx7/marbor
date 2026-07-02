#!/usr/bin/env sh
# ollama-mesh installer
# Downloads the latest release binary from GitHub for your OS and architecture.
# Usage: curl -fsSL https://raw.githubusercontent.com/Anirudhx7/ollama-mesh/main/install.sh | sh

set -e

REPO="Anirudhx7/ollama-mesh"
INSTALL_DIR="${INSTALL_DIR:-/usr/local/bin}"
BIN_NAME="ollama-mesh"

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

# Probe local Ollama
echo "Probing localhost:11434 for Ollama..."
OLLAMA_FOUND=false
if command -v curl > /dev/null 2>&1; then
  if curl -fs http://localhost:11434/api/tags > /dev/null 2>&1; then
    OLLAMA_FOUND=true
  fi
elif command -v wget > /dev/null 2>&1; then
  if wget -qO- http://localhost:11434/api/tags > /dev/null 2>&1; then
    OLLAMA_FOUND=true
  fi
fi

if [ "$OLLAMA_FOUND" = true ]; then
  echo "  [ok] Local Ollama detected at http://localhost:11434"
else
  echo "  [!] No local Ollama detected at http://localhost:11434"
  echo "      ollama-mesh will start with zero nodes configured. You can add nodes to config.yaml later."
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

