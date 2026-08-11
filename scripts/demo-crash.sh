#!/usr/bin/env bash
# demo-crash — the flagship crash-recovery demo (ticket 4.7).
#
# Act 1: boot the full compose stack (api + 2 workers) with a shortened
# lease TTL, submit a workflow whose long step sleeps 25s, find the worker
# holding that step's lease via the Redis PEL, and SIGKILL its container
# mid-step. Narrates the reclaim: the surviving worker's XAUTOCLAIM picks
# up the orphaned entry after lease expiry, takes over the step under a
# fresh claim, re-executes it, and completes the run — attempt history
# reads lost → succeeded.
#
# Act 2: submit again and SIGKILL the ENTIRE stack (api + every worker)
# mid-run, then boot it back up: the run resumes from the last completed
# step — pre-crash steps keep their single attempt.
#
# Kills are SIGKILL on purpose: a graceful stop (SIGTERM) drains the
# in-flight handler, which is the orderly-deploy path, not a crash. The
# automated CI twin of this demo is test/crash (make test-integration).
#
# Prerequisites: docker compose, go, curl, jq. Assumes an otherwise idle
# stack (the holder lookup expects the demo's step to be the only pending
# queue entry). See docs/demos/crash-recovery.md for the annotated tour.
set -euo pipefail

cd "$(dirname "$0")/.."

# Load .env the way the Makefile and Compose do, so ports and credentials
# match the running stack.
if [ -f .env ]; then
  set -a
  # shellcheck disable=SC1091
  . ./.env
  set +a
fi

# 5s lease (default 30s) keeps the wait-for-reclaim act watchable. The
# compose file passes this through to the workers; heartbeat and reclaim
# intervals derive from it.
export AGENTLOOM_QUEUE_LEASE_TTL="${DEMO_LEASE_TTL:-5s}"

API_URL="http://localhost:${AGENTLOOM_API_PORT:-8080}"
FIXTURE=docs/demos/crash-demo.json

bold=$'\033[1m' dim=$'\033[2m' reset=$'\033[0m'
say()  { printf '\n%s▶ %s%s\n' "$bold" "$*" "$reset"; }
note() { printf '%s  %s%s\n' "$dim" "$*" "$reset"; }
fail() { printf 'demo-crash: %s\n' "$*" >&2; exit 1; }

for dep in docker go curl jq; do
  command -v "$dep" >/dev/null || fail "missing dependency: $dep"
done

run_json()    { curl -fsS "$API_URL/v1/runs/$1"; }
run_status()  { run_json "$1" | jq -r '.run.status'; }
step_status() { run_json "$1" | jq -r --arg s "$2" '.steps[] | select(.id==$s) | .status'; }

