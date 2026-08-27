#!/usr/bin/env sh
# marbor installer
# Downloads the latest release binary from GitHub for your OS and architecture.
# Usage: curl -fsSL https://raw.githubusercontent.com/Anirudhx7/marbor/main/install.sh | sh
#
# There is no config.yaml anymore - marbor is DB-first (marbor.db). When
# starting the daemon for the first time, this installer scans the local
# subnet for running GPU nodes (Ollama, vLLM, TGI, llama.cpp) and lets you
# pick which ones to seed into marbor.db interactively. Everything else (API
# keys, ports, routing, HA, webhooks, ...) is configured from the dashboard
# after boot - log in with admin/admin and set a new password.
#
# Modes (opt in via env vars):
#   START=1    start marbor in the background (nohup) after install
#   SERVICE=1  install+enable a proper OS service instead of nohup (implies
#              START=1); persists across reboots and restarts on failure.
#              Currently implemented via systemd on Linux (root/sudo
#              required); on macOS or any host without systemd it falls back
#              to a plain background process rather than failing the install.
#              Recommended for production. Example: curl ... | SERVICE=1 sh
#   FORCE_PROBE=1  re-run the network discovery wizard even if marbor.db
#              already exists (by default it only runs on a fresh DB).
#
#   ROLE=agent  install the marbor agent: downloads the dedicated
#              marbor-agent binary (a separate artifact from the
#              control-plane marbor binary - a GPU host running this
#              role never has a control-plane-capable executable on disk),
#              then registers+starts it as a persistent OS service (systemd
#              on Linux, launchd on macOS - see install.ps1 for Windows) via
#              "marbor-agent service install", the binary's own
#              self-registration subcommand (internal/marboragent/service - no
#              separate service-file logic duplicated in this script for the
#              agent role). Credentials:
#                MARBOR_ENROLL=<code> MARBOR_SERVER=<url>  (default, shown by the
#                  marbor admin UI's "marbor agent" panel) - a short-lived,
#                  single-use code the binary itself exchanges for the real
#                  token by calling back to MARBOR_SERVER, so the real
#                  permanent bearer token never appears in this command / your
#                  shell history (P50). It is read from the MARBOR_AGENT_SECRET
#                  environment variable at agent startup (no legacy TOKEN env).
#                MARBOR_AGENT_SECRET=<token>  (manual path) - the real permanent
#                  token directly, no exchange, no MARBOR_SERVER needed.
#              One of MARBOR_AGENT_SECRET or MARBOR_ENROLL+MARBOR_SERVER is
#              required - there is no existing-installation upgrade path (no
#              prior Marbor deployments exist to preserve).
#              PORT=<port> optionally overrides the default (9200).
#              Example: curl ... | ROLE=agent MARBOR_SERVER=http://marbor-host:8080 MARBOR_ENROLL=xxxxx PORT=9200 sh
#
# Uninstall: https://raw.githubusercontent.com/Anirudhx7/marbor/main/uninstall.sh
# Uninstall a marbor agent: marbor-agent service uninstall (on the node, not this script)

set -e

REPO="Anirudhx7/marbor"
INSTALL_DIR="${INSTALL_DIR:-/usr/local/bin}"
ROLE="${ROLE:-marbor}"
if [ "$ROLE" = "agent" ]; then
  BIN_NAME="marbor-agent"
else
  BIN_NAME="marbor"
fi

START_DAEMON=false
if [ "$START" = "1" ]; then
  START_DAEMON=true
fi
for arg in "$@"; do
  if [ "$arg" = "--start" ] || [ "$arg" = "-s" ]; then
    START_DAEMON=true
  fi
done

FORCE_PROBE=false
if [ "$FORCE_PROBE" = "1" ]; then
  FORCE_PROBE=true
fi
for arg in "$@"; do
  if [ "$arg" = "--force-probe" ]; then
    FORCE_PROBE=true
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

echo "Downloading marbor for ${OS}/${ARCH}..."
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

