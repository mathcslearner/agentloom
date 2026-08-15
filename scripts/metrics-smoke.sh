#!/usr/bin/env bash
# metrics-smoke — ticket 7.2's acceptance script: boot the compose stack
# with the obs profile, drive a small mixed workload through the API
# (fan-outs, a retry-then-succeed, a deliberate retries-exhausted
# dead-letter), then interrogate Prometheus for every 7.2 metric.
#
# Two assertion tiers:
#   - EXIST: the series must be scrapeable (plain counters and gauges are
#     registered at 0, so their absence means broken wiring);
#   - POSITIVE: the workload must have moved the value above 0.
# Metrics that only appear under crash/chaos conditions (reclaims firing,
# poison diversions, fencing rejections, reconcile heals, 429s) are
# registered as plain counters where possible and checked at tier EXIST;
# the vec-shaped heal counter (engine_reconcile_healed_total) has no
# series until a heal happens and is deliberately not asserted — the crash
# suite (test/crash) is where those paths run.
#
# Prerequisites: docker compose, curl, jq. Run via `make smoke-metrics`.
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

bold=$'\033[1m' dim=$'\033[2m' reset=$'\033[0m'
say()  { printf '\n%s▶ %s%s\n' "$bold" "$*" "$reset"; }
note() { printf '%s  %s%s\n' "$dim" "$*" "$reset"; }
fail() { printf 'metrics-smoke: %s\n' "$*" >&2; exit 1; }

for dep in docker curl jq; do
  command -v "$dep" >/dev/null || fail "missing dependency: $dep"
done

say "booting compose app+obs profiles (idempotent)"
AGENTLOOM_OBS_OTEL_ENABLED=true docker compose --profile app --profile obs up -d --build --wait

# submit <definition-json> [params-json] — POST an inline definition, print
# the run id. The optional second arg supplies run params (default {});
# fanout.json templates on run.params.topic, so it must carry one.
submit() {
  local params="${2:-}"
  [ -n "$params" ] || params='{}'
  jq -n --argjson d "$1" --argjson p "$params" '{definition: $d, params: $p}' |
    curl -fsS -X POST -H "Authorization: Bearer $API_KEY" -H "Content-Type: application/json" \
      -d @- "$API_URL/v1/runs" | jq -r '.run_id'
}

run_status() {
  curl -fsS -H "Authorization: Bearer $API_KEY" "$API_URL/v1/runs/$1" | jq -r '.run.status'
}

wait_terminal() {
  local id="$1" want="$2" deadline=$((SECONDS + 120)) status
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

say "submitting workload"
fanout_ids=()
fanout_def="$(cat examples/definitions/fanout.json)"
for _ in 1 2 3; do
  fanout_ids+=("$(submit "$fanout_def" '{"topic":"turtles"}')")
done
note "3 fanout runs: ${fanout_ids[*]}"

# One transient failure, healed by the retry engine on a 1s backoff —
# exercises retries_total, the delayed set, and the promoter.
retry_id="$(submit '{
  "schema_version": 1, "name": "smoke-retry",
  "steps": [{"id": "flaky", "type": "fail_n_times", "config": {"n": 1},
             "retry": {"max_attempts": 3, "backoff": {"initial": "1s", "cap": "5s", "multiplier": 2}, "jitter": "none"}}],
  "edges": []
}')"
note "retry run: $retry_id"

# Fails every attempt under a 2-attempt budget — one retries_exhausted
# dead-letter, one failed run.
doomed_id="$(submit '{
  "schema_version": 1, "name": "smoke-doomed",
  "steps": [{"id": "doomed", "type": "fail_n_times", "config": {"n": 9},
             "retry": {"max_attempts": 2, "backoff": {"initial": "1s", "cap": "5s", "multiplier": 2}, "jitter": "none"}}],
  "edges": []
}')"
note "dead-letter run: $doomed_id"

