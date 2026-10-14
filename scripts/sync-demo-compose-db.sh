#!/usr/bin/env bash
# sync-demo-compose-db.sh - rewrite the inline demo database in
# demo/docker-compose.standalone.yml from demo/marbor.demo.db, so the
# standalone compose file has no second hand-maintained copy. Run by
# `make demo-db`; TestStandaloneComposeDemoDBMatchesFixture fails if skipped.
set -euo pipefail
cd "$(dirname "$0")/.."

compose=demo/docker-compose.standalone.yml
db=demo/marbor.demo.db
tmp="$(mktemp)"
trap 'rm -f "$tmp"' EXIT

awk -v db="$db" '
  /^  demo_marbor_db:$/ { print; getline; print; skip = 1
    cmd = "base64 < " db " | tr -d \"\n\" | fold -w 100"
    while ((cmd | getline line) > 0) print "      " line
    close(cmd); next }
  skip && /^  [A-Za-z_]+:$/ { skip = 0 }
  !skip { print }
' "$compose" > "$tmp"
cat "$tmp" > "$compose"
