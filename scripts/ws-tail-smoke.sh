#!/usr/bin/env bash
# ws-tail-smoke — the 16.5 DoD-2 check: the typed TS client tails a live compose
# run correctly through a forced reconnect.
#
# Boots the full app stack, submits a workflow with a ~15s span, tails it from
# the Node example (web/lib/engine-client/examples/tail-run.ts), restarts the
# api container mid-run to force a WS reconnect (the client re-mints a ticket
# and resumes from its cursor), waits for the tailer to reach a terminal event,
# and asserts: the tailer exited 0, its received seqs are exactly 1..max with no
# gaps or dupes, max == max(seq) in the durable events table, and it reconnected
# at least once. Run locally (needs docker + node + pnpm); not a CI job — the
# CI twin of the resume guarantee is the Go TestWSSnapshotBackfillResume test.
#
# Prerequisites: docker compose, go, curl, jq, node, corepack (pnpm).
set -euo pipefail

cd "$(dirname "$0")/.."

if [ -f .env ]; then
  set -a
  # shellcheck disable=SC1091
  . ./.env
  set +a
fi

# The client mints a WS ticket per (re)connect; a shared secret keeps a ticket
# valid across the api restart (an empty secret mints a fresh per-process one,
# which is exactly what a mid-stream restart should tolerate — the client just
# re-mints). Set one so both boots agree.
export AGENTLOOM_API_WS_TICKET_SECRET="${AGENTLOOM_API_WS_TICKET_SECRET:-ws-smoke-secret}"

if [ -z "${AGENTLOOM_API_ROOT_KEY:-}" ]; then
  AGENTLOOM_API_ROOT_KEY="sk_$(head -c 32 /dev/urandom | base64 | tr '+/' '-_' | tr -d '=')"
  export AGENTLOOM_API_ROOT_KEY
fi
API_KEY="$AGENTLOOM_API_ROOT_KEY"
API_URL="http://localhost:${AGENTLOOM_API_PORT:-8080}"

bold=$'\033[1m' dim=$'\033[2m' reset=$'\033[0m'
say()  { printf '\n%s▶ %s%s\n' "$bold" "$*" "$reset"; }
note() { printf '%s  %s%s\n' "$dim" "$*" "$reset"; }
fail() { printf 'ws-tail-smoke: %s\n' "$*" >&2; exit 1; }

for dep in docker go curl jq node corepack; do
  command -v "$dep" >/dev/null || fail "missing dependency: $dep"
done

say "booting the app stack (api + 2 workers)"
note "docker compose --profile app up -d --build --wait"
docker compose --profile app up -d --build --wait

say "installing the web workspace"
( cd web && corepack pnpm install --frozen-lockfile )

# A definition with a couple of sleeps so the run lasts long enough to survive an
# api restart mid-stream. Uses only offline built-ins (no provider keys needed).
DEF=$(cat <<'JSON'
{
  "schema_version": 1,
  "name": "ws_tail_smoke",
  "steps": [
    { "id": "a", "type": "sleep", "config": { "duration": "6s" } },
    { "id": "b", "type": "sleep", "config": { "duration": "6s" } },
    { "id": "c", "type": "echo", "config": { "value": "done" } }
  ],
  "edges": [
    { "from": "a", "to": "b" },
    { "from": "b", "to": "c" }
  ]
}
JSON
)

say "submitting the run"
RUN=$(curl -fsS -X POST "$API_URL/v1/runs" \
  -H "Authorization: Bearer $API_KEY" -H "Content-Type: application/json" \
  -d "{\"definition\": $DEF}" | jq -r '.run.id')
[ -n "$RUN" ] && [ "$RUN" != "null" ] || fail "submit failed"
note "run id: $RUN"

say "tailing with the typed TS client (Node example)"
OUT=$(mktemp)
( cd web/lib/engine-client && AGENTLOOM_API_KEY="$API_KEY" \
    corepack pnpm exec tsx examples/tail-run.ts --api "$API_URL" --run "$RUN" --timeout-ms 90000 ) \
  >"$OUT" 2>/tmp/ws-tail-smoke.err &
TAILER=$!

# Force a reconnect: restart the api container a few seconds into the run.
sleep 4
say "restarting the api container mid-run (forces a WS reconnect)"
docker compose restart api >/dev/null
docker compose --profile app up -d --wait >/dev/null

wait "$TAILER" && TAILER_RC=0 || TAILER_RC=$?

say "tailer output"
cat "$OUT"

SUMMARY=$(grep '"frame":"summary"' "$OUT" | tail -1)
[ -n "$SUMMARY" ] || fail "no summary line from the tailer"

OK=$(jq -r '.ok' <<<"$SUMMARY")
RECV=$(jq -r '.received' <<<"$SUMMARY")
LAST=$(jq -r '.last_seq' <<<"$SUMMARY")
RECONN=$(jq -r '.reconnects' <<<"$SUMMARY")

# Ground truth: the durable max(seq) for the run. Runs inside the postgres
# container so it reads that container's own POSTGRES_USER/POSTGRES_DB env.
DBMAX=$(docker compose exec -T postgres sh -c \
  "exec psql -U \"\$POSTGRES_USER\" -d \"\$POSTGRES_DB\" -tAc \"SELECT COALESCE(MAX(seq),0) FROM events WHERE run_id='$RUN'\"")
DBMAX=$(echo "$DBMAX" | tr -d '[:space:]')

say "assertions"
note "tailer rc=$TAILER_RC ok=$OK received=$RECV last_seq=$LAST reconnects=$RECONN db_max_seq=$DBMAX"
[ "$TAILER_RC" -eq 0 ]      || fail "tailer exited non-zero ($TAILER_RC) — gap or duplicate detected"
[ "$OK" = "true" ]         || fail "tailer reported a gap/dup"
[ "$LAST" = "$DBMAX" ]     || fail "tailer last_seq ($LAST) != durable max seq ($DBMAX)"
[ "$RECONN" -ge 1 ]        || fail "expected at least one reconnect, got $RECONN"

printf '\n%s✔ ws-tail-smoke passed: %s events, no gaps/dupes, %s reconnect(s), last_seq==db_max==%s%s\n' \
  "$bold" "$RECV" "$RECONN" "$DBMAX" "$reset"