# Cost signal (ticket 10.5): a deterministic temperature=0 mock llm step run
# twice — the first spends (engine_cost_spent_usd_total + token counters), the
# second is a response-cache hit recording counterfactual savings
# (engine_cost_saved_usd_total).
cost_def='{
  "schema_version": 1, "name": "smoke-cost",
  "steps": [{"id": "gen", "type": "llm",
             "config": {"model": "mock/sim-1", "prompt": "summarize turtles", "temperature": 0}}],
  "edges": []
}'
cost_miss_id="$(submit "$cost_def")"
cost_hit_id="$(submit "$cost_def")"
note "cost runs: miss $cost_miss_id, hit $cost_hit_id"

# Output-quality signals (ticket 11.6): a mixed-quality validated workload on
# the offline mock. qual_pass clears a contains check (verdict pass); qual_fail
# fails a contains check under a 3-attempt semantic budget then dead-letters
# (verdict fail + semantic depth under validation_failed); qual_native's
# output_format json echoes native structured JSON (repair provenance native,
# passing the implicit schema); qual_unrep's repair_only prose is unrepairable
# and validation-fails. The unscripted mock cannot emit a parseable judge
# verdict, so the judge-score histogram is exercised by the engine integration
# test (TestQualityMetricsJudgeScore), not here.
qual_pass_id="$(submit '{
  "schema_version": 1, "name": "smoke-qual-pass",
  "steps": [{"id": "gen", "type": "llm",
             "config": {"model": "mock/sim-1", "prompt": "hello quality", "temperature": 0},
             "validation": {"validators": [{"name": "contains", "config": {"substring": "[mock]"}}]}}],
  "edges": []
}')"
qual_fail_id="$(submit '{
  "schema_version": 1, "name": "smoke-qual-fail",
  "steps": [{"id": "gen", "type": "llm",
             "config": {"model": "mock/sim-1", "prompt": "hello quality", "temperature": 0},
             "validation": {"validators": [{"name": "contains", "config": {"substring": "NEVER_APPEARS_ZZZ"}}],
                            "max_attempts": 3}}],
  "edges": []
}')"
qual_native_id="$(submit '{
  "schema_version": 1, "name": "smoke-qual-native",
  "steps": [{"id": "gen", "type": "llm",
             "config": {"model": "mock/sim-1", "prompt": "hello native", "temperature": 0,
                        "output_format": {"type": "json"}}}],
  "edges": []
}')"
qual_unrep_id="$(submit '{
  "schema_version": 1, "name": "smoke-qual-unrep",
  "steps": [{"id": "gen", "type": "llm",
             "config": {"model": "mock/sim-1", "prompt": "hello prose", "temperature": 0,
                        "output_format": {"type": "json", "mode": "repair_only"}}}],
  "edges": []
}')"
note "quality runs: pass $qual_pass_id, fail $qual_fail_id, native $qual_native_id, unrep $qual_unrep_id"

say "waiting for terminal states"
for id in "${fanout_ids[@]}"; do wait_terminal "$id" succeeded; done
wait_terminal "$retry_id" succeeded
wait_terminal "$doomed_id" failed
wait_terminal "$cost_miss_id" succeeded
wait_terminal "$cost_hit_id" succeeded
wait_terminal "$qual_pass_id" succeeded
wait_terminal "$qual_fail_id" failed
wait_terminal "$qual_native_id" succeeded
wait_terminal "$qual_unrep_id" failed
note "all runs terminal"

# Two scrape intervals (5s each) plus the 10s gauge-sample interval, so
# Prometheus holds fresh post-workload samples of everything.
say "letting Prometheus scrape (15s)"
sleep 15

# query <promql> — print the number of series in the instant vector.
query() {
  curl -fsS --get --data-urlencode "query=$1" "$PROM_URL/api/v1/query" |
    jq -r '.data.result | length'
}

# query_max <promql> — print the max sample value across the result, -1
# when empty.
query_max() {
  curl -fsS --get --data-urlencode "query=$1" "$PROM_URL/api/v1/query" |
    jq -r '[.data.result[].value[1] | tonumber] | max // -1'
}

failures=0
must_exist() {
  local q="$1"
  if [ "$(query "$q")" -gt 0 ]; then
    note "exists   ✓  $q"
  else
    printf '  MISSING  ✗  %s\n' "$q"; failures=$((failures + 1))
  fi
}
must_be_positive() {
  local q="$1" v
  v="$(query_max "$1")"
  if [ "$(jq -n --argjson v "$v" '$v > 0')" = "true" ]; then
    note "positive ✓  $q ($v)"
  else
    printf '  NOT >0   ✗  %s (%s)\n' "$q" "$v"; failures=$((failures + 1))
  fi
}

