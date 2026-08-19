#!/usr/bin/env bash
# load-env — boot / tear down the resource-pinned load environment (ticket
# 19.1). It layers docker-compose.load.yml over the base stack (app + obs
# profiles) so the base compose file stays the single source of truth, echoes
# the resource pins so a campaign is reproducible against a documented machine,
# and applies migrations against the dedicated load database.
#
# Usage:
#   scripts/load-env.sh up      # build + boot + migrate + wait + print pins
#   scripts/load-env.sh status  # show service status and worker replica count
#   scripts/load-env.sh down     # stop services (dedicated load volumes kept)
#   scripts/load-env.sh nuke     # stop services AND drop the load volumes
#
# Knobs (env):
#   AGENTLOOM_LOAD_WORKERS  worker replica count (default 8)
#   AGENTLOOM_LOAD_OTEL     "true" to enable OTel export under load (default off)
set -euo pipefail

cd "$(dirname "$0")/.."

if [ -f .env ]; then
  set -a
  # shellcheck disable=SC1091
  . ./.env
  set +a
fi

COMPOSE=(docker compose -f docker-compose.yml -f docker-compose.load.yml)
WORKERS="${AGENTLOOM_LOAD_WORKERS:-8}"

bold=$'\033[1m' dim=$'\033[2m' reset=$'\033[0m'
say()  { printf '\n%s▶ %s%s\n' "$bold" "$*" "$reset"; }
note() { printf '%s  %s%s\n' "$dim" "$*" "$reset"; }
fail() { printf 'load-env: %s\n' "$*" >&2; exit 1; }

for dep in docker; do
  command -v "$dep" >/dev/null || fail "missing dependency: $dep"
done

print_pins() {
  say "load environment resource pins"
  note "postgres  : 2.0 CPU / 2 GiB   (pg_stat_statements on; shared_buffers=512MB, max_connections=200)"
  note "redis     : 1.0 CPU / 768 MiB (AOF on)"
  note "api       : 1.0 CPU / 768 MiB (cache OFF; rate limits raised; pprof on)"
  note "worker x${WORKERS} : 0.5 CPU / 384 MiB each (cache OFF; load mock script; pprof on)"
  note "otel export: ${AGENTLOOM_LOAD_OTEL:-false}"
  note "volumes   : postgres-load-data, redis-load-data (dedicated; make load-nuke drops them)"
  note "admin/pprof: worker & api :9090 (in-network only) — capture via 'docker compose ... exec'"
}

case "${1:-up}" in
  up)
    say "booting load stack (app + obs profiles), ${WORKERS} workers"
    AGENTLOOM_LOAD_WORKERS="$WORKERS" AGENTLOOM_OBS_OTEL_ENABLED="${AGENTLOOM_LOAD_OTEL:-false}" \
      "${COMPOSE[@]}" --profile app --profile obs up -d --build --wait
    print_pins
    say "service status"
    "${COMPOSE[@]}" ps
    say "ready"
    note "Prometheus: http://localhost:${AGENTLOOM_PROMETHEUS_PORT:-9090}   Grafana: http://localhost:${AGENTLOOM_GRAFANA_PORT:-3000}"
    note "API:        http://localhost:${AGENTLOOM_API_PORT:-8080}"
    ;;
  status)
    "${COMPOSE[@]}" ps
    ;;
  down)
    say "stopping load stack (volumes kept)"
    "${COMPOSE[@]}" --profile app --profile obs down
    ;;
  nuke)
    say "stopping load stack and dropping load volumes"
    "${COMPOSE[@]}" --profile app --profile obs down
    docker volume rm agentloom_postgres-load-data agentloom_redis-load-data 2>/dev/null || true
    note "load volumes dropped; next 'up' starts from a pristine database"
    ;;
  *)
    fail "unknown command '$1' (want: up | status | down | nuke)"
    ;;
esac
