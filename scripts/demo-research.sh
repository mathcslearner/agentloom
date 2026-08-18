#!/usr/bin/env bash
# demo-research — the flagship research → write → critique demo (ticket 14.5).
#
# Boots the full compose stack (api + workers) with a SCRIPTED mock provider
# (docs/examples/research-critic-writer.mock.json) so the writer⇄critic loop
# actually iterates offline — the stock echo mock never returns a "revise"
# verdict, so a scripted critic is what makes real loop unrolling visible.
# Seeds a small retrieval corpus, submits examples/definitions/
# research-critic-writer.json, watches it converge, and prints the receipts a
# newcomer wants to see: the handoff thread, the draft revision history, the
# cost breakdown (with judge overhead), and the runtime graph expansions.
#
# The automated CI twin is TestFlagshipResearchCriticWriter in internal/engine
# (make test-integration). Live mode (real providers) is documented in
# docs/examples/research-critic-writer.md and gated by LIVE_LLM_TESTS=1.
#
# Prerequisites: docker compose, go, curl, jq. Assumes an otherwise idle stack.
set -euo pipefail

cd "$(dirname "$0")/.."

if [ -f .env ]; then
  set -a
  # shellcheck disable=SC1091
  . ./.env
  set +a
fi

# The scripted mock: the compose worker's mock is echo-only by default, so we
# hand it a script that makes the critic reject twice then approve and the
# judge fail the first draft then pass. Compose passes AGENTLOOM_LLM_MOCK_SCRIPT
# through to the worker (docker-compose.yml).
AGENTLOOM_LLM_MOCK_SCRIPT="$(cat docs/examples/research-critic-writer.mock.json)"
export AGENTLOOM_LLM_MOCK_SCRIPT

# Every /v1 route requires a scoped bearer key. Reuse the stack's root
# credential from .env, or mint an ephemeral one (constructed, never a
# committed literal — the CI secret grep forbids those).
if [ -z "${AGENTLOOM_API_ROOT_KEY:-}" ]; then
  AGENTLOOM_API_ROOT_KEY="sk_$(head -c 32 /dev/urandom | base64 | tr '+/' '-_' | tr -d '=')"
  export AGENTLOOM_API_ROOT_KEY
  note_root_key=" (ephemeral root key minted for this demo)"
else
  note_root_key=" (root key from .env)"
fi
API_KEY="$AGENTLOOM_API_ROOT_KEY"

API_URL="http://localhost:${AGENTLOOM_API_PORT:-8080}"
FIXTURE=examples/definitions/research-critic-writer.json
TOPIC="${DEMO_TOPIC:-sea turtles}"

bold=$'\033[1m' dim=$'\033[2m' reset=$'\033[0m'
say()  { printf '\n%s▶ %s%s\n' "$bold" "$*" "$reset"; }
note() { printf '%s  %s%s\n' "$dim" "$*" "$reset"; }
fail() { printf 'demo-research: %s\n' "$*" >&2; exit 1; }

for dep in docker go curl jq; do
  command -v "$dep" >/dev/null || fail "missing dependency: $dep"
done

run_json()   { curl -fsS -H "Authorization: Bearer $API_KEY" "$API_URL/v1/runs/$1"; }
run_status() { run_json "$1" | jq -r '.run.status'; }
cost_json()  { curl -fsS -H "Authorization: Bearer $API_KEY" "$API_URL/v1/runs/$1/cost"; }
graph_json() { curl -fsS -H "Authorization: Bearer $API_KEY" "$API_URL/v1/runs/$1/graph"; }
ctl()        { go run ./cmd/ctl --api "$API_URL" --key "$API_KEY" "$@"; }
psql_q()     { docker compose exec -T postgres psql -qtAX -U "${POSTGRES_USER:-agentloom}" -d "${POSTGRES_DB:-agentloom}" -c "$1"; }

