# Canonical example definitions

The canonical workflow definition corpus (ticket 1.6). Each file is a
complete, valid `schema_version: 1` document per
[ADR-003](../../docs/adr/003-workflow-definition-format.md); the CEL
environment for `when`/`condition` predicates is documented in
[docs/expressions.md](../../docs/expressions.md).

These files are **golden fixtures**, not just documentation: the
`internal/dag` test suite decodes, validates, and round-trips every file
here (and pins the exact file list), later engine tests submit them as
runs, and the visual builder's serialization round-trip suite (M17)
consumes them directly. If you edit one, `go test ./internal/dag` must
stay green.

JSON has no comments and the decoder strictly rejects unknown fields, so
each example's "header comment" is its top-level `description` field.

## The examples

- **[linear.json](linear.json)** — the smallest realistic pipeline: fetch
  (tool) → summarize (llm) → store (echo) in a straight chain. Start here
  to learn the top-level shape: `schema_version`, `params`, `steps`,
  `edges` — plus an explicit `retry` policy on the network-bound fetch
  step ([ADR-006](../../docs/adr/006-failure-taxonomy-and-retries.md));
  the other steps inherit the engine's default policy.
- **[fanout.json](fanout.json)** — parallel fan-out/fan-in: one entry step
  with three unconditioned out-edges (all three successors run in
  parallel) converging on a `join` with `mode: all` before a synthesis
  step. Its `llm` steps use the offline mock (`mock/sim-1`, like
  `mock_pipeline.json`) and its `tool`/`retrieve` steps are the offline
  built-ins (`json_transform`, `pg_fulltext` over an empty corpus), so the
  whole fixture runs end-to-end with no API key.
- **[conditional_branch.json](conditional_branch.json)** — exclusive
  routing: a classifier feeds a `branch` step whose out-edges fire
  first-match-in-declaration-order — two `when`-conditioned arms plus a
  trailing default. The arms reconverge on a `join all`; the un-taken arms
  are *skipped*, and skipped parents satisfy the join (skip propagation).
- **[critic_loop.json](critic_loop.json)** — bounded writer↔critic
  refinement via a marked **loop edge** (`type: loop`, `condition`,
  `max_iterations`) — the only sanctioned kind of cycle, executed by
  unrolling (M14) — plus a conditioned exit edge.
- **[echo_pipeline.json](echo_pipeline.json)** — step input templating
  (ticket 8.2): a three-step echo chain where every value flows through
  `${{ ... }}` expressions — run params (whole objects preserved as
  JSON), nested output paths, string interpolation, a multi-hop
  reference two steps upstream, and the full restricted function set
  (`get`, `default`, `toJson`, `truncate`). Echo steps output exactly
  their rendered input, so each output is a probe of what rendering
  resolved; the engine integration suite executes this file end-to-end.
- **[mock_pipeline.json](mock_pipeline.json)** — two chained `llm` steps
  passing data through templating on the deterministic offline mock
  provider (ticket 8.6): a draft turns a param into a completion, a refine
  feeds it back into a second call via `${{ steps.draft.output.text }}`.
  The workhorse offline `llm` fixture.
- **[rag_lite.json](rag_lite.json)** — retrieval-augmented generation
  (ticket 8.8): a `retrieve` step queries the reference `pg_fulltext`
  retriever for a run param, then an `llm` step (offline mock) answers
  grounded in the retrieved documents and cites them by id. Proves
  retrieve → llm data flow — `${{ steps.search.output.results }}` splices
  the ranked `{id, content, score, metadata}` results into the prompt. The
  engine integration suite seeds a corpus and executes this file
  end-to-end.
- **[blackboard.json](blackboard.json)** — the run-scoped blackboard
  (ticket 12.2, ADR-014): a `draft` llm step (offline mock) publishes its
  completion to the blackboard declaratively (`blackboard.write`, `pinned`,
  `from: /text`), a parallel `blackboard_write` step records a plan, and a
  downstream `blackboard_write` revises the plan under a version
  compare-and-swap (`expected_version`) and reads the pinned draft back.
  Every entry is token-counted and retained for audit; inspect with
  `ctl blackboard <run-id>`. Runs offline on the mock.
