#!/usr/bin/env bash
# load-campaign — run one reproducible baseline campaign (ticket 19.3) against
# the pinned load stack (`make load-up`) and capture the full evidence bundle:
# the loadgen report, pprof (worker + API), pg_stat_statements, pg_stat_activity,
# Redis INFO/LATENCY, docker stats, and Prometheus range exports over the exact
# campaign window. Every number the findings doc publishes is reproducible from
# this one command (docs/load/plan.md §7).
#
# Usage:
#   scripts/load-campaign.sh <scenario> [loadgen flags...]
#
# Examples:
#   scripts/load-campaign.sh linear-10 --ramp 1:10:1:60s --run-timeout 15m
#   scripts/load-campaign.sh mixed --rate 4 --duration 10m
#
# Knobs (env):
#   AGENTLOOM_API_URL   default http://localhost:8080
#   AGENTLOOM_API_KEY   bearer (falls back to AGENTLOOM_API_ROOT_KEY from .env)
#   AGENTLOOM_PROMETHEUS_PORT  host Prometheus port (default 9090)
#   CAMPAIGN_PPROF_DELAY       seconds after loadgen start to capture pprof (default 90)
#   CAMPAIGN_PPROF_SECONDS     CPU profile duration (default 30)
#   OUT                        bundle directory (default results/<scenario>-<utc>)
set -euo pipefail
cd "$(dirname "$0")/.."

[ -f .env ] && { set -a; . ./.env; set +a; }

SCENARIO="${1:-}"
[ -n "$SCENARIO" ] || { echo "usage: $0 <scenario> [loadgen flags...]" >&2; exit 2; }
shift || true

API_URL="${AGENTLOOM_API_URL:-http://localhost:8080}"
API_KEY="${AGENTLOOM_API_KEY:-${AGENTLOOM_API_ROOT_KEY:-}}"
[ -n "$API_KEY" ] || { echo "load-campaign: no API key (set AGENTLOOM_API_KEY or AGENTLOOM_API_ROOT_KEY)" >&2; exit 2; }
PROM_PORT="${AGENTLOOM_PROMETHEUS_PORT:-9090}"
PPROF_DELAY="${CAMPAIGN_PPROF_DELAY:-90}"
PPROF_SECONDS="${CAMPAIGN_PPROF_SECONDS:-30}"
STAMP="$(date -u +%Y%m%dT%H%M%SZ)"
OUT="${OUT:-results/${SCENARIO}-${STAMP}}"
mkdir -p "$OUT/prom" "$OUT/pprof"

COMPOSE=(docker compose -f docker-compose.yml -f docker-compose.load.yml)

bold=$'\033[1m'; dim=$'\033[2m'; reset=$'\033[0m'
say()  { printf '\n%s▶ %s%s\n' "$bold" "$*" "$reset"; }
note() { printf '%s  %s%s\n' "$dim" "$*" "$reset"; }

psql_load() { "${COMPOSE[@]}" exec -T postgres psql -U agentloom -d agentloom -qtAX "$@"; }

# ── 0. provenance ────────────────────────────────────────────────────────────
say "campaign bundle → $OUT"
{
  echo "scenario: $SCENARIO"
  echo "loadgen_args: $*"
  echo "utc: $STAMP"
  echo "git_sha: $(git rev-parse HEAD 2>/dev/null || echo unknown)"
  echo "git_dirty: $([ -n "$(git status --porcelain 2>/dev/null)" ] && echo yes || echo no)"
  echo "host_uname: $(uname -srm)"
  echo "docker_cpus: $(docker info --format '{{.NCPU}}' 2>/dev/null)"
  echo "docker_mem_bytes: $(docker info --format '{{.MemTotal}}' 2>/dev/null)"
  echo "load_workers: ${AGENTLOOM_LOAD_WORKERS:-8}"
  echo "prometheus_port: $PROM_PORT"
} > "$OUT/env.txt"
"${COMPOSE[@]}" config > "$OUT/compose.resolved.yml" 2>/dev/null || true
"${COMPOSE[@]}" ps > "$OUT/services.txt" 2>/dev/null || true