# ---------------------------------------------------------------- Act 0
# A single worker: the scripted mock's per-rule response SEQUENCE (reject,
# reject, approve) is per worker PROCESS, so a multi-worker fleet would split
# the sequence across processes and the loop would iterate nondeterministically.
# One worker keeps the scripted narrative deterministic. (The engine is still
# fully distributed — the crash demo, make demo-crash, exercises the fleet; the
# CI twin of THIS example, TestFlagshipResearchCriticWriter, shares one mock
# instance across two workers to stay deterministic.)
say "Act 0 — booting the stack (1 worker) with a scripted mock$note_root_key"
note "docker compose --profile app up -d --build --wait --scale worker=1"
docker compose --profile app up -d --build --wait --scale worker=1

say "Seeding the retrieval corpus (pg_fulltext has no ingest API — we insert directly)"
psql_q "
  INSERT INTO retrieval_docs (id, content) VALUES
   ('sea-turtles', 'Sea turtles are ancient marine reptiles that migrate thousands of miles across oceans between feeding and nesting grounds.'),
   ('turtle-nesting', 'Female sea turtles return to the beaches where they hatched to lay their eggs, navigating by the Earth''s magnetic field.'),
   ('turtle-threats', 'Sea turtle populations are threatened by plastic pollution, bycatch in fisheries, and warming sand temperatures that skew hatchling sex ratios.'),
   ('tortoises', 'Tortoises are land-dwelling turtles known for their longevity and are not strong swimmers.')
  ON CONFLICT (id) DO UPDATE SET content = EXCLUDED.content;" >/dev/null
note "seeded 4 documents"

# ---------------------------------------------------------------- Act 1
say "Act 1 — submit the flagship example (topic: \"$TOPIC\")"
RUN_ID=$(ctl submit "$FIXTURE" --params "$(jq -nc --arg t "$TOPIC" '{topic:$t}')")
note "submitted as run $RUN_ID"

say "Watching it converge (researcher → writer ⇄ critic → editor → publish)"
ctl watch "$RUN_ID" || fail "run $RUN_ID did not succeed"

# ---------------------------------------------------------------- Receipts
say "The handoff thread — every agent turn, oldest first (ticket 14.2)"
ctl blackboard "$RUN_ID" --name thread --history || true

say "The draft revision history — one blackboard version per writer instance"
ctl blackboard "$RUN_ID" --name draft --history || true

say "Runtime graph — the loop unrolled into {id}#k instances (ticket 14.3/13.6)"
graph_json "$RUN_ID" | jq '{graph_version, nodes: [.nodes[] | {id, origin: .origin.kind}], expansions: [.expansions[] | {from_version, version, origin_step, added_steps}]}'

say "Cost breakdown — judge calls appear as overhead (ticket 11.5/ADR-012 rule 4)"
cost_json "$RUN_ID" | jq '{spent_usd: .summary.spent_usd, spent_nano_usd: .summary.spent_nano_usd, by_step: [.by_step[] | {step_id, spent_nano_usd, overhead_nano_usd}], judge_overhead: [.entries[] | select(.overhead) | {step_id, entry, resource, spent_nano_usd}]}'

say "Event highlights (from the append-only log)"
psql_q "SELECT lpad(seq::text,3,' ') || '  ' || type
        FROM events WHERE run_id = '$RUN_ID'
          AND type IN ('graph_expanded','step_semantic_retry_scheduled','step_dead_lettered','loop_exhausted','loop_no_progress','guard_tripped','run_succeeded')
        ORDER BY seq;"

# ---------------------------------------------------------------- Receipt
say "The receipt"
gv=$(graph_json "$RUN_ID" | jq -r '.graph_version')
[ "$(run_status "$RUN_ID")" = succeeded ] || fail "run did not succeed"
[ "$gv" = 3 ] || fail "graph_version is $gv, expected 3 (two loop iterations)"
retries=$(psql_q "SELECT count(*) FROM events WHERE run_id='$RUN_ID' AND type='step_semantic_retry_scheduled';")
note "✓ run succeeded at graph_version $gv (writer⇄critic loop unrolled twice)"
note "✓ $retries semantic retry scheduled (the judge sent the first draft back with a critique)"
note "✓ the judge's provider calls are metered as overhead on the writer step"
note "Full narrative: docs/examples/research-critic-writer.md"