# attempt_table renders every step's attempt history — the demo's receipt.
attempt_table() {
  printf '    %-12s %-8s %-11s %s\n' STEP ATTEMPT OUTCOME CLAIM
  run_json "$1" | jq -r '.steps[] | .id as $id | .attempts[]?
      | [$id, (.attempt|tostring), (.outcome // "in-flight"), .claim_id] | @tsv' |
    awk -F'\t' '{printf "    %-12s %-8s %-11s %s\n", $1, $2, $3, $4}'
}

# wait_for <desc> <fn> — poll fn (a shell function returning 0 when
# satisfied) every half second, up to 2 minutes.
wait_for() {
  local desc="$1" fn="$2" deadline=$((SECONDS + 120))
  until "$fn"; do
    ((SECONDS < deadline)) || fail "timed out waiting for $desc"
    sleep 0.5
  done
}

# lease_holder prints the consumer name holding the (sole) pending entry
# on the fleet stream — the worker mid-step right now. --no-raw forces the
# numbered reply format even without a TTY, so the parse is stable. The
# stream/group names honor the 4.7 env knobs (this script sources .env,
# so an override there must not silently break the lookup).
QUEUE_STREAM="${AGENTLOOM_QUEUE_STREAM:-steps:ready}"
QUEUE_GROUP="${AGENTLOOM_QUEUE_GROUP:-workers}"
lease_holder() {
  docker compose exec -T redis redis-cli --no-raw XPENDING "$QUEUE_STREAM" "$QUEUE_GROUP" - + 10 |
    sed -n 's/^ *2) "\(.*\)"$/\1/p' | head -1
}

# victim_container prints the container ID of the worker whose logs carry
# the given consumer name (worker_id == consumer name, logged at startup).
victim_container() {
  local cid
  for cid in $(docker compose ps -q worker); do
    if docker logs "$cid" 2>&1 | grep -q "$1"; then
      echo "$cid"
      return
    fi
  done
}

submit() { go run ./cmd/ctl --api "$API_URL" submit "$FIXTURE"; }
watch()  { go run ./cmd/ctl --api "$API_URL" watch "$1"; }

# ---------------------------------------------------------------- Act 0
say "Act 0 — booting the full stack (api + 2 workers), lease TTL $AGENTLOOM_QUEUE_LEASE_TTL"
note "docker compose --profile app up -d --build --wait"
docker compose --profile app up -d --build --wait

# ---------------------------------------------------------------- Act 1
say "Act 1 — SIGKILL the worker holding a lease mid-step"
RUN_ID=$(submit)
note "submitted $FIXTURE as run $RUN_ID"
note "waiting for the 25s long_task to start executing…"

long_task_running() { [ "$(step_status "$RUN_ID" long_task)" = running ]; }
wait_for "long_task to be running" long_task_running

HOLDER=""
holder_known() { HOLDER=$(lease_holder); [ -n "$HOLDER" ]; }
wait_for "the lease holder to appear in the PEL" holder_known
VICTIM=$(victim_container "$HOLDER")
[ -n "$VICTIM" ] || fail "no worker container's logs carry consumer $HOLDER"
note "consumer $HOLDER holds the lease (container ${VICTIM:0:12})"

say "SIGKILLing it — no drain, no ACK; its heartbeats just stop"
docker kill -s KILL "$VICTIM" >/dev/null
note "the step is still 'running' in Postgres under the dead worker's claim"
note "after the $AGENTLOOM_QUEUE_LEASE_TTL lease expires, the survivor's XAUTOCLAIM reclaims the entry…"

reclaimed() { [ "$(run_json "$RUN_ID" | jq -r '.steps[] | select(.id=="long_task") | .attempt_count')" = 2 ]; }
wait_for "the reclaim (attempt 2 on long_task)" reclaimed
note "reclaimed! attempt 1 closed as 'lost'; attempt 2 runs under a fresh claim (the dead worker's fence is void)"

say "Watching the run to completion"
watch "$RUN_ID"

say "The receipt — attempt history of run $RUN_ID"
attempt_table "$RUN_ID"

outcomes=$(run_json "$RUN_ID" | jq -r '.steps[] | select(.id=="long_task") | [.attempts[].outcome] | join(",")')
[ "$outcomes" = "lost,succeeded" ] || fail "long_task attempt history is '$outcomes', expected 'lost,succeeded'"
reruns=$(run_json "$RUN_ID" | jq '[.steps[] | select(.id != "long_task") | .attempt_count] | map(select(. != 1)) | length')
[ "$reruns" = 0 ] || fail "a step other than long_task has more than one attempt — completed work was re-run"
note "✓ run succeeded; the crashed step reads lost → succeeded; no other step ran twice"

say "Restoring the killed worker"
docker compose --profile app up -d --wait

# ---------------------------------------------------------------- Act 2
say "Act 2 — full-stack restart mid-run (SIGKILL api + every worker)"
RUN2=$(submit)
note "submitted a second run: $RUN2"
note "letting it make real progress first (short_task done, long_task executing)…"

progressed() {
  [ "$(step_status "$RUN2" short_task)" = succeeded ] &&
    [ "$(step_status "$RUN2" long_task)" = running ]
}
wait_for "short_task done and long_task running" progressed

note "killing every deployable at once (stores stay up — Postgres is the source of truth)"
# shellcheck disable=SC2046 — word splitting over container IDs is the point
docker kill -s KILL $(docker compose ps -q worker) $(docker compose ps -q api) >/dev/null
note "nothing is alive to make progress; the run is frozen mid-long_task"

say "Booting the stack back up"
docker compose --profile app up -d --wait
note "fresh workers (new consumer names) reclaim the dead fleet's entries and resume the run"

say "Watching the resumed run to completion"
watch "$RUN2"

say "The receipt — attempt history of run $RUN2"
attempt_table "$RUN2"

for step in warm_up short_task; do
  n=$(run_json "$RUN2" | jq -r --arg s "$step" '.steps[] | select(.id==$s) | .attempt_count')
  [ "$n" = 1 ] || fail "$step has $n attempts after the restart — pre-crash work was re-run"
done
[ "$(run_status "$RUN2")" = succeeded ] || fail "run $RUN2 did not succeed"
note "✓ resumed from the last completed step: pre-crash steps kept their single attempt"

say "Demo complete"
note "both runs succeeded across a worker SIGKILL and a full-stack restart;"
note "recovery was reclaim → takeover → fresh claim, with zombie writes fenced by claim_id."
note "The automated CI twin lives in test/crash (make test-integration)."