# ── 1. reset pg_stat_statements + arm Redis latency monitor ──────────────────
say "resetting pg_stat_statements and arming Redis latency monitor"
psql_load -c "SELECT pg_stat_statements_reset();" >/dev/null 2>&1 || note "pg_stat_statements_reset failed (extension present?)"
"${COMPOSE[@]}" exec -T redis redis-cli CONFIG SET latency-monitor-threshold 5 >/dev/null 2>&1 || true
"${COMPOSE[@]}" exec -T redis redis-cli LATENCY RESET >/dev/null 2>&1 || true
"${COMPOSE[@]}" exec -T redis redis-cli INFO > "$OUT/redis.info.start.txt" 2>/dev/null || true

# ── 2. background samplers (docker stats, pg_stat_activity) ───────────────────
STOP="$OUT/.stop"
rm -f "$STOP"
(
  echo "at_utc,name,cpu_pct,mem_usage,mem_pct" > "$OUT/docker-stats.csv"
  while [ ! -f "$STOP" ]; do
    ts="$(date -u +%H:%M:%S)"
    docker stats --no-stream --format '{{.Name}},{{.CPUPerc}},{{.MemUsage}},{{.MemPerc}}' \
      2>/dev/null | sed "s/^/$ts,/" >> "$OUT/docker-stats.csv" || true
    sleep 10
  done
) &
STATS_PID=$!
(
  echo "-- pg_stat_activity wait events, sampled every 10s --" > "$OUT/pg-activity.txt"
  while [ ! -f "$STOP" ]; do
    echo "== $(date -u +%H:%M:%S) ==" >> "$OUT/pg-activity.txt"
    psql_load -c "SELECT state, wait_event_type, wait_event, count(*)
                    FROM pg_stat_activity WHERE datname='agentloom'
                   GROUP BY 1,2,3 ORDER BY 4 DESC;" >> "$OUT/pg-activity.txt" 2>/dev/null || true
    sleep 10
  done
) &
ACT_PID=$!

# ── 3. pprof capture, fired mid-steady-window in the background ───────────────
(
  sleep "$PPROF_DELAY"
  say "capturing pprof (${PPROF_SECONDS}s CPU) — worker + API" >&2
  "${COMPOSE[@]}" exec -T worker wget -qO- "http://localhost:9090/debug/pprof/profile?seconds=${PPROF_SECONDS}" > "$OUT/pprof/worker.cpu.pprof" 2>/dev/null || note "worker CPU pprof failed (pprof enabled?)"
  "${COMPOSE[@]}" exec -T worker wget -qO- "http://localhost:9090/debug/pprof/heap" > "$OUT/pprof/worker.heap.pprof" 2>/dev/null || true
  "${COMPOSE[@]}" exec -T api    wget -qO- "http://localhost:9090/debug/pprof/profile?seconds=${PPROF_SECONDS}" > "$OUT/pprof/api.cpu.pprof" 2>/dev/null || true
  "${COMPOSE[@]}" exec -T api    wget -qO- "http://localhost:9090/debug/pprof/heap" > "$OUT/pprof/api.heap.pprof" 2>/dev/null || true
) &
PPROF_PID=$!

cleanup() { touch "$STOP"; wait "$STATS_PID" "$ACT_PID" "$PPROF_PID" 2>/dev/null || true; }
trap cleanup EXIT

# ── 4. the campaign ──────────────────────────────────────────────────────────
say "running loadgen: --scenario $SCENARIO $*"
AGENTLOOM_API_URL="$API_URL" AGENTLOOM_API_KEY="$API_KEY" \
  go run ./cmd/loadgen --scenario "$SCENARIO" --out "$OUT" "$@" || note "loadgen exited non-zero (see taxonomy)"

# stop samplers + let pprof finish before post-capture
touch "$STOP"; wait "$STATS_PID" "$ACT_PID" "$PPROF_PID" 2>/dev/null || true; trap - EXIT

# ── 5. post-campaign Postgres / Redis snapshots ──────────────────────────────
say "post-campaign Postgres + Redis snapshots"
psql_load -F',' -c "\copy (SELECT calls, total_exec_time, mean_exec_time, rows, query
                             FROM pg_stat_statements ORDER BY total_exec_time DESC LIMIT 25)
                     TO STDOUT WITH CSV HEADER" > "$OUT/pgss-by-time.csv" 2>/dev/null || true
