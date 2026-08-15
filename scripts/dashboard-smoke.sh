#!/usr/bin/env bash
# dashboard-smoke — ticket 7.5's acceptance script: boot the compose stack
# with the obs profile, drive a chaos-grade workload (fan-outs, a retry, a
# burst of retries-exhausted dead letters, a worker SIGKILL mid-step, a
# 429 storm), then assert
#   1. every panel query in the provisioned Engine and API dashboards
#      returns a non-empty result from Prometheus (a short allowlist of
#      deliberately-quiet-when-healthy panels excepted), and
#   2. the four example alert rules are loaded, with DeadLetterRateSpike
#      test-fired to the `firing` state by the dead-letter burst.
#
# "Under the chaos suite" (ROADMAP 7.5): the sustained chaos suite
# (test/crash) runs host subprocesses on isolated queue keys that compose
# Prometheus structurally cannot scrape, so this script recreates its
# signal shape — crash/reclaim/takeover, retries, dead letters — against
# the scrapable compose fleet instead.
#
# Prerequisites: docker compose, curl, jq. Run via `make smoke-dashboards`
# on an otherwise idle stack (the lease-holder lookup expects the kill
# scenario's step to be the only pending queue entry).
set -euo pipefail

cd "$(dirname "$0")/.."

if [ -f .env ]; then
  set -a
  # shellcheck disable=SC1091
  . ./.env
  set +a
fi

# Root credential: reuse .env's, or mint an ephemeral one (constructed at
# runtime — never a committed literal) and hand it to the api container.
if [ -z "${AGENTLOOM_API_ROOT_KEY:-}" ]; then
  AGENTLOOM_API_ROOT_KEY="sk_$(head -c 32 /dev/urandom | base64 | tr '+/' '-_' | tr -d '=')"
  export AGENTLOOM_API_ROOT_KEY
fi
API_KEY="$AGENTLOOM_API_ROOT_KEY"
API_URL="http://localhost:${AGENTLOOM_API_PORT:-8080}"
PROM_URL="http://localhost:${AGENTLOOM_PROMETHEUS_PORT:-9090}"
GRAFANA_URL="http://localhost:${AGENTLOOM_GRAFANA_PORT:-3000}"

# 5s lease (default 30s) so the SIGKILL scenario reclaims quickly; the
# compose file passes this through to the workers.
export AGENTLOOM_QUEUE_LEASE_TTL="${AGENTLOOM_QUEUE_LEASE_TTL:-5s}"

bold=$'\033[1m' dim=$'\033[2m' reset=$'\033[0m'
say()  { printf '\n%s▶ %s%s\n' "$bold" "$*" "$reset"; }
note() { printf '%s  %s%s\n' "$dim" "$*" "$reset"; }
fail() { printf 'dashboard-smoke: %s\n' "$*" >&2; exit 1; }

for dep in docker curl jq; do
  command -v "$dep" >/dev/null || fail "missing dependency: $dep"
done

say "booting compose app+obs profiles (idempotent), lease TTL $AGENTLOOM_QUEUE_LEASE_TTL"
AGENTLOOM_OBS_OTEL_ENABLED=true docker compose --profile app --profile obs up -d --build --wait

# ---------------------------------------------------------------- helpers

# submit <definition-json> [params-json] — POST a run, print the run id.
# The optional second arg supplies run params (default {}); fanout.json
# templates on run.params.topic, so it must be submitted with one.
submit() {
  local params="${2:-}"
  [ -n "$params" ] || params='{}'
  jq -n --argjson d "$1" --argjson p "$params" '{definition: $d, params: $p}' |
    curl -fsS -X POST -H "Authorization: Bearer $API_KEY" -H "Content-Type: application/json" \
      -d @- "$API_URL/v1/runs" | jq -r '.run_id'
}

run_json()   { curl -fsS -H "Authorization: Bearer $API_KEY" "$API_URL/v1/runs/$1"; }
run_status() { run_json "$1" | jq -r '.run.status'; }

wait_terminal() {
  local id="$1" want="$2" deadline=$((SECONDS + 180)) status
  while :; do
    status="$(run_status "$id")"
    case "$status" in
      succeeded|failed|cancelled) break ;;
    esac
    ((SECONDS < deadline)) || fail "run $id stuck in $status"
    sleep 0.5
  done
  [ "$status" = "$want" ] || fail "run $id ended $status, want $want"
}

wait_for() {
  local desc="$1" fn="$2" deadline=$((SECONDS + 120))
  until "$fn"; do
    ((SECONDS < deadline)) || fail "timed out waiting for $desc"
    sleep 0.5
  done
}

# lease_holder / victim_container: the demo-crash lookup — the consumer
# name holding the sole pending entry, mapped to its container. Consumer
# names start with the container hostname, which in compose is the
# 12-char short container ID, so a prefix match suffices (grepping logs,
# as demo-crash does, is pipefail-fragile under load: grep -q's early
# exit SIGPIPEs a still-streaming docker logs into exit 141).
QUEUE_STREAM="${AGENTLOOM_QUEUE_STREAM:-steps:ready}"
QUEUE_GROUP="${AGENTLOOM_QUEUE_GROUP:-workers}"
lease_holder() {
  docker compose exec -T redis redis-cli --no-raw XPENDING "$QUEUE_STREAM" "$QUEUE_GROUP" - + 10 |
    sed -n 's/^ *2) "\(.*\)"$/\1/p' | head -1
}
victim_container() {
  local cid
  for cid in $(docker compose ps -q worker); do
    case "$1" in
      "$(printf '%.12s' "$cid")"*) echo "$cid"; return ;;
    esac
  done
}

