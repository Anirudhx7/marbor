#!/usr/bin/env bash
# sanity.sh - Tier 2 of 3 (Smoke -> Sanity -> Regression). Fast, targeted `go test`
# for only the packages whose .go files actually changed, so an obvious mistake in
# the package you're touching fails in seconds instead of waiting on the full
# 4-6 minute regression suite (`go test -race ./...` in gate.sh / ci.yml's
# go-checks job) to grind through every package in the repo.
#
# This is a fast filter, not a substitute - the full regression run right after
# this step is unconditional and unchanged. If this script can't determine a
# usable diff base (shallow history, fresh branch, no prior commit), it skips
# cleanly rather than failing the gate - only an actual test failure in a
# determinable scope should ever block here.
#
# Where each tier runs:
#   Smoke      - scripts/smoke.sh, real-binary e2e (gate.sh step 7/7, ci.yml's
#                separate `smoke` job, gated as `docker-main`)
#   Sanity     - this script (gate.sh step 4/7, ci.yml's go-checks job, right
#                before the full test run)
#   Regression - `go test -race ./...`, unchanged (gate.sh step 5/7, ci.yml's
#                go-checks job's "Run Go Tests (race detector)" step)
#
# Base ref: GATE_SANITY_BASE env var if set (CI passes the PR base SHA for
# pull_request events, or the pre-push commit SHA for push events); otherwise
# falls back to HEAD~1, a reasonable local-dev default (the last commit's own
# changes). Uncommitted working-tree and staged changes are always included in
# addition to the base diff, since this is meant to catch mistakes when run
# locally via gate.sh before they're even committed.
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/.."

base="${GATE_SANITY_BASE:-}"
if [ -z "$base" ] || [ "$base" = "0000000000000000000000000000000000000000" ] || ! git cat-file -e "${base}^{commit}" 2>/dev/null; then
  if git cat-file -e "HEAD~1^{commit}" 2>/dev/null; then
    base="HEAD~1"
  else
    echo "Sanity: no usable base commit for a diff (shallow/fresh history) - skipping."
    exit 0
  fi
fi

changed_files=$( { git diff --name-only "$base" -- '*.go' 2>/dev/null; git diff --name-only HEAD -- '*.go' 2>/dev/null; git diff --name-only --cached -- '*.go' 2>/dev/null; } | sort -u)

if [ -z "$changed_files" ]; then
  echo "Sanity: no changed .go files against $base - skipping (nothing scoped to test)."
  exit 0
fi

# Map each changed file to its containing package directory, de-duplicated.
# A root-level file's dirname is "." - go test needs that exact form, not "./.".
packages=$(echo "$changed_files" | xargs -n1 dirname | sort -u | awk '{ if ($0 == ".") print "."; else print "./" $0 }')

echo "Sanity: testing $(echo "$packages" | wc -l | tr -d ' ') changed package(s) (base: $base):"
echo "$packages"

# 300s: matches the full regression step's timeout (ci.yml/gate.sh). Verified
# empirically that 200s is NOT enough margin for internal/admin alone under
# -race on a real run (timed out at exactly 200s) - use the same budget as
# regression rather than re-deriving a smaller one that keeps proving wrong.
# shellcheck disable=SC2086
go test -race -timeout 300s $packages
