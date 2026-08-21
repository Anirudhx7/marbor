#!/usr/bin/env sh
# marbor uninstaller
# Usage: sh uninstall.sh   (run from the directory install.sh was run in, so
#                           it can find marbor.db / the pidfile)
#
# Env vars:
#   INSTALL_DIR   binary location to remove (default /usr/local/bin, must
#                 match whatever install.sh used)
#   KEEP_DB=1     keep marbor.db without prompting
#   KEEP_DB=0     remove marbor.db without prompting
#
# When run non-interactively (e.g. piped via curl, where stdin isn't a
# terminal) marbor.db is kept by default unless the env var above says
# otherwise - an uninstall should never silently destroy data.

set -e

INSTALL_DIR="${INSTALL_DIR:-/usr/local/bin}"
BIN_NAME="marbor"
BIN_PATH="$INSTALL_DIR/$BIN_NAME"
WORKDIR="$(pwd)"
PIDFILE="$WORKDIR/marbor.pid"
UNIT_PATH="/etc/systemd/system/marbor.service"
DB_FILE="$WORKDIR/marbor.db"
LOG_FILE="$WORKDIR/marbor.log"

echo "marbor uninstaller"
echo "------------------------"

# 1. Remove the systemd service, if one was installed.
if [ -f "$UNIT_PATH" ]; then
  if command -v systemctl >/dev/null 2>&1; then
    echo "Removing systemd service ($UNIT_PATH)..."
    if [ "$(id -u)" = "0" ]; then
      systemctl stop marbor >/dev/null 2>&1 || true
      systemctl disable marbor >/dev/null 2>&1 || true
      rm -f "$UNIT_PATH"
      systemctl daemon-reload
    elif command -v sudo >/dev/null 2>&1; then
      sudo systemctl stop marbor >/dev/null 2>&1 || true
      sudo systemctl disable marbor >/dev/null 2>&1 || true
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

# 3b. Remove man pages installed by install.sh (docs/man/*.1, fetched from
# the release into /usr/local/share/man/man1 - see install.sh's
# install_man_pages). Same write-permission/sudo fallback as the binary
# above; a missing man dir or missing pages is not an error, just a no-op.
#
# install_man_pages (Fix 4/5 of the P83+ CLI hardening code review) drops a
# local marker file, .marbor-installed-manifest, listing exactly which
# filenames IT placed in MAN_DIR - uninstall.sh has no source tree and no
# release manifest of its own, so that marker is the only place this exact
# list is ever recorded. Prefer it when present (removes exactly what a
# Fix-4-or-later install created); the wildcard below survives only as a
# fallback for an install that predates this fix and never wrote a marker.
MAN_DIR="/usr/local/share/man/man1"
MAN_MANIFEST="$MAN_DIR/.marbor-installed-manifest"
if [ -f "$MAN_MANIFEST" ]; then
  MAN_PAGES_TO_REMOVE="$(cat "$MAN_MANIFEST" 2>/dev/null || true)"
  if [ -n "$MAN_PAGES_TO_REMOVE" ]; then
    if [ -w "$MAN_DIR" ]; then
      for page in $MAN_PAGES_TO_REMOVE; do
        rm -f "$MAN_DIR/$page"
      done
      rm -f "$MAN_MANIFEST"
      echo "Removed man pages listed in $MAN_MANIFEST"
    elif command -v sudo >/dev/null 2>&1; then
      for page in $MAN_PAGES_TO_REMOVE; do
        sudo rm -f "$MAN_DIR/$page"
      done
      sudo rm -f "$MAN_MANIFEST"
      echo "Removed man pages listed in $MAN_MANIFEST"
    else
      echo "  [!] Cannot remove man pages in $MAN_DIR (no write permission, no sudo). Remove them manually."
    fi
  fi
elif ls "$MAN_DIR"/marbor*.1 >/dev/null 2>&1; then
  if [ -w "$MAN_DIR" ]; then
    rm -f "$MAN_DIR"/marbor*.1
    echo "Removed man pages: $MAN_DIR/marbor*.1"
  elif command -v sudo >/dev/null 2>&1; then
    sudo rm -f "$MAN_DIR"/marbor*.1
    echo "Removed man pages: $MAN_DIR/marbor*.1"
  else
    echo "  [!] Cannot remove man pages in $MAN_DIR (no write permission, no sudo). Remove them manually."
  fi
fi

# 4. marbor.db holds real state (nodes, API keys, warm-state history) - ask
# before deleting, and default to keeping it when not on a terminal.
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
    rm -f "$DB_FILE" "$DB_FILE.key"
    echo "Removed $DB_FILE"
  else
    echo "Kept $DB_FILE"
  fi
fi

if [ -f "$LOG_FILE" ]; then
  rm -f "$LOG_FILE"
  echo "Removed $LOG_FILE"
fi

echo ""
echo "marbor has been uninstalled."