say "checking metric presence in Prometheus"
# Proof-of-life and gauges (sampled every 10s by each worker).
must_exist  'engine_build_info'
must_exist  'engine_queue_ready_depth'
must_exist  'engine_queue_stream_length'
must_exist  'engine_queue_pel_size'
must_exist  'engine_queue_delayed_depth'
must_exist  'engine_outbox_backlog'
must_exist  'engine_outbox_oldest_age_seconds'
must_be_positive 'engine_worker_active'
# Crash-path plain counters: registered at 0; movement is the chaos
# suite's business, presence is this script's.
must_exist  'engine_queue_reclaimed_total'
must_exist  'engine_queue_poison_total'
must_exist  'engine_step_takeovers_total'
must_exist  'engine_step_fencing_rejections_total'
must_exist  'engine_queue_promote_lag_seconds_count'
must_exist  'engine_dispatch_lag_seconds_count'
must_exist  'engine_step_scheduling_latency_seconds_count'
# Step-log capture counters (ticket 7.4; folded in by 7.5): plain
# counters, presence-checked — movement needs an executor that logs.
must_exist  'engine_steplog_captured_total'
must_exist  'engine_steplog_dropped_total'
must_exist  'engine_steplog_flush_failures_total'
# In-flight request gauge (ticket 7.5): registered at 0, sampled between
# requests, so presence is the assertion.
must_exist  'engine_api_requests_in_flight'

say "checking workload-driven values"
must_be_positive 'engine_step_claims_total{result="won"}'
must_be_positive 'engine_step_scheduling_latency_seconds_count'
must_be_positive 'engine_step_duration_seconds_count{outcome="succeeded"}'
must_be_positive 'engine_step_duration_seconds_count{outcome="transient"}'
must_be_positive 'engine_step_retries_total{class="transient"}'
must_be_positive 'engine_queue_promoted_total'
must_be_positive 'engine_step_dead_letters_total{source="retries_exhausted"}'
must_be_positive 'engine_run_duration_seconds_count{status="succeeded"}'
must_be_positive 'engine_run_duration_seconds_count{status="failed"}'
must_be_positive 'engine_dispatch_dispatched_total{reason="step_ready"}'
must_be_positive 'engine_api_requests_total{route="/v1/runs",method="POST",code="201"}'
must_be_positive 'engine_api_request_duration_seconds_count'
must_be_positive 'engine_api_ratelimit_decisions_total{decision="allowed"}'
# Cost metrics (ticket 10.5): the mock llm workload spends at the mock:*
# rate and the temperature=0 pair produces a cache hit that saves.
must_be_positive 'engine_cost_spent_usd_total{resource="mock:sim-1"}'
must_be_positive 'engine_cost_output_tokens_total{resource="mock:sim-1"}'
must_be_positive 'engine_cost_saved_usd_total{resource="mock:sim-1"}'
# Output-quality metrics (ticket 11.6): the mixed-quality workload drives
# verdict pass/fail, per-validator results, the semantic-depth histogram under
# both terminal outcomes, and native/unrepairable repair provenance.
must_be_positive 'engine_validate_verdicts_total{status="pass"}'
must_be_positive 'engine_validate_verdicts_total{status="fail"}'
must_be_positive 'engine_validate_validator_results_total{validator="contains",status="pass"}'
must_be_positive 'engine_validate_validator_results_total{validator="contains",status="fail"}'
must_be_positive 'engine_validate_semantic_depth_attempts_count{outcome="succeeded"}'
must_be_positive 'engine_validate_semantic_depth_attempts_count{outcome="validation_failed"}'
must_be_positive 'engine_validate_repairs_total{status="native"}'
must_be_positive 'engine_validate_repairs_total{status="unrepairable"}'

if [ "$failures" -gt 0 ]; then
  fail "$failures metric check(s) failed"
fi
say "all metric checks passed"
