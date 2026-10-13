#!/usr/bin/env bash
# release-check.sh - verifies what a release WOULD publish, before it is published.
#
# Run it after `goreleaser release --snapshot --clean` has filled dist/ (and,
# for the image checks, built the snapshot Docker images). The release workflow
# runs it as a gate in front of the real release, and CI runs it on pull
# requests that touch release packaging. It checks the built artifacts, not the
# source, so a change that compiles and passes unit tests but would ship a
# broken release still fails here.
#
#   1. The asset set is exactly what we intend to publish (10 binaries, the
#      man-page archive and checksums.txt - no stray loose files).
#   2. Every checksum in checksums.txt matches the file it names.
#   3. The man-page archive holds exactly docs/man/*.1 under man1/.
#   4. install.sh's man-page step, run against the REAL archive and checksums,
#      installs every page and records them for uninstall.sh (and the pages
#      render with man, when man is installed).
#   5. The release Docker images exist for both architectures, the amd64 one
#      reports the snapshot version and a started container answers /health.
#
# Set RELEASE_CHECK_SKIP_DOCKER=1 to skip step 5 (local runs without Docker).
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/.."

fail() { echo ""; echo "RELEASE CHECK FAILED: $*" >&2; exit 1; }
ok() { echo "  ok: $*"; }

PY=""
for candidate in python3 python; do
  if command -v "$candidate" >/dev/null 2>&1 && "$candidate" -c "import sys" >/dev/null 2>&1; then PY="$candidate"; break; fi
done
[ -n "$PY" ] || fail "python3 is required"
[ -f dist/metadata.json ] && [ -f dist/checksums.txt ] && [ -f dist/artifacts.json ] \
  || fail "dist/ is missing - run 'goreleaser release --snapshot --clean' first"

WORK="$(mktemp -d)"
server_pid=""
container=""
cleanup() {
  if [ -n "$server_pid" ]; then kill "$server_pid" >/dev/null 2>&1 || true; wait "$server_pid" 2>/dev/null || true; fi
  [ -z "$container" ] || docker rm -f "$container" >/dev/null 2>&1 || true
  rm -rf "$WORK" 2>/dev/null || true
}
trap cleanup EXIT

version="$("$PY" -c 'import json;print(json.load(open("dist/metadata.json"))["version"])')"
echo "Release check for snapshot version $version"

echo "=== [1/5] Asset set ==="
expected="$(printf '%s\n' \
  marbor-linux-amd64 marbor-linux-arm64 marbor-darwin-amd64 marbor-darwin-arm64 marbor-windows-amd64.exe \
  marbor-agent-linux-amd64 marbor-agent-linux-arm64 marbor-agent-darwin-amd64 marbor-agent-darwin-arm64 marbor-agent-windows-amd64.exe \
  marbor-man-pages.tar.gz | sort)"
actual="$(awk '{print $2}' dist/checksums.txt | tr -d '\r*' | sort)"
if [ "$expected" != "$actual" ]; then
  echo "expected:"; echo "$expected"; echo "checksums.txt lists:"; echo "$actual"
  fail "the published asset set differs from the intended 10 binaries + man-page archive"
fi
ok "checksums.txt lists exactly the 11 intended files (plus itself = 12 release assets)"
"$PY" - <<'PYEOF2' || fail "the release would upload loose man pages or a MANIFEST.txt again"
import json, sys
bad = [a["name"] for a in json.load(open("dist/artifacts.json"))
       if a["type"] == "UploadableFile" or a["name"].endswith(".1") or a["name"] == "MANIFEST.txt"]
if bad:
    print("unexpected release files:", bad); sys.exit(1)
PYEOF2

echo "=== [2/5] Checksums match the files ==="
"$PY" - <<'PYEOF' || fail "a checksum in checksums.txt does not match its file"
import hashlib, json, sys
sums = {}
for line in open("dist/checksums.txt"):
    parts = line.split()
    if len(parts) == 2:
        sums[parts[1].lstrip("*")] = parts[0]
paths = {}
for a in json.load(open("dist/artifacts.json")):
    if a["name"] in sums and a.get("path"):
        paths[a["name"]] = a["path"]
missing = sorted(set(sums) - set(paths))
if missing:
    print("no artifact file found for:", missing); sys.exit(1)
for name, want in sorted(sums.items()):
    h = hashlib.sha256(open(paths[name], "rb").read()).hexdigest()
    if h != want:
        print("MISMATCH", name, want, h); sys.exit(1)
print("  ok: %d checksums verified against the built files" % len(sums))
PYEOF