# Verify the downloaded binary against goreleaser's published checksums.txt
# before chmod+x/execution (this is the primary Linux/macOS install path, run
# via `curl | sh`). checksums.txt is fetched from the same release tag as the
# binary itself, so this also catches a release published mid-install.
CHECKSUMS_URL="https://github.com/${REPO}/releases/latest/download/checksums.txt"
CHECKSUMS_TMP="$(mktemp)"
CHECKSUMS_OK=true
if command -v curl > /dev/null 2>&1; then
  curl -fsSL "$CHECKSUMS_URL" -o "$CHECKSUMS_TMP" || CHECKSUMS_OK=false
elif command -v wget > /dev/null 2>&1; then
  wget -qO "$CHECKSUMS_TMP" "$CHECKSUMS_URL" || CHECKSUMS_OK=false
else
  echo "Error: curl or wget required"
  CHECKSUMS_OK=false
fi

if [ "$CHECKSUMS_OK" = false ] || [ ! -s "$CHECKSUMS_TMP" ]; then
  echo "Error: failed to download checksums.txt for verification"
  rm -f "$TMP" "$CHECKSUMS_TMP"
  exit 1
fi

EXPECTED_HASH="$(grep -E "  \*?${BINARY}\$" "$CHECKSUMS_TMP" | awk '{print $1}')"
if [ -z "$EXPECTED_HASH" ]; then
  echo "Error: checksums.txt has no entry for $BINARY - refusing to run an unverified binary"
  rm -f "$TMP" "$CHECKSUMS_TMP"
  exit 1
fi

if command -v sha256sum > /dev/null 2>&1; then
  ACTUAL_HASH="$(sha256sum "$TMP" | awk '{print $1}')"
elif command -v shasum > /dev/null 2>&1; then
  ACTUAL_HASH="$(shasum -a 256 "$TMP" | awk '{print $1}')"
else
  echo "Error: neither sha256sum nor shasum is available to verify the download"
  rm -f "$TMP" "$CHECKSUMS_TMP"
  exit 1
fi
rm -f "$CHECKSUMS_TMP"

if [ "$ACTUAL_HASH" != "$EXPECTED_HASH" ]; then
  echo "Error: checksum mismatch for $BINARY"
  echo "  expected: $EXPECTED_HASH"
  echo "  actual:   $ACTUAL_HASH"
  echo "Refusing to run this binary."
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
  echo "Upgraded marbor: $OLD_VERSION -> $NEW_VERSION ($BIN_PATH)"
elif [ -n "$OLD_VERSION" ]; then
  echo "Reinstalled marbor $NEW_VERSION ($BIN_PATH, already up to date)"
else
  echo "Installed marbor $NEW_VERSION to $BIN_PATH"
fi

