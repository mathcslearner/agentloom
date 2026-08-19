#!/usr/bin/env bash
# check-bundle-no-key — ticket 17.1 DoD: the API key is never shipped to the
# browser in proxy mode.
#
# Builds the app with a sentinel API key in the *server* environment, then greps
# the client bundle (.next/static) for that sentinel value and for the env-var
# name itself. Either appearing means a server secret leaked into browser code —
# a hard failure. The sentinel is generated at runtime (never committed), so the
# repo's `sk_`-shaped-literal leak check stays clean.
set -euo pipefail

cd "$(dirname "$0")/.."

SENTINEL="sk_bundlecheck_$(head -c 18 /dev/urandom | base64 | tr '+/' '-_' | tr -d '=')"

echo "▶ building with a sentinel AGENTLOOM_API_KEY in the server env"
AGENTLOOM_API_KEY="$SENTINEL" AGENTLOOM_API_URL="http://127.0.0.1:8080" \
  corepack pnpm exec next build >/dev/null

STATIC_DIR=".next/static"
if [ ! -d "$STATIC_DIR" ]; then
  echo "check-bundle-no-key: no $STATIC_DIR after build" >&2
  exit 1
fi

fail=0
if grep -rqF "$SENTINEL" "$STATIC_DIR"; then
  echo "✗ FAIL: the sentinel API key value appears in the client bundle" >&2
  grep -rlF "$SENTINEL" "$STATIC_DIR" >&2 || true
  fail=1
fi
if grep -rqF "AGENTLOOM_API_KEY" "$STATIC_DIR"; then
  echo "✗ FAIL: the env-var name AGENTLOOM_API_KEY appears in the client bundle" >&2
  grep -rlF "AGENTLOOM_API_KEY" "$STATIC_DIR" >&2 || true
  fail=1
fi

if [ "$fail" -ne 0 ]; then
  exit 1
fi

echo "✓ PASS: neither the API key value nor AGENTLOOM_API_KEY is in $STATIC_DIR"
