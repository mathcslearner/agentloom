#!/usr/bin/env bash
# trace-smoke — ticket 7.3's acceptance script: boot the compose stack
# with the obs profile (2 worker replicas), submit one run whose shape
# forces both a retry and enough parallel steps to spread across the
# worker fleet, then interrogate Jaeger for the single run trace:
#
#   - every step.attempt span for the run shares ONE trace id, rooted at
#     the API's POST /v1/runs server span;
#   - the trace's worker spans come from >= 2 distinct worker processes
#     (service.instance.id), proving cross-process propagation through
#     the queue;
#   - the retry attempt references the failed attempt as a span LINK
#     (Jaeger renders OTel links as FOLLOWS_FROM references), never as a
#     parent.
#
# Prerequisites: docker compose, curl, jq. Run via `make smoke-trace`.
set -euo pipefail

cd "$(dirname "$0")/.."

if [ -f .env ]; then
  set -a
  # shellcheck disable=SC1091
  . ./.env
  set +a
fi

if [ -z "${AGENTLOOM_API_ROOT_KEY:-}" ]; then
  AGENTLOOM_API_ROOT_KEY="sk_$(head -c 32 /dev/urandom | base64 | tr '+/' '-_' | tr -d '=')"
  export AGENTLOOM_API_ROOT_KEY
fi
API_KEY="$AGENTLOOM_API_ROOT_KEY"
API_URL="http://localhost:${AGENTLOOM_API_PORT:-8080}"
JAEGER_URL="http://localhost:${AGENTLOOM_JAEGER_PORT:-16686}"

bold=$'\033[1m' dim=$'\033[2m' reset=$'\033[0m'
say()  { printf '\n%s▶ %s%s\n' "$bold" "$*" "$reset"; }
note() { printf '%s  %s%s\n' "$dim" "$*" "$reset"; }
fail() { printf 'trace-smoke: %s\n' "$*" >&2; exit 1; }

for dep in docker curl jq; do
  command -v "$dep" >/dev/null || fail "missing dependency: $dep"
done

say "booting compose app+obs profiles (idempotent)"
AGENTLOOM_OBS_OTEL_ENABLED=true docker compose --profile app --profile obs up -d --build --wait

# One run: a flaky entry (one transient failure -> one retry link), then a
# four-way fan-out so the two worker replicas both claim work.
run_id="$(jq -n '{definition: {
  schema_version: 1, name: "smoke-trace",
  steps: ([{id: "flaky", type: "fail_n_times", config: {n: 1},
            retry: {max_attempts: 3, backoff: {initial: "1s", cap: "5s", multiplier: 2}, jitter: "none"}}]
          + [range(4) | {id: "fan\(.)", type: "noop"}]),
  edges: [range(4) | {from: "flaky", to: "fan\(.)"}]
}}' |
  curl -fsS -X POST -H "Authorization: Bearer $API_KEY" -H "Content-Type: application/json" \
    -d @- "$API_URL/v1/runs" | jq -r '.run_id')"
say "submitted run $run_id"

deadline=$((SECONDS + 120))
while :; do
  status="$(curl -fsS -H "Authorization: Bearer $API_KEY" "$API_URL/v1/runs/$run_id" | jq -r '.run.status')"
  [ "$status" = "succeeded" ] && break
  case "$status" in failed|cancelled) fail "run ended $status, want succeeded" ;; esac
  ((SECONDS < deadline)) || fail "run stuck in $status"
  sleep 0.5
done
note "run succeeded"

# The batch span processor exports on a cadence; poll Jaeger until the
# run's attempt spans are all indexed (6 attempts: flaky x2 + fan0..3).
say "querying Jaeger for the run trace"
trace=""
deadline=$((SECONDS + 60))
while :; do
  trace="$(curl -fsS --get \
    --data-urlencode "service=agentloom-worker" \
    --data-urlencode "operation=step.attempt" \
    --data-urlencode "tags={\"run_id\":\"$run_id\"}" \
    --data-urlencode "limit=20" \
    "$JAEGER_URL/api/traces")"
  attempts="$(jq '[.data[].spans[] | select(.operationName == "step.attempt")
                   | select(any(.tags[]; .key == "run_id" and .value == "'"$run_id"'"))] | length' <<<"$trace")"
  [ "$attempts" -ge 6 ] && break
  ((SECONDS < deadline)) || fail "Jaeger indexed only $attempts/6 attempt spans for the run"
  sleep 2
done
note "$attempts step.attempt spans indexed"

failures=0
check() {
  local desc="$1" ok="$2"
  if [ "$ok" = "true" ]; then
    note "✓  $desc"
  else
    printf '  ✗  %s\n' "$desc"; failures=$((failures + 1))
  fi
}

# One trace: every result row for this run's spans shares one trace id.
trace_count="$(jq '[.data[].traceID] | unique | length' <<<"$trace")"
check "all run spans share one trace (got $trace_count)" "$(jq -n --argjson n "$trace_count" '$n == 1')"

# The trace includes the API's server span — the submission root.
has_api="$(jq '[.data[0].processes[] | select(.serviceName == "agentloom-api")] | length > 0' <<<"$trace")"
check "trace includes the agentloom-api submission span" "$has_api"

# Cross-process: >= 2 distinct worker instances contributed spans.
workers="$(jq '[.data[0].processes[] | select(.serviceName == "agentloom-worker")
                | (.tags[]? | select(.key == "service.instance.id") | .value)] | unique | length' <<<"$trace")"
check "spans from >= 2 worker processes (got $workers)" "$(jq -n --argjson n "$workers" '$n >= 2')"

# The retry link: the flaky step's attempt 2 carries a FOLLOWS_FROM
# reference (an OTel span link) to attempt 1 — and is not its child.
retry_link="$(jq '
  [.data[0].spans[] | select(.operationName == "step.attempt")
   | select(any(.tags[]; .key == "step_id" and .value == "flaky"))] as $flaky
  | ($flaky[] | select(any(.tags[]; .key == "attempt" and (.value == 1 or .value == "1"))) | .spanID) as $a1
  | ($flaky[] | select(any(.tags[]; .key == "attempt" and (.value == 2 or .value == "2")))) as $a2
  | any($a2.references[]?; .refType == "FOLLOWS_FROM" and .spanID == $a1)
    and (any($a2.references[]?; .refType == "CHILD_OF" and .spanID == $a1) | not)
' <<<"$trace")"
check "retry attempt links (FOLLOWS_FROM, not CHILD_OF) to the failed attempt" "$retry_link"

[ "$failures" -eq 0 ] || fail "$failures assertion(s) failed"
say "trace-smoke green: one trace, $workers worker processes, retry link present"
note "open it: $JAEGER_URL/trace/$(jq -r '.data[0].traceID' <<<"$trace")"