# Best-effort: install man pages (docs/man/*.1, generated by `make docs` -
# see cmd/gen-docs) from the SAME release tag the binary above was just
# downloaded from - this script has no source checkout to copy them from,
# so they're fetched individually the same way the binary was (goreleaser
# uploads them as extra release assets, .goreleaser.yaml). Never fails the
# overall install: every step here is guarded with "|| true" (the same
# idiom used throughout this script), and the whole function is called in a
# subshell with "|| true" on top so a `set -e` failure inside it can't abort
# the rest of the install.
#
# Which filenames to fetch comes from docs/man/MANIFEST.txt, itself a release
# asset generated by cmd/gen-docs alongside the *.1 files it lists - this is
# the single source of truth for "what man pages exist in this release", so
# install.sh never has to hardcode a list that can silently drift out of sync
# with the generator (see Fix 4 of the P83+ CLI hardening code review).
# MAN_PAGES_FALLBACK below is used ONLY if MANIFEST.txt can't be fetched
# (older release published before this file existed, or a network hiccup) -
# it is a compatibility safety net, not the primary source of the list.
install_man_pages() {
  MAN_DIR="/usr/local/share/man/man1"
  MAN_PAGES_FALLBACK="marbor.1 marbor-models.1 marbor-runtime.1 marbor-nodes.1 marbor-node.1 marbor-key.1 marbor-requests.1"
  MAN_TMP_DIR="$(mktemp -d)" || return 0

  MANIFEST_URL="https://github.com/${REPO}/releases/latest/download/MANIFEST.txt"
  MANIFEST_PATH="$MAN_TMP_DIR/MANIFEST.txt"
  MANIFEST_OK=false
  if command -v curl >/dev/null 2>&1; then
    curl -fsSL "$MANIFEST_URL" -o "$MANIFEST_PATH" >/dev/null 2>&1 && [ -s "$MANIFEST_PATH" ] && MANIFEST_OK=true
  elif command -v wget >/dev/null 2>&1; then
    wget -q "$MANIFEST_URL" -O "$MANIFEST_PATH" >/dev/null 2>&1 && [ -s "$MANIFEST_PATH" ] && MANIFEST_OK=true
  fi

  if [ "$MANIFEST_OK" = true ]; then
    MAN_PAGES="$(tr '\n' ' ' < "$MANIFEST_PATH")"
  else
    MAN_PAGES="$MAN_PAGES_FALLBACK"
  fi

  FETCHED_ANY=false
  INSTALLED_PAGES=""
  for page in $MAN_PAGES; do
    PAGE_URL="https://github.com/${REPO}/releases/latest/download/${page}"
    if command -v curl >/dev/null 2>&1; then
      curl -fsSL "$PAGE_URL" -o "$MAN_TMP_DIR/$page" >/dev/null 2>&1 && FETCHED_ANY=true && INSTALLED_PAGES="$INSTALLED_PAGES $page"
    elif command -v wget >/dev/null 2>&1; then
      wget -q "$PAGE_URL" -O "$MAN_TMP_DIR/$page" >/dev/null 2>&1 && FETCHED_ANY=true && INSTALLED_PAGES="$INSTALLED_PAGES $page"
    fi
  done

  if [ "$FETCHED_ANY" = true ]; then
    if [ -w "$MAN_DIR" ] || { [ ! -d "$MAN_DIR" ] && mkdir -p "$MAN_DIR" 2>/dev/null; }; then
      cp "$MAN_TMP_DIR"/*.1 "$MAN_DIR/" 2>/dev/null && echo "Installed man pages to $MAN_DIR (try: man marbor)"
      # Record exactly which filenames THIS install placed in MAN_DIR, so
      # uninstall.sh can remove exactly those files later instead of a broad
      # wildcard (Fix 5 of the code review) - this is the only place that
      # actually knows the real list; uninstall.sh has no source tree and no
      # manifest of its own to consult.
      printf '%s\n' $INSTALLED_PAGES > "$MAN_DIR/.marbor-installed-manifest" 2>/dev/null || true
    elif command -v sudo >/dev/null 2>&1; then
      sudo mkdir -p "$MAN_DIR" 2>/dev/null
      sudo cp "$MAN_TMP_DIR"/*.1 "$MAN_DIR/" 2>/dev/null && echo "Installed man pages to $MAN_DIR (try: man marbor)"
      printf '%s\n' $INSTALLED_PAGES | sudo tee "$MAN_DIR/.marbor-installed-manifest" >/dev/null 2>&1 || true
    fi
    if command -v mandb >/dev/null 2>&1; then
      mandb >/dev/null 2>&1 || sudo mandb >/dev/null 2>&1 || true
    fi
  fi
  rm -rf "$MAN_TMP_DIR"
  return 0
}
if [ "$ROLE" != "agent" ]; then
  ( install_man_pages ) || echo "  [!] Skipped man page install (non-fatal - 'man marbor' won't work, everything else does)."
fi

# marbor agent role: register+start as a persistent OS service and stop here -
# none of the control-plane logic below (node discovery wizard, marbor's own
# systemd unit, dashboard) applies to a node running only the agent. The
# binary's own "service install" subcommand owns the actual
# systemd/launchd registration (internal/marboragent/service) - this script's
# job for this role is just "download the right binary, then hand off to it."
if [ "$ROLE" = "agent" ]; then
  if [ -z "$MARBOR_AGENT_SECRET" ] && [ -z "$MARBOR_ENROLL" ]; then
    echo ""
    echo "Error: ROLE=agent requires MARBOR_AGENT_SECRET=<token> or MARBOR_ENROLL=<code> MARBOR_SERVER=<url>."
    echo "  Generate one from the marbor admin UI: GPU Nodes -> (a node) -> marbor agent -> Enable Agent."
    exit 1
  fi
  if [ -n "$MARBOR_ENROLL" ] && [ -z "$MARBOR_AGENT_SECRET" ] && [ -z "$MARBOR_SERVER" ]; then
    echo ""
    echo "Error: MARBOR_ENROLL=<code> requires MARBOR_SERVER=<url> (the marbor admin dashboard's address)."
    exit 1
  fi
  AGENT_PORT="${PORT:-9200}"

  echo ""
  echo "Installing marbor agent as a persistent service (port $AGENT_PORT)..."
  # set -- preserves per-arg quoting (unlike an unquoted variable expansion,
  # which would word-split/glob a code, token, or URL containing whitespace
  # or shell metacharacters) - "$@" below expands each positional param as
  # its own word. MARBOR_AGENT_SECRET is deliberately NOT passed as a CLI
  # argument here (that would put the real bearer token in this process's
  # argv, visible via `ps`/Task Manager for the life of the install) - it's
  # already in this shell's environment (MARBOR_AGENT_SECRET=... sh), and the
  # binary's own "service install" subcommand already reads MARBOR_AGENT_SECRET
  # from its environment, so it's just inherited below.
  if [ -n "$MARBOR_AGENT_SECRET" ]; then
    set -- service install --port="$AGENT_PORT"
  else
    set -- service install --port="$AGENT_PORT" --enroll="$MARBOR_ENROLL" --server="$MARBOR_SERVER"
  fi
  if [ "$(id -u)" = "0" ]; then
    "$BIN_PATH" "$@"
  elif command -v sudo >/dev/null 2>&1; then
    # -E forwards this shell's environment (incl. MARBOR_AGENT_SECRET) to the
    # sudo'd process instead of resetting it - required for the
    # MARBOR_AGENT_SECRET env-var value above to actually reach the binary
    # under sudo.
    sudo -E "$BIN_PATH" "$@"
  else
    echo "Error: installing the marbor agent service requires root, and sudo is not available."
    exit 1
  fi

  echo ""
  echo "marbor agent installed and running - the marbor server will start polling it on its next poll cycle."
  echo "  Status:    marbor-agent service status"
  echo "  Uninstall: marbor-agent service uninstall"
  exit 0
fi

if [ "$START_DAEMON" = false ]; then
  echo ""
  echo "marbor successfully installed to $BIN_PATH"
  echo "Run: marbor"
  echo "Once it's running, check it with: marbor status  (or marbor --help for all commands)"
  echo "Docs: https://github.com/$REPO"
  echo "Uninstall: https://raw.githubusercontent.com/$REPO/main/uninstall.sh"
  exit 0
fi

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
    # marbor's own /health returns {"proxy_port":...}; a real TGI or
    # llama.cpp server never does. Rule this out FIRST, before the /info
    # probe below - the marbor's embedded dashboard SPA answers 200 on any
    # unmatched path (including /info), so checking status code alone would
    # misidentify a marbor instance (possibly this one) as a TGI node.
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
    # ruled out being marbor above), not just any non-empty body.
    case "$HEALTH_BODY" in
      *'"status"'*) echo "$IP:8080:llamacpp" ;;
    esac
  fi
}

# Scan local subnets for running LLM nodes (Ollama :11434, vLLM :8000,
# TGI/llama.cpp :8080) and print each hit as it's found. Results land in
# $TEMP_FOUND (one "ip:port:runtime" line per hit, deduped by the caller).
probe_network() {
  # Informational only - must go to stderr, not stdout: the caller captures
  # this function's stdout wholesale (FOUND_IPS=$(probe_network)) and
  # word-splits/cuts it expecting only "ip:port:runtime" data lines. A banner
  # on stdout becomes bogus numbered entries in the node picker, corrupting
  # the real discovered-node list and its indices.
  echo "Scanning local subnet for active GPU nodes (Ollama, vLLM, TGI, llama.cpp)..." >&2
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

  sort -u "$TEMP_FOUND" 2>/dev/null || cat "$TEMP_FOUND" | sort -u
  rm -f "$TEMP_FOUND"
}

# Install a systemd unit for marbor so it persists across restarts/
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
  if [ -f marbor.pid ]; then
    OLD_PID=$(cat marbor.pid 2>/dev/null || true)
    if [ -n "$OLD_PID" ] && kill -0 "$OLD_PID" 2>/dev/null; then
      echo "  Stopping existing background (nohup) instance (PID $OLD_PID)..."
      kill "$OLD_PID" 2>/dev/null || true
      sleep 1
    fi
    rm -f marbor.pid
  fi

  # Anything else already bound to the admin port (unmanaged process, another
  # tool) would still make the new unit fail to bind on restart - warn.
  if curl -fs -m 0.5 "http://localhost:8080/health" >/dev/null 2>&1; then
    echo "  [!] Something is already listening on :8080 (possibly an old"
    echo "      hand-started marbor process). Stop it first, or the"
    echo "      systemd service may fail to bind."
  fi

  UNIT_PATH="/etc/systemd/system/marbor.service"
  WORKDIR="$(pwd)"
  RUN_USER="${SERVICE_USER:-$(id -un)}"
  BIN_PATH="$INSTALL_DIR/$BIN_NAME"

  UNIT_CONTENT="[Unit]
Description=marbor
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=${RUN_USER}
WorkingDirectory="${WORKDIR}"
ExecStart="${BIN_PATH}" --db "${DB_PATH}"
Restart=on-failure
RestartSec=2
StandardOutput=append:"${WORKDIR}/marbor.log"
StandardError=append:"${WORKDIR}/marbor.log"

[Install]
WantedBy=multi-user.target
"

  if [ "$(id -u)" = "0" ]; then
    printf '%s' "$UNIT_CONTENT" > "$UNIT_PATH"
    if [ "$RUN_USER" != "root" ]; then
      chown "$RUN_USER" "$DB_PATH" 2>/dev/null || true
    fi
    systemctl daemon-reload
    systemctl enable marbor >/dev/null 2>&1
    systemctl restart marbor
  elif command -v sudo >/dev/null 2>&1; then
    printf '%s' "$UNIT_CONTENT" | sudo tee "$UNIT_PATH" >/dev/null
    sudo systemctl daemon-reload
    sudo systemctl enable marbor >/dev/null 2>&1
    sudo systemctl restart marbor
  else
    echo "  [!] Writing $UNIT_PATH requires root, and sudo is not available."
    return 1
  fi

  sleep 1
  if systemctl is-active --quiet marbor 2>/dev/null; then
    echo "marbor installed as a systemd service and running!"
    echo "--------------------------------------------------------"
    echo "  Proxy Endpoint:   http://localhost:11434"
    echo "  Admin Dashboard:  http://localhost:8080  (login: admin / admin)"
    echo "  Metrics:          http://localhost:9090/metrics"
    echo "  Unit file:        $UNIT_PATH"
    echo "  Logs:             journalctl -u marbor -f  (also ${WORKDIR}/marbor.log)"
    echo "  Database:         ${DB_PATH}"
    echo "--------------------------------------------------------"
    echo "Enabled - will restart on failure and on reboot."
    echo "Check status any time with: marbor status  (or marbor --help for all commands)"
    echo "Uninstall:        https://raw.githubusercontent.com/$REPO/main/uninstall.sh"
    return 0
  else
    echo "  [!] marbor.service failed to start. Check: journalctl -u marbor -n 50"
    # A crash-looping unit reports "activating," not "active," one second
    # after restart - the check above alone doesn't distinguish that from a
    # real failure, so this branch is also reached with the unit installed
    # and enabled but still retrying in the background. Without disabling it
    # here, the caller's nohup fallback below would start a second instance
    # of the same binary against the same ports/DB while the enabled unit
    # keeps fighting it for those resources.
    if [ "$(id -u)" = "0" ]; then
      systemctl disable --now marbor >/dev/null 2>&1 || true
    elif command -v sudo >/dev/null 2>&1; then
      sudo systemctl disable --now marbor >/dev/null 2>&1 || true
    fi
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

# Real post-install verification: the three listeners the binary starts.
# Never fails the install - this is diagnostics for the operator. Ports
# reflect marbor's built-in defaults (11434/8080/9090); if you changed
# them from the dashboard after a previous run, check there instead.
run_health_checks() {
  LOG_HINT="$1"
  HEALTH_OK=true

  echo ""
  echo "Verifying installation..."

  wait_for_http "http://localhost:11434/" "Proxy" || HEALTH_OK=false
  wait_for_http "http://localhost:8080/health" "Admin dashboard" || HEALTH_OK=false
  wait_for_http "http://localhost:9090/metrics" "Metrics" || HEALTH_OK=false

  if [ "$HEALTH_OK" = false ]; then
    echo ""
    echo "  [!] One or more core health checks failed."
    echo "      Troubleshooting:"
    echo "        - Logs:            $LOG_HINT"
    echo "        - Port conflicts:  confirm nothing else is bound to 11434, 8080, or 9090"
  fi
}

DB_PATH="${MARBOR_DB_PATH:-$(pwd)/marbor.db}"

# Node discovery + seeding wizard: only runs when we're about to start the
# daemon against a fresh database (marbor.db doesn't exist yet), so re-running
# the installer against an already-configured host is a no-op here. Nodes
# added another way (dashboard, previous install) are left alone.
if [ ! -f "$DB_PATH" ] || [ "$FORCE_PROBE" = true ]; then
  FOUND_IPS=$(probe_network)

  if [ -z "$FOUND_IPS" ]; then
    echo "  [!] No active LLM nodes found on the local subnet."
    echo "      Add one later from the dashboard (Nodes tab) or re-run with FORCE_PROBE=1."
  else
    echo ""
    echo "Found:"
    N=0
    # POSIX sh has no arrays; index the matches with a parallel-numbered
    # temp file so the selection step below can map "2" back to its line
    # without relying on bash-only constructs.
    LIST_FILE="$(mktemp)"
    for entry in $FOUND_IPS; do
      N=$((N+1))
      echo "$entry" >> "$LIST_FILE"
      IP=$(echo "$entry" | cut -d: -f1)
      PORT=$(echo "$entry" | cut -d: -f2)
      RUNTIME=$(echo "$entry" | cut -d: -f3)
      echo "  $N) $RUNTIME at $IP:$PORT"
    done

    SELECTION=""
    if [ -r /dev/tty ]; then
      printf "Which do you want to add? (comma-separated numbers, 'all', or 'skip'): "
      read -r SELECTION < /dev/tty || SELECTION="skip"
    else
      echo "  [!] No interactive terminal available (piped install) - skipping node selection."
      echo "      Re-run with FORCE_PROBE=1 from an interactive shell to pick nodes, or add them from the dashboard."
      SELECTION="skip"
    fi

    case "$SELECTION" in
      ""|skip|SKIP|Skip) : ;;
      all|ALL|All)
        SEED_ARGS=""
        n=0
        while IFS= read -r entry; do
          n=$((n+1))
          IP=$(echo "$entry" | cut -d: -f1)
          PORT=$(echo "$entry" | cut -d: -f2)
          RUNTIME=$(echo "$entry" | cut -d: -f3)
          SEED_ARGS="$SEED_ARGS --seed-node name=discovered-${RUNTIME}-${n},url=http://${IP}:${PORT},runtime=${RUNTIME}"
        done < "$LIST_FILE"
        if [ -n "$SEED_ARGS" ]; then
          # shellcheck disable=SC2086 # intentional word-splitting of repeatable flags
          "$BIN_PATH" --db "$DB_PATH" $SEED_ARGS
        fi
        ;;
      *)
        SEED_ARGS=""
        OLDIFS="$IFS"
        IFS=','
        for idx in $SELECTION; do
          idx=$(echo "$idx" | tr -d ' ')
          [ -z "$idx" ] && continue
          case "$idx" in
            ''|*[!0-9]*) echo "  [!] Skipping invalid selection: $idx"; continue ;;
          esac
          entry=$(sed -n "${idx}p" "$LIST_FILE")
          [ -z "$entry" ] && { echo "  [!] Skipping invalid selection: $idx"; continue; }
          IP=$(echo "$entry" | cut -d: -f1)
          PORT=$(echo "$entry" | cut -d: -f2)
          RUNTIME=$(echo "$entry" | cut -d: -f3)
          SEED_ARGS="$SEED_ARGS --seed-node name=discovered-${RUNTIME}-${idx},url=http://${IP}:${PORT},runtime=${RUNTIME}"
        done
        IFS="$OLDIFS"
        if [ -n "$SEED_ARGS" ]; then
          # shellcheck disable=SC2086 # intentional word-splitting of repeatable flags
          "$BIN_PATH" --db "$DB_PATH" $SEED_ARGS
        else
          echo "  [!] No valid selections - no nodes added."
        fi
        ;;
    esac
    rm -f "$LIST_FILE"
  fi
else
  echo "marbor.db already exists at $DB_PATH - skipping node discovery wizard (re-run with FORCE_PROBE=1 to force it, or add nodes from the dashboard)."
fi

PIDFILE="marbor.pid"

if [ "$SERVICE_MODE" = true ]; then
  echo ""
  SVC_MANAGER=$(detect_service_manager)
  case "$SVC_MANAGER" in
    systemd)
      echo "Setting up marbor as a systemd service..."
      if setup_systemd_service; then
        run_health_checks "journalctl -u marbor -f  (also $(pwd)/marbor.log)"
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
    echo "marbor is already running in the background (PID $EXISTING_PID)."
    echo "Skipping duplicate start. To restart: kill $EXISTING_PID and re-run this installer,"
    echo "or re-run with SERVICE=1 to switch to a managed systemd service."
    echo "Check status any time with: marbor status  (or marbor --help for all commands)"
    if [ -n "$OLD_VERSION" ] && [ "$OLD_VERSION" != "$NEW_VERSION" ]; then
      echo "Note: the binary on disk was just upgraded to $NEW_VERSION, but the running"
      echo "process (PID $EXISTING_PID) is still $OLD_VERSION until it's restarted."
    fi
    run_health_checks "$(pwd)/marbor.log"
    exit 0
  fi
  rm -f "$PIDFILE"
fi

echo ""
echo "Starting marbor in the background..."
nohup "$INSTALL_DIR/$BIN_NAME" --db "$DB_PATH" > marbor.log 2>&1 &
PID=$!
echo "$PID" > "$PIDFILE"

sleep 2

if kill -0 $PID >/dev/null 2>&1; then
  echo "marbor successfully started (PID: $PID)!"
  echo "--------------------------------------------------------"
  echo "  Proxy Endpoint:   http://localhost:11434"
  echo "  Admin Dashboard:  http://localhost:8080  (login: admin / admin)"
  echo "  Metrics:          http://localhost:9090/metrics"
  echo "  Logs:             marbor.log"
  echo "  Database:         ${DB_PATH}"
  echo "--------------------------------------------------------"
  run_health_checks "$(pwd)/marbor.log"
  echo ""
  echo "Check status any time with: marbor status  (or marbor --help for all commands)"
  echo "Uninstall: https://raw.githubusercontent.com/$REPO/main/uninstall.sh"
else
  rm -f "$PIDFILE"
  echo "Error: marbor failed to start. Check marbor.log for details."
  if [ -f marbor.log ]; then
    cat marbor.log
  fi
  exit 1
fi
