#!/usr/bin/env bash
# web-e2e-smoke — ticket 17.1 DoD: the Playwright smoke runs against the compose
# backend.
#
# Boots the full app stack, builds the web app, and runs the Playwright smoke
# spec (web/app/e2e/smoke.spec.ts) against it: the app lists definitions and runs
# from the backend through the typed client and its same-origin proxy, and no
# browser request carries the API key.
#
# Run locally (needs docker + node + corepack pnpm). CI runs the same steps in
# the `web-e2e` job.
set -euo pipefail

cd "$(dirname "$0")/.."

if [ -f .env ]; then
  set -a
  # shellcheck disable=SC1091
  . ./.env
  set +a
fi

# A stable root key so the app's server env and the seeding step agree.
if [ -z "${AGENTLOOM_API_ROOT_KEY:-}" ]; then
  AGENTLOOM_API_ROOT_KEY="sk_$(head -c 32 /dev/urandom | base64 | tr '+/' '-_' | tr -d '=')"
  export AGENTLOOM_API_ROOT_KEY
fi
export AGENTLOOM_API_URL="http://localhost:${AGENTLOOM_API_PORT:-8080}"
export AGENTLOOM_API_KEY="$AGENTLOOM_API_ROOT_KEY"

bold=$'\033[1m' dim=$'\033[2m' reset=$'\033[0m'
say()  { printf '\n%s▶ %s%s\n' "$bold" "$*" "$reset"; }
note() { printf '%s  %s%s\n' "$dim" "$*" "$reset"; }
fail() { printf 'web-e2e-smoke: %s\n' "$*" >&2; exit 1; }

for dep in docker node corepack; do
  command -v "$dep" >/dev/null || fail "missing dependency: $dep"
done

say "booting the app stack (api + 2 workers)"
note "docker compose --profile app up -d --build --wait"
docker compose --profile app up -d --build --wait

say "installing the web workspace"
( cd web && corepack pnpm install --frozen-lockfile )

say "building the app"
( cd web/app && corepack pnpm run build )

say "installing the Playwright Chromium browser"
# On CI (Linux) pull the system libs too; locally just the browser binary.
if [ -n "${CI:-}" ]; then
  ( cd web/app && corepack pnpm exec playwright install --with-deps chromium )
else
  ( cd web/app && corepack pnpm exec playwright install chromium )
fi

say "running the Playwright smoke against the compose backend"
( cd web/app && corepack pnpm exec playwright test )

say "done — the app lists definitions/runs through the typed client, key stayed server-side"