# ---------------------------------------------------------------- workload

# SIGKILL scenario FIRST, on the freshly-booted idle stack: the
# lease-holder lookup expects the crash run's step to be the only pending
# queue entry, and — the load-bearing part — a worker restart resets its
# in-process counter registry, so any label-vec counters only that worker
# had recorded (no series until first increment) would go stale
# fleet-wide. Killing first means the dead-letter burst below lands on a
# fleet that stays alive through the panel and alert checks.
say "SIGKILL scenario — killing the worker holding a lease mid-step"
crash_id="$(jq -n --argjson d "$(cat docs/demos/crash-demo.json)" '{definition: $d}' |
  curl -fsS -X POST -H "Authorization: Bearer $API_KEY" -H "Content-Type: application/json" \
    -d @- "$API_URL/v1/runs" | jq -r '.run_id')"
note "crash-demo run: $crash_id"

long_task_running() {
  [ "$(run_json "$crash_id" | jq -r '.steps[] | select(.id=="long_task") | .status')" = running ]
}
wait_for "long_task to be running" long_task_running

HOLDER=""
holder_known() { HOLDER=$(lease_holder); [ -n "$HOLDER" ]; }
wait_for "the lease holder to appear in the PEL" holder_known
VICTIM=$(victim_container "$HOLDER")
[ -n "$VICTIM" ] || fail "no worker container's logs carry consumer $HOLDER"
note "consumer $HOLDER holds the lease (container ${VICTIM:0:12}) — SIGKILL"
docker kill -s KILL "$VICTIM" >/dev/null

reclaimed() {
  [ "$(run_json "$crash_id" | jq -r '.steps[] | select(.id=="long_task") | .attempt_count')" = 2 ]
}
wait_for "the reclaim (attempt 2 on long_task)" reclaimed
note "reclaimed — engine_queue_reclaimed_total and engine_step_takeovers_total moved"

say "restoring the killed worker"
AGENTLOOM_OBS_OTEL_ENABLED=true docker compose --profile app --profile obs up -d --wait

say "waiting for the crash run to complete"
wait_terminal "$crash_id" succeeded
note "crash run recovered to succeeded"

# Dead-letter burst on the now-stable fleet, PACED at one submission per
# 2s: rate() only measures increases between samples of a visible
# series, and a vec counter has no series until its first increment — a
# burst absorbed by one worker between two scrapes is born at its final
# value and rates as zero (observed: the restarted worker claimed all 10
# and hit 10 before Prometheus's first scrape of its new target).
# Pacing ~24s of increments across the 5s scrape interval guarantees
# observed increases; 12 rows ≈ 0.03/s over the 5m window — over
# DeadLetterRateSpike's 0.01/s threshold with margin even if the first
# couple of increments predate the series' first sample. The `for: 2m`
# clock runs while the rest of the workload and the panel checks
# execute, so the alert wait below catches it firing.
say "submitting the dead-letter burst (12 retries-exhausted runs, paced 2s)"
doomed_def='{
  "schema_version": 1, "name": "smoke-doomed",
  "steps": [{"id": "doomed", "type": "fail_n_times", "config": {"n": 9},
             "retry": {"max_attempts": 2, "backoff": {"initial": "1s", "cap": "5s", "multiplier": 2}, "jitter": "none"}}],
  "edges": []
}'
doomed_ids=()
for _ in $(seq 12); do
  doomed_ids+=("$(submit "$doomed_def")")
  sleep 2
done
note "12 doomed runs submitted"

say "submitting fan-outs and a retry run"
fanout_ids=()
fanout_def="$(cat examples/definitions/fanout.json)"
for _ in 1 2 3; do
  fanout_ids+=("$(submit "$fanout_def" '{"topic":"turtles"}')")
done
retry_id="$(submit '{
  "schema_version": 1, "name": "smoke-retry",
  "steps": [{"id": "flaky", "type": "fail_n_times", "config": {"n": 1},
             "retry": {"max_attempts": 3, "backoff": {"initial": "1s", "cap": "5s", "multiplier": 2}, "jitter": "none"}}],
  "edges": []
}')"
note "3 fanout runs + 1 retry run"

# Cost signal (ticket 10.5): a deterministic temperature=0 mock llm step
# submitted repeatedly — the first run spends (engine_cost_spent_usd_total),
# the rest are response-cache hits recording counterfactual savings
# (engine_cost_saved_usd_total). Paced 2s like the dead-letter burst so the
# spend/saved increments are observed across scrapes and the rate() panels
# are non-empty. Budget parks and downgrades are not driven here (the "Budget
# actions & downgrades/s" panel is allowlisted below).
say "submitting the cost workload (1 spend + cache-hit savings, paced 2s)"
cost_def='{
  "schema_version": 1, "name": "smoke-cost",
  "steps": [{"id": "gen", "type": "llm",
             "config": {"model": "mock/sim-1", "prompt": "summarize turtles", "temperature": 0}}],
  "edges": []
}'
cost_ids=()
for _ in $(seq 6); do
  cost_ids+=("$(submit "$cost_def")")
  sleep 2