- **[planner.json](planner.json)** — dynamic DAG expansion (ticket 13.3,
  ADR-015): a `planner` step is an `llm` call whose validated `PlanOutput`
  is spliced into the running graph atomically with its completion
  (`store.ExpandRun`). The plan injects two `llm` worker steps that depend on
  the planner (an *after* splice) and fan into a pre-existing `gather` join
  (a *before* splice, widening the join); the injected steps then execute
  like any authored step and the join continues. The plan is gated by an
  implicit `json_schema` validator over `plan-output.v1.json` (a malformed
  plan semantic-retries with the rejection issues as feedback,
  `max_attempts` 3) and by the run's `expansion` caps. Runs offline on the
  mock — the planner's prompt *is* a valid `PlanOutput`, echoed back
  verbatim.
- **[map_fanout.json](map_fanout.json)** — dynamic map fan-out (ticket 13.4,
  ADR-015): a `map` step instantiates one instance of its `body` sub-template
  (the `templates` library) per runtime list item plus a generated `gather`
  join that collects the ordered per-instance results — an engine-generated
  expansion (no LLM), applied through the same `store.ExpandRun` as a planner.
  `source` emits a three-element list, `process` maps `analyze_one` over it
  (each an `llm` call referencing its item through `${{ item }}` /
  `${{ item_index }}`), and `process#gather` emits the ordered array of
  analyses. `max_items` caps the width. Runs offline on the mock.
- **[agent_handoff.json](agent_handoff.json)** — two-agent relay over the
  blackboard handoff thread (ticket 14.2, ADR-016): a `researcher` agent's
  turn is auto-appended to the run `thread` (author/role/iteration metadata)
  and to a pinned `handoff` payload; the `writer` agent's role carries a
  "conversation view" context preset (a `thread` context source + the pinned
  handoff), so the researcher's findings reach the writer automatically —
  the writer's task never names the topic, yet its output carries it. Runs
  offline on the mock (the mock echoes the assembled context).
- **[research-critic-writer.json](research-critic-writer.json)** — the M14
  flagship (ticket 14.5, ADR-016): a `researcher` (retrieve → agent) → `writer`
  → `critic` refinement loop → `editor` → `publish` pipeline that composes
  nearly every AI-native feature at once — the blackboard handoff thread
  (14.2), loop unrolling (14.3), a cost-bearing `llm_judge` with semantic-retry
  feedback (11.4/11.5), `model_fallbacks` (10.4) under a run `budget_usd`
  (10.3), a `context` preset with `summarize` compaction under `budget_tokens`
  (12.4/12.5), and run guards incl. an opt-in `no_progress` detector (14.4).
  Runs offline on the mock (loop iterations need the env-scripted mock — see
  [docs/examples/research-critic-writer.md](../../docs/examples/research-critic-writer.md)
  and `make demo-research`); swap the `mock/*` model ids for real providers to
  run it live.
- **[kitchen_sink.json](kitchen_sink.json)** — one coherent
  research-and-publish pipeline exercising every construct: every registered
  step type (including a `map` over a `templates` sub-template and a `gather`),
  both join modes (`any` and `all`), conditioned and unconditioned
  edges, `has()` guards, a branch with a trailing default, a loop edge,
  all five param types, explicit retry policies (one full block, one
  partial block inheriting engine defaults) with the
  `continue_independent_branches` failure policy (ADR-006), a per-step
  execution `timeout` (ticket 5.3), and an
  engine-opaque `ui` block that round-trips byte-for-byte. A test asserts
  this file's coverage, so a new step type fails CI until the example
  uses it.

Notes on provisional shapes: the `map` step's `config.body` names the
per-item sub-workflow by convention only until M13 (ADR-015) pins the
final shape, and `agent` roles (`config.agent`) bind to the agent registry
arriving in M14 (ADR-016).
