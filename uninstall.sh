#!/usr/bin/env sh
# ollama-mesh uninstaller
# Usage: sh uninstall.sh   (run from the directory install.sh was run in, so
#                           it can find config.yaml / mesh.db / the pidfile)
#
# Env vars:
#   INSTALL_DIR   binary location to remove (default /usr/local/bin, must
#                 match whatever install.sh used)
#   KEEP_DB=1     keep mesh.db without prompting
#   KEEP_DB=0     remove mesh.db without prompting
#   KEEP_CONFIG=1 keep config.yaml without prompting
#   KEEP_CONFIG=0 remove config.yaml without prompting
#
# When run non-interactively (e.g. piped via curl, where stdin isn't a
# terminal) config.yaml and mesh.db are kept by default unless the env vars
# above say otherwise - an uninstall should never silently destroy data.

set -e

INSTALL_DIR="${INSTALL_DIR:-/usr/local/bin}"
BIN_NAME="ollama-mesh"
BIN_PATH="$INSTALL_DIR/$BIN_NAME"
WORKDIR="$(pwd)"
PIDFILE="$WORKDIR/ollama-mesh.pid"
UNIT_PATH="/etc/systemd/system/ollama-mesh.service"
DB_FILE="$WORKDIR/mesh.db"
CONFIG_FILE="$WORKDIR/config.yaml"
LOG_FILE="$WORKDIR/ollama-mesh.log"

echo "ollama-mesh uninstaller"
echo "------------------------"

# 1. Remove the systemd service, if one was installed.
if [ -f "$UNIT_PATH" ]; then
  if command -v systemctl >/dev/null 2>&1; then
    echo "Removing systemd service ($UNIT_PATH)..."
    if [ "$(id -u)" = "0" ]; then
      systemctl stop ollama-mesh >/dev/null 2>&1 || true
      systemctl disable ollama-mesh >/dev/null 2>&1 || true
      rm -f "$UNIT_PATH"
      systemctl daemon-reload
    elif command -v sudo >/dev/null 2>&1; then
      sudo systemctl stop ollama-mesh >/dev/null 2>&1 || true
      sudo systemctl disable ollama-mesh >/dev/null 2>&1 || true
      sudo rm -f "$UNIT_PATH"
      sudo systemctl daemon-reload
    else
      echo "  [!] Removing $UNIT_PATH requires root, and sudo is not available. Remove it manually."
    fi
  else
    echo "  [!] Found $UNIT_PATH but systemctl is unavailable; remove the unit file manually."
  fi
fi

# 2. Stop a plain background (nohup) instance, if one is tracked.
if [ -f "$PIDFILE" ]; then
  PID=$(cat "$PIDFILE" 2>/dev/null || true)
  if [ -n "$PID" ] && kill -0 "$PID" 2>/dev/null; then
    echo "Stopping background process (PID $PID)..."
    kill "$PID" 2>/dev/null || true
    sleep 1
  fi
  rm -f "$PIDFILE"
fi

# 3. Remove the binary.
if [ -f "$BIN_PATH" ]; then
  if [ -w "$INSTALL_DIR" ]; then
    rm -f "$BIN_PATH"
    echo "Removed binary: $BIN_PATH"
  elif command -v sudo >/dev/null 2>&1; then
    sudo rm -f "$BIN_PATH"
    echo "Removed binary: $BIN_PATH"
  else
    echo "  [!] Cannot remove $BIN_PATH (no write permission, no sudo). Remove it manually."
  fi
else
  echo "No binary found at $BIN_PATH (already removed, or INSTALL_DIR differs from install)."
fi

# 4. mesh.db and config.yaml hold real state (API keys, warm-state history) -
# ask before deleting, and default to keeping them when not on a terminal.
ask_keep() {
  # ask_keep <label> <file> <env override>  -> prints "yes" or "no"
  LABEL="$1"
  FILE="$2"
  OVERRIDE="$3"
  if [ "$OVERRIDE" = "0" ]; then
    echo "no"
    return
  fi
  if [ "$OVERRIDE" = "1" ]; then
    echo "yes"
    return
  fi
  if [ -t 0 ] && [ -f "$FILE" ]; then
    printf "Keep %s (%s)? [Y/n] " "$LABEL" "$FILE" >&2
    read -r ans || ans=""
    case "$ans" in
      [nN]*) echo "no" ;;
      *) echo "yes" ;;
    esac
    return
  fi
  echo "yes"
}

if [ -f "$DB_FILE" ]; then
  if [ "$(ask_keep "the SQLite database" "$DB_FILE" "$KEEP_DB")" = "no" ]; then
    rm -f "$DB_FILE"
    echo "Removed $DB_FILE"
  else
    echo "Kept $DB_FILE"
  fi
fi

if [ -f "$CONFIG_FILE" ]; then
  if [ "$(ask_keep "config.yaml (API keys, node list)" "$CONFIG_FILE" "$KEEP_CONFIG")" = "no" ]; then
    rm -f "$CONFIG_FILE"
    echo "Removed $CONFIG_FILE"
  else
    echo "Kept $CONFIG_FILE"
  fi
fi

if [ -f "$LOG_FILE" ]; then
  rm -f "$LOG_FILE"
  echo "Removed $LOG_FILE"
fi

echo ""
echo "ollama-mesh has been uninstalled."