psql_load -F',' -c "\copy (SELECT calls, total_exec_time, mean_exec_time, rows, query
                             FROM pg_stat_statements ORDER BY calls DESC LIMIT 25)
                     TO STDOUT WITH CSV HEADER" > "$OUT/pgss-by-calls.csv" 2>/dev/null || true
"${COMPOSE[@]}" exec -T redis redis-cli INFO > "$OUT/redis.info.end.txt" 2>/dev/null || true
"${COMPOSE[@]}" exec -T redis redis-cli LATENCY LATEST > "$OUT/redis.latency.txt" 2>/dev/null || true
"${COMPOSE[@]}" exec -T redis redis-cli INFO commandstats >> "$OUT/redis.latency.txt" 2>/dev/null || true

# ── 6. Prometheus range exports over the exact campaign window ────────────────
if [ -f "$OUT/summary.json" ]; then
  say "exporting Prometheus range series over the campaign window"
  read -r START END STEP < <(python3 - "$OUT/summary.json" <<'PY'
import json,sys,datetime
w=json.load(open(sys.argv[1]))["windows"]
def ep(s): return int(datetime.datetime.fromisoformat(s.replace("Z","+00:00")).timestamp())
s=ep(w["campaign_start"]); e=ep(w["arrivals_end"])
print(s, e, max(1,(e-s)//400))
PY
)
  base="http://localhost:${PROM_PORT}/api/v1/query_range"
  export_q() { # name  promql
    curl -sG "$base" --data-urlencode "query=$2" \
      --data-urlencode "start=$START" --data-urlencode "end=$END" --data-urlencode "step=${STEP}s" \
      -o "$OUT/prom/$1.json" 2>/dev/null || true
  }
  export_q sched_p50   'histogram_quantile(0.50, sum(rate(engine_step_scheduling_latency_seconds_bucket[1m])) by (le))'
  export_q sched_p99   'histogram_quantile(0.99, sum(rate(engine_step_scheduling_latency_seconds_bucket[1m])) by (le))'
  export_q ready_depth 'max(engine_queue_ready_depth)'
  export_q pel         'max(engine_queue_pel_size)'
  export_q delayed     'max(engine_queue_delayed_depth)'
  export_q outbox      'max(engine_outbox_backlog)'
  export_q dispatch_p99 'histogram_quantile(0.99, sum(rate(engine_dispatch_lag_seconds_bucket[1m])) by (le))'
  export_q claims      'sum(rate(engine_step_claims_total[1m])) by (result)'
  export_q step_p99    'histogram_quantile(0.99, sum(rate(engine_step_duration_seconds_bucket[1m])) by (le))'
  export_q run_rate    'sum(rate(engine_run_duration_seconds_count[1m]))'
  export_q api_submit_p99 'histogram_quantile(0.99, sum(rate(engine_api_request_duration_seconds_bucket{route!="unmatched"}[1m])) by (le))'
  export_q worker_cpu  'sum(rate(process_cpu_seconds_total{job="worker"}[1m]))'
  note "range: $(date -u -r "$START" +%H:%M:%S 2>/dev/null || echo "$START") → $(date -u -r "$END" +%H:%M:%S 2>/dev/null || echo "$END") step ${STEP}s"
else
  note "no summary.json — skipping Prometheus export (did loadgen run?)"
fi

# ── 7. render pprof tops (best effort — needs the go toolchain) ───────────────
say "rendering pprof top functions"
for f in worker.cpu api.cpu worker.heap api.heap; do
  p="$OUT/pprof/$f.pprof"
  [ -s "$p" ] || continue
  go tool pprof -top -nodecount=30 "$p" > "$OUT/pprof/$f.top.txt" 2>/dev/null \
    && note "→ pprof/$f.top.txt" || note "pprof render failed for $f (empty profile?)"
done

say "done — bundle at $OUT"
note "loadgen report:  $OUT/summary.md"
note "ramp knee table: see 'Ramp steps' in summary.md; cross-check prom/sched_p99.json"
note "write findings:  docs/load/findings-baseline.md"