done
note "6 cost runs submitted (1 miss + 5 cache hits)"

say "429 storm — admin class (capacity 10, refill 2/s)"
got_429=0
for _ in $(seq 30); do
  code=$(curl -s -o /dev/null -w '%{http_code}' -H "Authorization: Bearer $API_KEY" "$API_URL/v1/keys")
  [ "$code" = 429 ] && got_429=$((got_429 + 1))
done
[ "$got_429" -gt 0 ] || fail "no 429 out of 30 admin-class requests"
note "$got_429 requests rate-limited"

say "waiting for terminal states"
for id in "${doomed_ids[@]}"; do wait_terminal "$id" failed; done
for id in "${fanout_ids[@]}"; do wait_terminal "$id" succeeded; done
for id in "${cost_ids[@]}"; do wait_terminal "$id" succeeded; done
wait_terminal "$retry_id" succeeded
note "all runs terminal"

# Two scrape intervals (5s) plus the 10s gauge-sample interval.
say "letting Prometheus scrape (15s)"
sleep 15

# ---------------------------------------------------------------- panels

query_count() {
  curl -fsS --get --data-urlencode "query=$1" "$PROM_URL/api/v1/query" |
    jq -r '.data.result | length'
}

# Panels whose queries are legitimately empty on a healthy stack: their
# metrics are label-vec counters with no series until the failure mode
# happens (an empty panel is the good state, and the panel descriptions
# say so).
allowlisted() {
  case "$1" in
    *engine_reconcile_healed_total*) return 0 ;;  # queue-level reclaim usually beats the reconciler
    *engine_api_ratelimit_failopen_total*) return 0 ;;  # rate-limit Redis is healthy
    *'code=~"5.."'*) return 0 ;;  # no server errors driven, by design
    *engine_cost_budget_exceeded_total*) return 0 ;;  # no budgeted runs in the workload (ticket 10.5)
    *engine_cost_downgrades_total*) return 0 ;;  # no model_fallbacks in the workload (ticket 10.5)
  esac
  return 1
}

failures=0
check_dashboard() {
  local file="$1"
  say "checking every panel query in $(basename "$file")"
  while IFS=$'\t' read -r title expr; do
    [ -n "$expr" ] || continue
    if allowlisted "$expr"; then
      note "allowed empty  —  $title"
      continue
    fi
    if [ "$(query_count "$expr")" -gt 0 ]; then
      note "non-empty ✓  $title"
    else
      printf '  EMPTY    ✗  %s: %s\n' "$title" "$expr"
      failures=$((failures + 1))
    fi
  done < <(jq -r '.panels[] | select(.targets) | .title as $t | .targets[] | [$t, .expr] | @tsv' "$file")
}

check_dashboard deploy/observability/grafana/dashboards/engine.json
check_dashboard deploy/observability/grafana/dashboards/api.json

# ---------------------------------------------------------------- alerts

say "checking the alert rules loaded in Prometheus"
rules_json="$(curl -fsS "$PROM_URL/api/v1/rules")"
for alert in QueueDepthGrowing DeadLetterRateSpike ReclaimRateSpike OutboxDispatchLag BudgetParkRateSpike; do
  if [ "$(jq -r --arg a "$alert" '[.data.groups[].rules[] | select(.name==$a)] | length' <<<"$rules_json")" = 1 ]; then
    note "loaded ✓  $alert"
  else
    printf '  MISSING  ✗  alert rule %s\n' "$alert"
    failures=$((failures + 1))
  fi
done

say "waiting for DeadLetterRateSpike to fire (for: 2m hold)"
alert_firing() {
  curl -fsS "$PROM_URL/api/v1/rules" |
    jq -e '[.data.groups[].rules[] | select(.name=="DeadLetterRateSpike") | .alerts[]? | select(.state=="firing")] | length > 0' >/dev/null
}
fire_deadline=$((SECONDS + 240))
until alert_firing; do
  ((SECONDS < fire_deadline)) || { printf '  NOT FIRING ✗  DeadLetterRateSpike\n'; failures=$((failures + 1)); break; }
  sleep 5
done
if alert_firing; then
  note "firing ✓  DeadLetterRateSpike"
  curl -fsS "$PROM_URL/api/v1/alerts" |
    jq '.data.alerts[] | select(.labels.alertname=="DeadLetterRateSpike") | {state, activeAt, value}'
fi

if [ "$failures" -gt 0 ]; then
  fail "$failures check(s) failed"
fi
say "all dashboard panels non-empty, rules loaded, DeadLetterRateSpike test-fired"
note "Engine dashboard: $GRAFANA_URL/d/agentloom-engine"
note "API dashboard:    $GRAFANA_URL/d/agentloom-api"