echo "=== [3/5] Man-page archive contents ==="
want_pages="$(cd docs/man && ls ./*.1 | sed 's|^\./|man1/|' | sort)"
got_pages="$(tar -tzf dist/marbor-man-pages.tar.gz | sort)"
[ "$want_pages" = "$got_pages" ] || { diff <(echo "$want_pages") <(echo "$got_pages") || true; fail "marbor-man-pages.tar.gz does not hold exactly docs/man/*.1 under man1/"; }
page_count="$(echo "$got_pages" | wc -l | tr -d ' ')"
ok "archive holds exactly the $page_count pages in docs/man, all under man1/"

echo "=== [4/5] install.sh man-page step against the real archive ==="
mkdir "$WORK/release"
cp dist/marbor-man-pages.tar.gz dist/checksums.txt "$WORK/release/"
port=$((20000 + RANDOM % 20000))
(cd "$WORK/release" && exec "$PY" -m http.server "$port" --bind 127.0.0.1 >/dev/null 2>&1) &
server_pid=$!
for _ in $(seq 1 30); do
  curl -fsS "http://127.0.0.1:$port/checksums.txt" >/dev/null 2>&1 && break
  sleep 0.2
done
curl -fsS "http://127.0.0.1:$port/checksums.txt" >/dev/null 2>&1 || fail "local release server did not start"

man_root="$WORK/share/man"
funcs="$WORK/funcs.sh"
awk '/^fetch_quiet\(\) \{/{f=1} /^if \[ "\$ROLE" != "agent" \]; then/{f=0} f' install.sh > "$funcs"
[ -s "$funcs" ] || fail "could not extract the man-page functions from install.sh"
grep -q 'releases/latest/download' "$funcs" && grep -q '/usr/local/share/man/man1' "$funcs" \
  || fail "install.sh man-page block no longer has the URL/directory this check rewrites"
sed -i.bak -e "s|https://github.com/\${REPO}/releases/latest/download|http://127.0.0.1:$port|" \
           -e "s|/usr/local/share/man/man1|$man_root/man1|" "$funcs"
out="$(REPO=x/y sh -c ". '$funcs'; install_man_pages" 2>&1)" || fail "install_man_pages exited non-zero: $out"
installed="$(ls "$man_root/man1"/*.1 2>/dev/null | wc -l | tr -d ' ')"
[ "$installed" = "$page_count" ] || fail "installer placed $installed pages, expected $page_count: $out"
marker="$man_root/man1/.marbor-installed-manifest"
[ -f "$marker" ] || fail "installer wrote no uninstall marker"
[ "$(wc -l < "$marker" | tr -d ' ')" = "$page_count" ] || fail "uninstall marker does not list all $page_count pages"
grep -v -E '^[A-Za-z0-9._][A-Za-z0-9._-]*$' "$marker" && fail "marker has a name uninstall.sh would reject"
ok "verified and installed $installed pages, marker lists them all"
if command -v man >/dev/null 2>&1; then
  man -M "$man_root" marbor 2>/dev/null | head -n 5 | grep -q . || fail "man could not render the installed marbor page"
  ok "man renders the installed marbor page"
else
  echo "  (man not installed here - render check skipped)"
fi

echo "=== [5/5] Docker images ==="
if [ "${RELEASE_CHECK_SKIP_DOCKER:-}" = "1" ]; then
  echo "  RELEASE_CHECK_SKIP_DOCKER=1 - skipped"
  echo ""; echo "RELEASE CHECK PASSED (Docker step skipped)"; exit 0
fi
command -v docker >/dev/null 2>&1 || fail "docker is required for the image checks (set RELEASE_CHECK_SKIP_DOCKER=1 to skip)"
image="ghcr.io/anirudhx7/marbor:${version}"
for arch in amd64 arm64; do
  got="$(docker image inspect --format '{{.Architecture}}' "$image-$arch" 2>/dev/null || true)"
  [ "$got" = "$arch" ] || fail "release image $image-$arch is missing or has architecture '$got'"
done
ok "images exist for amd64 and arm64"

reported="$(docker run --rm "$image-amd64" -version 2>&1)" || fail "the amd64 image failed to run: $reported"
[ "$reported" = "marbor $version" ] || fail "image reports '$reported', expected 'marbor $version'"
ok "image reports its version: $reported"

container="marbor-release-check-$$"
hport=$((40000 + RANDOM % 20000))
docker run -d --name "$container" -p "127.0.0.1:$hport:8080" "$image-amd64" >/dev/null || fail "could not start the amd64 image"
healthy=false
for _ in $(seq 1 60); do
  if curl -fsS "http://127.0.0.1:$hport/health" >/dev/null 2>&1; then healthy=true; break; fi
  [ "$(docker inspect --format '{{.State.Running}}' "$container" 2>/dev/null)" = "true" ] || break
  sleep 1
done
if [ "$healthy" != true ]; then
  echo "--- container logs ---"; docker logs "$container" 2>&1 | tail -n 40 || true
  fail "a container started from the image never answered /health"
fi
ok "a container started from the image answers /health"

echo ""
echo "RELEASE CHECK PASSED"
