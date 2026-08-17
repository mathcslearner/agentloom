# ADR-015: Dynamic graph expansion — planner steps & runtime DAG growth

- **Status:** Accepted
- **Date:** 2026-08-15
- **Ticket:** ROADMAP.md ticket 13.1 (opens Milestone 13)

<!--
This ADR opens M13. Ticket 13.1 fixes the whole expansion contract — the
PlanOutput schema, the validation rules and caps, graph_version semantics, the
rejection routing, and the crash matrix — so the later M13 tickets conform to
it without re-litigating the design:

  - 13.2 ExpandRun in the store (graph mutation, counters, outbox, event)
  - 13.3 planner step executor (LLM call → PlanOutput → ExpandRun in the
         completion transaction)
  - 13.4 dynamic map fan-out (engine-generated expansion, no LLM)
  - 13.5 expansion chaos & recovery matrix (kill-at-every-boundary + fuzz)
  - 13.6 run-graph introspection API (versioned provenance)

Sections tagged "(arrives in 13.x)" state the contract now; those tickets add
"### … (as built, 13.x)" subsections under ## Decision as they land, the way
ADR-014 grew across M12.
-->

## Context

Differentiator #2 is workflows whose graphs grow at runtime: a **planner**
step's validated output injects new steps and edges into the *running* graph,
and **map** fan-out instantiates a runtime-sized set of parallel instances.
Neither shape is expressible statically — the planner's steps depend on the
model's output, and the map's width depends on an upstream list — so the run's
graph must be mutable after instantiation.

The persistence model was built for exactly this. ADR-004 gave every run its
own **graph copy** (`run_steps`/`run_edges` rows carrying a `graph_version`)
precisely so runtime mutation is a row insert plus a version bump, not a
copy-on-write of shared definition rows mid-run. ADR-002's event-driven
scheduling computes successor readiness inside the completing worker's
transaction. ADR-005's at-least-once delivery with CAS-guarded, `claim_id`-
fenced claims means duplicate deliveries are ACK-and-drop, never double
execution. The forces this ADR must resolve, given that groundwork:

- **Atomicity.** A half-expanded graph must never be observable. If a planner
  injects five steps and a join, either all six rows and the version bump
  commit together with the planner's own completion, or none do.
- **Crash-safety at every boundary.** A worker can die before claiming, mid-
  LLM, before the commit, or after the commit before the ACK. No boundary may
  produce a lost expansion (the graph never grew) or a duplicated one (the
  same plan applied twice).
- **Bounded growth.** A planner that keeps injecting planners, or emits a
  10,000-step plan, must be stopped by hard caps — expansion is the one place
  a run's cost is unbounded by its definition.
- **Malformed plans are ordinary output failures.** A planner is an llm-family
  step (ADR-013): a plan that does not parse or validate is a
  `validation_failed` verdict that M11's semantic-retry loop repairs, not a
  bespoke error path.
- **Determinism of the *check*, not the *plan*.** The plan itself is
  model output and may differ between attempts; but validating a given plan
  against a given graph state must be a pure function, so the same crashed-and-
  re-executed attempt reaches the same verdict, and only one attempt ever
  commits.

## Decision

We add a **PlanOutput** document, an **ExpandRun** operation that applies it
atomically inside the origin step's completion transaction, a pure
**ValidateExpansion** function that gates every plan under M1's whole-graph
rules plus expansion caps, and a two-class rejection contract that routes
plan-attributable failures to semantic retry and exhausted caps to permanent
failure. `graph_version` (already on every run/step/edge row, ADR-004)
increments once per committed expansion and stamps the rows it introduced.

### The expansion operation (contract; store code in 13.2)

An **expansion** is a pair `(origin, delta)`:

- **origin** — the step whose completion carries the expansion, its kind
  (`planner` in 13.1; `map` in 13.4, `loop` in 14.3, both engine-generated),
  and its **depth** (a definition-authored step is depth 0).
- **delta** — a `PlanOutput`: new **steps** (each a full `dag.Step` — id, type,
  typed config, and every envelope block: retry, timeout, cache, budget,
  validation, context, blackboard) and new **edges** (normal or loop) that
  splice the new steps into the graph.

A plan is **additive-only**. It never deletes or mutates an existing step or
edge. A plan that wants to bypass an existing path adds new edges carrying a
`when` predicate; it cannot rewrite what is already there. This keeps the
frozen part of the graph a stable substrate — existing `remaining_deps`
counters, resolutions, and in-flight attempts are never disturbed — and makes
"resume from the last completed step" unambiguous.

`PlanOutput` reuses the definition's own `Step`/`Edge` types verbatim, so an
injected step is validated, materialized, executed, retried, and billed by
exactly the same machinery as an authored one, and its JSON Schema shares the
definition schema's step-config `$defs` (below).

### Anchors and the three splice patterns (contract)

An edge in a plan connects two endpoints, each of which is either a **delta
step** (a step the same plan adds) or an **anchor** — an existing step in the
run graph, including the origin itself. The splice patterns follow from where
the anchors sit:

- **after** — `origin → new`: the injected step depends on the origin. When the
  origin completes (this very transaction), fan-out resolves the new edge and
  the injected step becomes ready. This is the ordinary planner shape.
- **before** — `new → existing`: the injected step becomes a new upstream
  dependency of an existing step. Legal only while that existing step is still
  `pending` — see the anchor-status rule.
- **parallel-to** — `existing → new → existing-join`: the injected step runs
  beside an existing branch and fans into an existing join.

**Anchor-status rules**, checked under the run lock at expansion time against
the live status of each existing endpoint:

- An edge **from** an existing step is legal only while that step is **non-
  terminal** (`pending`/`ready`/`running`/`retrying`/throttled). A terminal
  step's out-edges have already been resolved by its own fan-out, so a new edge
  from it would sit `unresolved` forever, permanently blocking its target. The
  origin is `running` (mid-completion) and therefore a valid `from`.
- An edge **to** an existing step is legal only while that step is **`pending`**
  (not yet dispatched). Adding an incoming dependency to a step that is already
  `ready`/`running`/terminal is meaningless — it is past the readiness decision
  the new edge would participate in.
- At least one endpoint of every plan edge must be a delta step. An edge
  between two pre-existing steps would mutate the frozen graph (forbidden by
  additive-only).

**Injected-step ids are the plan's ids, verbatim.** They must match the
ADR-003 id rule (`^[a-z][a-z0-9_-]{0,63}$`), be unique within the plan, and not
collide with any existing run-step id. They are *not* namespaced or `#`-
suffixed, because templates and CEL reference steps by id (`${{ steps.<id>.
output }}`) and a downstream authored step may legitimately reference a
planner-injected one by the id the plan chose. The reserved `#k` instance
suffix (ADR-003) belongs to **engine-generated** expansions — map fan-out
(13.4) and loop unrolling (14.3) — where the engine both mints the instance ids
and rewrites the templates that reference them, so no author ever types a `#`.
Provenance (which origin introduced a step, at which version and depth) is
carried by columns (13.2), not by encoding it into the id.

### Validation (as built, 13.1 — `dag.ValidateExpansion`)

`internal/dag` gains `ValidateExpansion(ExpansionInput) ExpansionVerdict`: a
**pure, deterministic** function over the plan and a snapshot of the run graph
(existing step ids with their types and anchor statuses, existing edges, the
resolved caps, the current step count, and the expansions-so-far count). The
store reads that snapshot under the run lock and hands it here (13.2), so the
validator does no IO — the same input always yields the same verdict, which is
what makes a re-executed attempt reach the same decision.

It checks, in one pass reporting every issue:

1. **Plan shape** — `schema_version == 1`, at least one step, each injected
   step's id syntax / intra-plan uniqueness / non-collision with existing ids,
   registered step type, and the full per-step envelope+config validation
   reused from `Validate` (`checkStepConfig`, `checkRetry`, `checkTimeout`,
   `checkCache`, `checkStepBudget`, `checkValidation`, `checkContext`,
   `checkOutputFormat`).
2. **Edge resolution & fields** — every endpoint resolves to a delta step or an
   existing anchor; the additive-only and anchor-status rules above; and the
   loop/normal edge-field rules (a loop edge needs `condition` + in-range
   `max_iterations` and forbids `when`; a normal edge forbids `condition`/
   `max_iterations`), identical to `checkEdges`.
3. **Whole-merged-graph invariants** — the existing nodes/edges plus the delta
   must stay **acyclic under normal edges** (a plan that closes a cycle is
   rejected; only cycles the delta introduces are the plan's fault, since a
   pre-existing cycle would have been rejected at submit), and any plan loop
   edge's target must be a normal-edge **ancestor** of its source — the same
   acyclic-instance-graph invariant every durability mechanism assumes
   (ADR-003/004).
4. **Caps** — the four run guards below.

**Deferred to the in-transaction revalidation (13.2/13.3):** cross-graph
template and context-source **ancestry** lint (a `${{ steps.x.output }}`
reference from an injected step to an existing one). Those references resolve
against *materialized run rows* — a succeeded step's stored output — not a
static definition, so they are revalidated inside the expansion transaction
where the resolved outputs exist, alongside `ExpandRun`'s own recomputation of
`remaining_deps`. `ValidateExpansion` covers everything that is a pure function
of the plan and the graph *shape*; the row-state-dependent checks ride the
store transaction.

### Caps (contract; enforced in-tx in 13.2)

Expansion is the one place a run's size and cost escape its definition, so it
is bounded by hard caps. A run declares overrides in an optional top-level
`expansion` block; absent keys take compiled defaults. `ExpansionPolicy.
Resolve()` yields the effective `ExpansionCaps`:

| Cap | Scope | Default | Meaning |
|---|---|---|---|
| `max_added_steps` | per expansion | 32 | Steps one expansion may inject. A planner may tighten it further via its config `max_added_steps` (never loosen it). |
| `max_total_steps` | per run | 10,000 (= `MaxSteps`) | The run's whole graph size, initial + all injected. Never exceeds the authored-definition ceiling. |
| `max_expansions` | per run | 100 | How many expansions the run may perform. The count is `graph_version - 1` — no separate counter. |
| `max_depth` | per run | 4 | Expansion nesting. A definition-authored step is depth 0; the steps it injects are depth 1; a planner injected at depth *d* produces depth-(*d*+1) steps. The deepest injected step may not exceed this. |

Validation of the block (`checkExpansion`, submit-time): every explicit
override is positive; `max_total_steps` may not exceed the 10k ceiling; the
per-expansion default may not exceed `max_total_steps`; a planner's config
`max_added_steps` is positive and may not exceed the resolved run per-expansion
cap — a planner that could inject more than the run allows is an authoring
mistake.

The 32/256-style defaults are deliberately generous for real agent workflows
(a planner decomposing a task into a handful-to-dozens of steps) yet orders of
magnitude below the 10k performance envelope, so a runaway planner is stopped
long before it degrades the run's graph algorithms.

### Rejection routing — two classes (contract; ADR-013/006)

A rejected plan is not one thing. `ExpansionVerdict` separates two classes so
the engine routes each correctly (`CapExceeded()` is the signal):

- **Plan-attributable** (bad shape, invalid/duplicate/colliding id, unresolved
  endpoint, illegal splice/anchor, cycle, a per-step config error, exceeding
  the **per-expansion** `max_added_steps`) → the planner attempt records
  **`validation_failed`**, and M11's semantic-retry loop (11.4) re-prompts the
  planner with the rejection issues rendered as feedback. A *better plan can
  fix these*, so a retry is worthwhile. This is the same `validation_failed`
  route ADR-013 gives every llm-family step; a planner needs no new outcome or
  class. The transaction rolls back, so a rejected plan expands nothing.
- **Run-guard exhaustion** (`max_total_steps`, `max_expansions`, `max_depth`, and
  the caller-resolved effective per-expansion cap when the run itself is out of
  budget) → a **permanent** failure (`expansion_cap_exceeded`), dead-lettered
  with a descriptive reason, then the run's `on_failure` disposition. A *better
  plan cannot fix these* — the run is simply out of expansion budget — so
  semantic retry would only burn tokens. This is the ADR-006 rows-4/15/17/19
  precedent: a deterministic function of state, re-execution provably futile.
  (14.4 may later add a `park` policy so an operator can raise a run's caps and
  resume, exactly as budget parking works today; 13.1 fixes the permanent-fail
  default.)

Because a planner declares an `output_format: json_schema` over the published
PlanOutput schema (13.3), the M11 pipeline already handles the JSON layer: a
plan that is not even valid JSON is `invalid_json`, repaired by 11.3's
`jsonrepair` where possible, and a structurally-wrong plan is a schema
`validation_failed` — all before `ValidateExpansion` runs its graph-level
checks. The planner's implicit validator and `ValidateExpansion` compose: the
former gates JSON/shape, the latter gates the plan *against this graph*.

### The completion transaction and graph_version (contract; 13.2/13.3)

`ExpandRun(tx, runID, delta)` runs **inside the planner's completion
transaction**, after the claim-fenced `SucceedStep` (and the cost/blackboard
writes) and **before** `fanOut`:

```
LockRun (the run-row lock every transition takes first, ADR-004)
SucceedStep (claim-fenced; a zombie's completion is rejected here → nothing below runs)
  ApplyAttemptCost / blackboard writes (existing)
ExpandRun:
  read the current graph under the lock; ValidateExpansion(snapshot, delta)
    reject → roll back the whole transaction; route per class above
  insert run_steps (status ready iff zero-indegree, else pending; remaining_deps
    and fired_deps computed from the merged incoming edges; graph_version = v+1;
    every envelope policy materialized exactly as instantiation does; depth = origin.depth+1)
  insert run_edges (ordinals continuing from MAX(ordinal)+1; graph_version = v+1)
  bump steps_total by len(delta.steps); set runs.graph_version = v+1
  append the graph_expanded event (origin, version, the full delta)
fanOut (now sees the origin's freshly-inserted out-edges, so an "after"-spliced
  step is readied and outboxed in the same transaction)
attemptRunRollup
--- commit ---
ACK  (ADR-005 discipline: ACK only after the completion transaction commits)
```

Placing `ExpandRun` before `fanOut` is what makes an "after" splice work in one
transaction: the origin's newly-inserted `origin → new` edge is resolved by the
same fan-out that resolves the origin's authored out-edges, so the injected
zero-indegree steps are readied and their outbox rows written atomically with
everything else. The planner's completion assumption that out-edges are
"immutable for the life of the run" (`completeSuccess`, valid through M12) is
exactly what 13.3 changes for the planner path: the origin's out-edge read
moves under the run lock, after `ExpandRun`.

`graph_version` increments **once per committed expansion** and stamps every
row the expansion introduced — so the run's expansion count is `graph_version -
1`, and the row's `graph_version` is its provenance (which expansion created
it; the 13.6 introspection API and M18 UI read it). Concurrent planners (a
fan-out of two planners completing at once) **serialize on the run-row lock**:
each validates against the graph the other already committed, so their versions
are strictly ordered and the merged graph each produced is individually valid.
There is no observable half-expanded state because the whole mutation is one
transaction.

### Crash matrix (contract; automated in 13.3/13.5)

Expansion adds no new recovery mechanism — it rides the ADR-005 boundaries,
because it is *part of* the planner's completion transaction and ACK. Every
cell reduces to "did the completion transaction commit?" The matrix mirrors
ADR-005's format; boundaries are the ROADMAP 13.5 kill points.

| # | Dies / fails at | State left behind | Recovery | Proven by |
|---|---|---|---|---|
| E1 | Before claiming the planner step | Step `ready`; graph unchanged | Ordinary redelivery/claim; nothing to heal | ADR-005 W1/W2 |
| E2 | Mid-LLM, before the completion tx | PEL entry going stale; step `running`, dead worker's `claim_id`; **graph unchanged** | Lease-expiry takeover (`running → ready`, clear claim) → fresh claim → **re-executes the planner**. The re-execution may (via cache) or may not produce the same plan; only the *committed* plan exists, so no plan was lost or applied | 4.5 fencing; 13.3 fencing test |
| E3 | Inside the completion tx, before commit (ExpandRun ran but the tx did not commit) | Nothing durable — the transaction rolled back atomically; graph unchanged, `graph_version` unchanged | As E2: reclaim → takeover → re-execute. The half-applied inserts vanished with the rollback (Postgres atomicity) | 13.5 pre-commit kill |
| E4 | After commit, before `XACK` | Step `succeeded`; injected steps `ready`/`pending`; `graph_version` bumped; PEL entry lingers | Reclaim redelivers → claim CAS sees the planner terminal → **ACK-and-drop**. The expansion committed exactly once; the injected steps' own outbox rows dispatch them | 4.2 duplicate-delivery; ADR-005 W4 |
| E5 | After commit, before the injected steps' dispatch drains (outbox rows written, not yet XADDed) | `graph_expanded` committed; injected `ready` steps have outbox rows undrained | The transactional outbox drains them (at-least-once); a duplicate dispatch is ACK-and-dropped at claim. Reconciler P2 heals a lost dispatch | 4.4 outbox/reconciler; ADR-005 P1/P2 |
| E6 | Zombie planner: reclaimed mid-flight (E2), original worker revives and tries to commit its own expansion | Two workers each hold a plan; the new holder committed (or will) | The original's `SucceedStep` is **fenced on `claim_id`** — its whole transaction, `ExpandRun` included, rolls back and it abandons without ACK. **At most one attempt ever expands** | 4.5 fencing; 13.3 zombie-double-expand test |
| E7 | Redis loses the stream/PEL after commit | Postgres intact: injected `ready` steps with no messages | Reconciler re-outboxes stuck-`ready` steps of a running run (P2/R1(a)); `graph_version` and rows survived in Postgres, the source of truth | ADR-005 R1(a); 5.8 chaos |

The invariant across the matrix: **the expansion is atomic with the claim-
fenced completion**, so it commits exactly when the completion commits, at most
once, and a crash at any boundary leaves either the pre-expansion graph (roll
back / re-execute) or the fully-expanded graph (commit / ACK-drop) — never a
partial one. 13.5 automates the kill-at-every-boundary matrix (E1/E2/E3/E5
against real `cmd/worker` subprocesses, `TestExpansionKillAtBoundaryMatrix`; E6
in-process at 13.3) and fuzzes `ValidateExpansion` over generated deltas — see
the as-built §"Expansion chaos & recovery matrix (13.5)" below.

### Map fan-out and loop unrolling (contract; 13.4 / 14.3)

Both are the same `ExpandRun` primitive with an **engine-generated** delta, no
LLM involved:

- **map** (13.4): a `map` step over a runtime list (`items`, templated from an
  upstream output) instantiates N copies of a named sub-template (`body`) plus
  a gather join collecting ordered results. The sub-template is a **definition-
  level `templates` section** (reserved by this ADR; the `dag` change lands in
  13.4) — `MapConfig.body` names one. Instances are `{body}#k` (the reserved
  `#` suffix), the engine rewriting the sub-template's internal references per
  instance. N is capped by the per-expansion cap.
- **loop** (14.3): a marked loop edge whose condition signals "iterate again"
  expands one fresh copy of the loop body (`{id}#k`) with the prior iteration's
  feedback on the blackboard, exactly the writer⇄critic revision loop.

Both produce a validated delta through `ValidateExpansion` and commit through
`ExpandRun`, so they inherit atomicity, caps, and crash-safety for free — which
is why M13/M14 build them on this primitive rather than bespoke machinery.

### Schema & UI (contract)

The published **`docs/schema/plan-output.v1.json`** (generated by
`make generate` from `PlanOutput`, drift-checked in CI beside the definition
schema) reuses the definition schema's step-config `$defs`, so an injected
step's config shape provably cannot drift from an authored step's. It drives
the planner's implicit `json_schema` validator (13.3) and the builder's plan
inspector (M18). The 13.6 introspection API returns the versioned graph with
per-row provenance (`origin`, `added-at` version/time) and per-version deltas
reconstructed from the `graph_expanded` events; the M18 dashboard animates
expansions from that feed, and needs a layout strategy for injected nodes (they
carry no authored `ui` position) — noted for M18.2.

### Graph mutation in the store (as built, 13.2)

13.2 ships `store.ExpandRun` — the transition-style primitive that applies a
validated `PlanOutput` to a running graph inside the caller's transaction — plus
its schema, its shared materialization, and the deferred cross-graph ref lint.
The engine wiring (a planner completing through it) is 13.3; **nothing in
production calls `ExpandRun` yet.**

**Migration 0023 — provenance columns.** `run_steps` gains `depth` (`INT NOT
NULL DEFAULT 0`; a definition-authored step is 0, an injected step is
`origin.depth + 1`) and `origin_step`/`origin_kind` (both NULL for an authored
step; a paired CHECK keeps them consistent, and `origin_kind ∈
{planner,map,loop}`). `run_edges` gains the same `origin_step`/`origin_kind`. So
the 13.6 introspection API reads a row's provenance as a column, never a join
through the event log. `runs` gains `expansion_caps JSONB` — the run's
**resolved** `dag.ExpansionCaps`, materialized at instantiation exactly like
`retry_policy`/`on_failure` (the failure path never reparses the snapshot, and a
worker upgrade cannot change an in-flight run's caps). A NULL column (a pre-0023
row) means the compiled defaults apply. `graph_version` (on all three tables,
since 0002) needs no change — it already stamps the row's introducing version.

**Shared materialization.** `stepRowParams`/`edgeRowParams` (a new
`materialize.go`) are the single materialization both `CreateRun` and
`ExpandRun` call. Placement (`status`, `remaining_deps`, `graph_version`,
`depth`, origin) differs between the two paths; every **envelope policy**
(retry, timeout, cache, budget, validation, blackboard, context) is materialized
by the shared helper, so an injected step's stored row cannot drift from an
authored step's — the property "an injected step is executed by exactly the same
machinery as an authored one" holds by construction.

**`ExpandRun(ctx, q, ExpandRunArgs) → ExpandRunResult`.** `ErrNoTx`-guarded
(like `ApplyAttemptCost`), it: takes the run-row lock and guards the run status
(`running`/`parked` only — a `cancelling`/terminal run refuses to grow, a
`ConflictRunNotRunning`); reads the run, the origin row (for its `depth`), and
the current steps/edges under the lock; builds the `ExpansionInput` snapshot and
calls `dag.ValidateExpansion`; on rejection returns a typed
`*ExpansionRejectedError` (carrying the verdict, `CapExceeded()` and unwrapping
to the joined `*ValidationIssue`s) so the caller's error return rolls the whole
transaction back — **a rejected plan mutates nothing**; else inserts the steps
(zero-indegree → `ready`, else `pending`; `remaining_deps` = incoming normal
delta-edge count) and edges (ordinals from `MAX(ordinal)+1`), widens the
`remaining_deps` of every `new → existing-pending` "before"-splice anchor,
`ExpandRunGraph` bumps `graph_version` and `steps_total` (an `expected_version`
CAS under the lock — belt-and-braces), appends the `graph_expanded` event, and
readies + outboxes the zero-indegree injected steps. **"After"-spliced steps stay
`pending`** in `ExpandRun` and are readied by the origin's fan-out, which the
caller (13.3) runs after `ExpandRun`.

**The origin anchor.** By the time `ExpandRun` reads the graph the origin's
`SucceedStep` has already run (the row reads `succeeded`), but its fan-out — the
step that resolves its out-edges — has *not*. So `buildAnchors` forces the
origin to `AnchorActive` regardless of its row status: an `origin → new` "after"
edge is a valid `from`, exactly as ADR-015 models the origin as `running`. The
origin's `Succeeded` flag is read from the true row status, so the ref lint sees
its output as available.

**The deferred cross-graph ref lint.** 13.1 deferred the
`${{ steps.x.output }}` / `step_output`-context ancestry check to "the store's
in-tx revalidation," because it depends on materialized run rows. 13.2 lands it
*inside* `dag.ValidateExpansion` (not the store — keeping the check pure and
fuzzable): `ExpansionAnchor.Succeeded` and `ExpansionInput.RunParams` are new
inputs the store fills from the graph snapshot and the run's params;
`checkExpansionRefs` requires every injected-step reference to resolve to a
merged-graph **normal-edge ancestor** *or* an existing **succeeded** step (its
output is materialized and immutable), and every `run.params.<key>` reference to
name a submitted parameter. It reuses `classifyUpstreamRef` and the same codes
as the authored-template lint, so an injected step is held to exactly the
authored reference contract. A reference failure is plan-attributable
(`validation_failed`), never a cap.

**Concurrency.** Two planners completing at once serialize on the run-row lock:
each `ExpandRun` validates against the graph the other already committed, so
their `graph_version`s are strictly ordered (2 then 3), `steps_total` is the
sum, the `graph_expanded` events step the versions linearly in seq order, and an
id collision between the two plans is rejected on the loser. Proven by
`TestExpandRunConcurrent` + `TestExpandRunIDCollisionRejected`; the splice
matrix (after/before/parallel-to), cap/rejection atomicity, depth propagation,
and the guards are the rest of `expand_integration_test.go`.

Not yet (after 13.2): the planner executor that composes `SucceedStep` +
`ExpandRun` + fan-out and routes `*ExpansionRejectedError`
(`CapExceeded` → permanent `expansion_cap_exceeded`, else `validation_failed`
→ 11.4 semantic retry) is 13.3, which also moves `completeSuccess`'s out-edge
read under the run lock (after `ExpandRun`) for **every** completion, since an
active non-origin anchor may complete concurrently with an expansion.

### Planner executor (as built, 13.3)

13.3 ships the `planner` step executor and composes `ExpandRun` into the
completion transaction — the runtime that turns a validated plan into a graph
mutation. Nothing new is invented: a planner is an **llm-family step**, so the
whole M8–M12 pipeline (routing, request framing, response cache, rate limiter,
cost estimate, semantic feedback, context assembly, window guard) is reused
verbatim, and the plan validation reuses M11's implicit-validator + semantic-
retry machinery. The ticket is mostly *composition*.

**`exec.PlannerExecutor` is the llm executor re-targeted.** It embeds
`LLMExecutor`; the only overrides are `Type()` (`planner`) and the manifest
(a distinct `(executor, planner, 1.0.0)` identity, cacheable + cost-bearing).
The reuse hinges on one helper: `llmConfigView(sc)` decodes the step's config
into the effective `LLMConfig` every request-building hook runs on — for an
`llm` step it is the `LLMConfig` verbatim; for a `planner` step it **projects**
the `PlannerConfig` onto an `LLMConfig` carrying the same model-call fields plus
an **implicit plan `output_format`** (`type: json`, `mode: auto`) and drops
`max_added_steps` (engine state, not a model input). Because the raw-key hooks
(`WithFeedback` / `WithContext` / `WithModel`) already rewrite the top-level
`prompt` / `messages` / `model` keys — which a planner config also has — they
need no change, so a planner gets semantic-retry feedback injection and context
assembly for free. The plan lands on the ordinary llm output shape
(`output.json`, via 11.3's `shapeStructured`), so `${{ steps.plan.output.json }}`
is addressable and the audit/UI provenance is the same field every structured
step uses.

**The provider request is deliberately schema-less; the full schema is enforced
engine-side.** The implicit `output_format` is a plain-`json` native request
(OpenAI `json_object` / Anthropic forced permissive-object tool / mock
structured echo, with `jsonrepair` fallback), *not* the 17 KB `PlanOutput`
schema. The engine then prepends an implicit `json_schema` validator over the
**published `plan-output.v1.json`** to the planner's chain
(`implicitPlanSchemaSpec`, the schema generated once per process). Reason: the
generated schema uses `$ref`/`$defs`/`if-then` per step type, which OpenAI
strict mode rejects and which cannot be validated against Anthropic offline —
so keeping the schema on the engine's validate stage makes plan-shape checking
provider-independent. The JSON layer (invalid JSON → `invalid_json`, wrong
shape → schema `validation_failed`) and `ValidateExpansion` (graph-illegal
splice → `validation_failed`, cap exhaustion → permanent) **compose** exactly as
this ADR's §"Rejection routing" describes.

**The completion transaction gained `ExpandRun` and moved the edge read under
the lock.** `completeSuccess` now, for a `planner` origin: decodes the plan from
`output.json` pre-transaction (the implicit validator already gated its shape,
so this normally succeeds; a residual decode defect is plan-attributable), then
inside the transaction — after the claim-fenced `SucceedStep` and cost/
blackboard writes, at a new `after_expand` failpoint — calls `store.ExpandRun`,
skipped on a cancelling run (the run must quiesce, not grow). The out-edge read
and `planEdges` **moved from before the transaction to inside it, under the run
lock, after any expansion — for every completion, not just planners** — because
the pre-M13 "out-edges immutable for the life of the run" assumption is exactly
what expansion breaks: a planner's own `origin → new` "after" edge, and a
concurrent `parallel-to` splice onto an active non-origin anchor, both add
out-edges the fan-out must see. So the origin's fan-out resolves the freshly-
inserted `origin → new` edges and readies the "after"-spliced steps in the same
transaction, alongside `ExpandRun`'s own outbox rows for the zero-indegree
injected steps.

**Rejection routing is one branch on `CapExceeded()`.** A rolled-back
transaction carrying a `*store.ExpansionRejectedError` routes via
`routeExpansionRejection`: `CapExceeded()` → `completeFailure` permanent with a
descriptive `expansion_cap_exceeded:` reason (a better plan cannot lift a cap);
otherwise → `completeValidationFailure` with a **synthesized fail verdict** —
the passing chain verdict's results plus one `expansion` validator result whose
issues are the rejection's error-severity issues mapped 1:1 (code/path/message,
structure only). 11.4's `buildFeedback` renders those issues into the re-prompt
and the terminal DLQ records them as verdict history, so a rejected plan self-
heals through the same loop as any other validation failure — no bespoke planner
feedback path. `out` carries the productive spend on both routes, so the
planner's provider call is metered whether the plan is applied, retried, or
capped.

**Empty plans are a no-op** (an ADR amendment). A planner that legitimately
concludes it should add nothing returns `{"schema_version": 1, "steps": []}`;
rather than force a wasteful semantic retry against `ValidateExpansion`'s
"at least one step" rule, `completeSuccess` treats a plan with no steps as no
expansion — an ordinary success. The store guard stays (a plan reaching
`ExpandRun` still must be non-empty); the engine simply does not call it. A plan
with edges but no steps is still malformed (its edges reference nothing) and
`ExpandRun` rejects it.

**Crash-safety is inherited, proven.** The `after_expand` failpoint test aborts
the transaction right after `ExpandRun` and shows no injected rows and an
unmoved `graph_version` (cell E3). The zombie-planner test stalls worker A past
its lease, lets B take over and expand, then resumes A — whose completion,
`ExpandRun` included, is fenced at `SucceedStep` and abandoned without ACK, the
graph expanded exactly once (cell E6). The mock refinement that makes the
offline example runnable: the mock's structured echo returns the last user text
verbatim when it is already a JSON object (a planner prompt that carries its
plan), else the `{"echo": …}` wrapper — so `examples/definitions/planner.json`
runs a real expansion on the unscripted mock with zero scripting.

Not yet (after 13.3): dynamic map fan-out (13.4 — the same `ExpandRun` with an
engine-generated delta), the chaos/recovery matrix (13.5 — automating the E1–E7
kill points and fuzzing `ValidateExpansion`), and the run-graph introspection
API (13.6).

### Map fan-out (as built, 13.4)

13.4 ships the `map` step — the second producer of an expansion delta and the
first **engine-generated** one (no LLM). It composes the same `ExpandRun`
primitive as the planner: only how the plan is built and how a rejection routes
differ. **No migration, no new config var, no new metric** — the `origin_kind`
CHECK (0023) already admitted `'map'`, and the run's definition snapshot already
carries everything the delta is generated from.

- **The `templates` library.** `Definition.Templates` (`map[string]*Template`,
  `Template = {steps, edges}`) is a definition-level library of reusable
  sub-graphs — validated but **never instantiated at run creation**; it rides in
  the run's definition snapshot so a map completion reads it at runtime. A
  `map` step's `MapConfig.body` names one; `MapConfig.items` (now `json.RawMessage`,
  so a whole-expression template renders an array into it) is the runtime list;
  `MapConfig.max_items` is a per-map cap. `checkTemplateSection` validates each
  template as a self-contained mini-definition — unique local ids, valid
  per-step config/envelope, local-only edges, no normal-edge cycles, and
  **exactly one sink** (the terminal step whose output the gather collects) —
  and `checkMaps` requires every `body` to name a declared template.

- **The reserved `#` instance space, finally used.** A map instance is
  `<body_step>#<k>`; its generated gather is `<map>#gather`. `instanceStepIDRe`
  (`^…(#[a-z0-9_]+)+$`) is disjoint from the authored `stepIDRe` (which forbids
  `#`), so an engine-minted id can never collide with an authored one.
  `ValidateExpansion` accepts instance ids **only for map/loop origins** (a
  planner still injects into the authored space); the template grammar
  (`parseRef`, `isRefChar`) accepts `steps.<body>#<k>.output` refs so instance
  and gather configs parse at runtime; and two new reserved template roots,
  `${{ item }}` / `${{ item_index }}`, are admitted **only inside a template
  body** (an item root on an ordinary step is `template_ref_invalid`).

- **Generation (pure `dag.GenerateMapExpansion`).** Per item `k`: one instance
  of every body step (`<id>#k`), the body's internal edges rewritten
  (`<from>#k → <to>#k`), an **after-splice** `map → <entry>#k` for each entry
  step, and `<sink>#k → gather`. The body's config is rewritten per instance —
  `item → steps.<map>.output.items.<k>`, `item_index →
  steps.<map>.output.indices.<k>`, `steps.<local> → steps.<local>#<k>` — so the
  item flows through the map step's own output and internal refs point at
  instance siblings, all resolved by the ordinary runtime renderer (no partial
  render engine). The gather step carries an ordered
  `["${{ steps.<sink>#k.output }}", …]` config, so it emits the results **in
  list order**. An empty list yields a gather-only delta (the empty ordered
  array).

- **Executors.** `MapExecutor` (no flags) resolves the rendered list, enforces
  `max_items` (over → permanent, before any expansion), and emits `{items,
  indices, count}` — the list the completion generates from and each instance
  references. `GatherExecutor` (no flags) emits its resolved ordered items
  array verbatim (all-mode readiness makes it a barrier over the instances).

- **Completion + routing.** `completeSuccess` handles a `map` origin exactly
  where it handles a planner: after the fenced `SucceedStep`, `store.ExpandRun`
  (origin `map`), then the map's fan-out readies the after-spliced entries. A
  rejected map delta is **always permanent** (`routeMapRejection`): the delta is
  engine-generated, so no re-prompt can fix it — a run-guard cap exhaustion
  dead-letters `expansion_cap_exceeded`, anything else `map_expansion_invalid`.
  fail-fast falls out for free: an instance dead-letter fails the run under the
  default `fail_fast` policy, and the gather never gathers.

- **Scope split (13.4b).** collect-errors — an item failure collected as an
  error slot in the gathered array with the run still succeeding — needs new
  readiness semantics (the gather firing on *all-terminal*, not all-succeeded)
  and is carved into a follow-up ticket **13.4b**; 13.4 ships map + fail-fast +
  gather + the caps. Multi-step sub-template bodies are fully supported (the
  rewriter is general); downstream consumption of the gathered array by an
  authored step is deferred (the gather id is minted at runtime, so an authored
  reference to it cannot be validated at submit) — 13.4's acceptance reads the
  gather's own output. Canonical `examples/definitions/map_fanout.json`; both
  kitchen sinks + the construct pin gained a `templates` section, a `map`, and a
  `gather`.

### Map collect-errors (as built, 13.4b)

13.4b adds the `collect_errors` item-failure policy — the alternative to 13.4's
fail-fast. Under it a map instance that fails terminally is **tolerated**: the
run stays alive, the failure is recorded, and the generated gather emits the
ordered result array with an error slot for the failed item.

- **The irreducible constraint.** `SucceedRun`'s guard is
  `steps_succeeded + steps_skipped = steps_total AND steps_failed = 0`, so a
  dead-lettered instance (which bumps `steps_failed`) makes the run
  unsucceedable. Therefore a tolerated failure must **not** count as
  `steps_failed`. That is the one unavoidable store change; everything else
  composes around it.

- **A new terminal status `collected`** (migration 0024), distinct from
  `dead_lettered` for observability honesty: `CollectFailStep` settles a failed
  instance `running → collected` (claim-fenced), records the real ADR-006 class
  on the attempt, stores an **engine-synthesized error marker**
  (`{"map_item_failed": true, "class": …}` — structural, never raw error text,
  the 11.x hygiene convention) as the step's output, and bumps a new
  `runs.steps_collected` counter (not `steps_failed`). No `dead_letters` row — a
  collected item did not stop progress. The `SucceedRun` / `FailRunRollup` /
  `CancelRunRollup` all-terminal sums are widened with `+ steps_collected`, so a
  run with collected instances rolls up honestly (success still requires
  `steps_failed = 0`, so `collected` — unlike `dead_lettered` — never blocks it).

- **The gather is mode-agnostic.** Its config is identical for fail-fast and
  collect-errors (strict `${{ steps.<sink>#k.output }}` refs). The engine's
  `renderConfig` is widened to include `collected` outputs, so a failed
  instance's ref resolves to the error marker rather than a missing reference —
  a rich marker, not a bare null, with no generator branch. Under fail-fast the
  gather simply never renders (the run failed / it was written off).

- **Readiness via the ordinary fan-out.** `completeFailure`'s terminal branch,
  when the step is a collect-errors map instance (detected pre-transaction by
  reading the origin map's config; an unreadable config falls back to
  fail-fast), routes to `CollectFailStep` and then the **normal success-style
  fan-out** — the instance's unconditioned `sink#k → gather` edge fires,
  advancing the gather's ordinary all-mode counter. The gather thus readies when
  all instances are terminal (succeeded ∨ collected), and the whole
  dead-letter/write-off path is bypassed (`planWriteOff` only sees resolved
  edges). The instance's normal transport-retry budget is unchanged — only the
  *terminal* disposition differs.

- **Scope: single-step bodies only** (validation-enforced —
  `config_field_invalid` on a multi-step body). A mid-body failure would strand
  the sink and needs a per-instance chain write-off; deferred. fail-fast works
  for any body.

`collected` was added to the terminal-status set everywhere it is enumerated
(the claim/takeover ack-drop classifiers, the OpenAPI `StepState` enum, the
run-view `steps_collected`, `ctl watch`'s glyph). One migration (0024), no new
config var, no new metric.

Not yet (after 13.4b): the chaos/recovery matrix
(13.5), and the run-graph introspection API (13.6).

### Expansion chaos & recovery matrix (as built, 13.5)

13.5 turned the crash matrix above from an argued contract into an automated
one, and fuzzed `ValidateExpansion`. It added **no migration, no config var, no
new metric** — only a test-gated crash-injection seam and two test files.

**The crash seam** (`engine/crashpoint.go`). The completion-transaction
boundaries the matrix must hit — E3 (after `ExpandRun`, before commit) and E5
(after commit, before the ACK/dispatch) — live *inside* the engine and cannot
be provoked by a parent process's SIGKILL, which lands at an arbitrary
instruction. This is the same gap the queue's `ConsumerConfig.PhaseHook` (3.6)
and the in-process `completeFailpoint` fill for their boundaries. Unlike those,
a crash point **hard-exits** the process (`os.Exit(137)`) with no deferred
cleanup — the faithful SIGKILL analogue: heartbeats stop, the pgx connection
drops (any in-flight transaction rolls back under Postgres atomicity), the PEL
entry lingers un-acked, and recovery flows entirely through ADR-005's reclaim →
takeover path. `maybeCrash(stage, stepID)` is a nil-pointer fast path when
unarmed (the same negligible cost as the `failpoint` check already on these
paths), armed only by `AGENTLOOM_WORKER_CRASH_POINT=<stage>:<step_id>` via
`InstallCrashPointFromEnv`, which `cmd/worker` calls **only when test executors
are enabled** — so it is doubly inert in any real deployment. Four boundaries
are wired: `pre_claim` (E1, before the claim CAS in `Handle`), `pre_completion`
(E2, after the executor returns, before the completion tx in `execute`),
`after_expand` (E3, sharing the `stageAfterExpand` failpoint site in
`completeSuccess`), and `post_commit` (E5, right after the completion tx commits,
before the ACK). The stage constants are exported (`engine.CrashStage*`) so the
subprocess matrix references one vocabulary — a rename is a compile error, not a
silently-never-firing crash. Matching is by step id (uniform: the pre-claim
boundary has only the envelope's step id).

**The subprocess matrix** (`test/crash/expansion_chaos_integration_test.go`,
`TestExpansionKillAtBoundaryMatrix`) runs the offline planner definition (the
mock echoes the plan verbatim, as in `examples/definitions/planner.json`) and,
per boundary × repeated runs, spawns **one armed worker** that crashes at that
boundary on the planner step, waits for its process to exit (asserting the 137
crash code, never a clean drain), then spawns an **unarmed survivor fleet** that
recovers the run. The response cache is disabled for the run (it is global-scoped
in the shared dev Redis, so an identical planner request would hit a prior run's
entry and short-circuit the boundary — cache behavior is orthogonal to expansion
recovery, exactly the E2 "may or may not re-plan via cache" note). The invariant
proved across every boundary — the matrix's whole point — is **exactly one
committed expansion and eventual completion**: `graph_version` 2, `steps_total`
5, one `graph_expanded` event stepping 1→2 (a linear history, the "no orphan
steps / linear graph_version" quiescence check), and every step (injected
included) terminal `succeeded` with `step_succeeded` firing exactly once (no
double-execution, no double-expansion). The reconciler stays out (10m interval)
so reclaim + the transactional outbox are the recovery mechanisms under test.

**The fuzzer** (`internal/dag/expansion_fuzz_test.go`, `FuzzValidateExpansion`)
feeds arbitrary bytes through `DecodePlanOutput` — the same gate a planner's
output passes before `ValidateExpansion` runs — and for every decodable plan
asserts two properties: (1) `ValidateExpansion` never panics; (2) no
acceptance-of-invalid — when the verdict is OK, an **independent oracle**
(`NewGraph` + `TopoOrder` over the merged graph, sharing none of
`ValidateExpansion`'s code path) confirms the structural invariants
`ValidateExpansion` owns actually hold: every injected id unique and
collision-free, every edge endpoint resolves, and the merged normal-edge graph
is acyclic. The oracle checks necessary conditions only (graph structure), so it
never false-positives; anchor-status and cap semantics stay covered by the
deterministic tables in `expansion_test.go`. Over 1M+ executions the fuzzer found
no panic and no over-acceptance.

### Run-graph introspection API (as built, 13.6)

13.6 closes M13 with `GET /v1/runs/{id}/graph` — the read-only endpoint that
surfaces a run's current versioned graph with per-row provenance plus the
ordered per-version expansion deltas, so the M18 dashboard can render and
animate runtime expansion. It is purely additive: **no migration, no new config
var, no new metric** — every fact it reports was already persisted by 13.2
(the `depth`/`origin_step`/`origin_kind`/`graph_version` columns) and the
`graph_expanded` events. The only store change is a bounded read query,
`ListEventsByType` (a run's `graph_expanded` events are few, so filtering the
event log by type beats scanning it).

**Sourcing — rows for the graph, events for the deltas.** The `nodes` and
`edges` come from the `run_steps` / `run_edges` rows: they carry the live
`status`/`resolution` plus the denormalized provenance columns, mapped onto a
`GraphOriginView` (`kind: definition` when the origin columns are NULL, else the
stored `origin_kind` with `origin_step`). The `expansions` come from the
`graph_expanded` events, which carry the authoritative version transition
(`from_version`/`to_version`), the injected `depth`, the delta, and — via the
event row's `created_at` — the injection time. A node's `added_at` resolves
through a version→time map: version 1 is the run's creation time, and each later
version is its expansion event's time. The event delta is decoded to topology
only (step ids/types, edge endpoints/types) — deliberately not into `dag.Step`,
whose `Config` is a `StepConfig` interface only `DecodePlanOutput` can populate,
and which the graph view surfaces nothing of.

**Reconstructing any version.** Both DoD reconstruction paths hold by
construction: every node and edge carries the `graph_version` it was introduced
at, so a client reconstructs version *N* by keeping the rows with
`graph_version ≤ N`; independently, `expansions` replays base-graph + ordered
deltas. The consistency DoD (`graph_expanded` events and API versions agree) is
a test: `graph_version = 1 + len(expansions) = run.graph_version`, every node's
introducing version is 1 or belongs to an expansion, the row-derived and
event-derived added sets coincide per version, and the delta feed is linear
(`from_version[i] = to_version[i-1]`). A committed golden
(`internal/api/testdata/run_graph_fixture.json`) is the exported fixture for the
M17/M18 frontend tests; the `TestRunGraphIntrospection` integration test drives
`planner.json` to completion and asserts the injected workers carry
`origin.kind=planner`/`depth=1`/`graph_version=2`. Reads are non-transactional
(three separate queries), matching the `GET /v1/runs/{id}` precedent — a torn
read against a live-expanding run is a benign momentary inconsistency the next
poll heals, and introspection needs no snapshot isolation.

## Consequences

Positive:

- **Runtime graph growth is a row insert plus a version bump**, atomic with the
  planner's completion — the payoff of ADR-004's per-run graph copy. No new
  recovery mechanism, no new event beyond `graph_expanded`, no two-phase
  protocol.
- **Malformed plans self-heal.** A planner is an llm-family step, so M11's
  semantic-retry loop repairs bad plans with the rejection issues as feedback —
  the differentiators compound (M11 × M13) with zero bespoke code.
- **Caps make runaway planners safe**, and `graph_version` doubles as the
  expansion counter, so `max_expansions` needs no new column.
- **One primitive, three features.** Planner injection, map fan-out, and loop
  unrolling are all `ExpandRun` over a validated delta — the hardest
  correctness milestone reduces to one atomic operation plus one pure
  validator.
- **The check is pure and testable in isolation.** `ValidateExpansion` is a
  library function fuzzable without a database (13.5), and its determinism is
  what makes the crash matrix's "re-executed attempt reaches the same verdict"
  argument hold.

Negative:

- **A planner completion is now a larger transaction** — it may insert dozens
  of rows and revalidate the graph under the run lock. Bounded by the caps
  (default ≤ 32 rows/expansion), and it holds a lock the completion already
  took, but it is heavier than an ordinary step's completion.
- **Re-execution of a crashed planner may produce a different plan.** After E2/E3
  the re-executed attempt calls the model again; response caching (ADR-011)
  makes the same request return the same plan, but a non-deterministic model +
  a cache miss can diverge. This is acceptable — only one plan ever commits,
  and both plans are valid — but it means "the plan a run used" is the
  committed one, not necessarily the first one attempted.
- **Cross-graph template ancestry is validated in two places** — the pure
  `ValidateExpansion` for plan-internal references and the store transaction for
  references into materialized run rows. The split is principled (static shape
  vs. row state) but is a seam future readers must hold in their head.
- **Injected ids share the authored id namespace**, so a plan colliding with an
  existing id is a validation_failed the planner must avoid; the engine cannot
  silently rename it without breaking template references.

## Alternatives considered

- **Apply the expansion in a separate transaction after the planner
  completes.** Rejected: it creates an observable half-state (planner done,
  graph not yet grown) and a new crash boundary between the two commits that
  needs its own recovery cell. Folding `ExpandRun` into the completion
  transaction makes "did it commit?" the only question, reusing every ADR-005
  boundary unchanged.
- **Mutate the shared definition snapshot instead of inserting run rows.**
  Rejected by ADR-004 already: runs share no graph rows precisely so runtime
  mutation touches only the mutating run, with no copy-on-write at the worst
  possible moment (mid-run, under contention).
- **Namespace injected ids** (`plan.step1`, or `#`-suffix them). Rejected:
  authored and injected steps must share one id space so a downstream authored
  step can reference a planner-injected step's output by id; the `#` suffix is
  reserved for engine-generated instances where the engine controls both the
  minting and the template rewriting.
- **Allow plans to delete or rewire existing steps.** Rejected for v1: mutating
  the frozen graph would disturb in-flight `remaining_deps` counters and
  resolutions and make "resume from the last completed step" ambiguous.
  Additive-only plans keep the frozen substrate stable; a plan expresses "skip
  this path" with a `when`-conditioned new edge, not a deletion.
- **Route every rejected plan to a semantic retry.** Rejected: a plan rejected
  for exceeding `max_total_steps`/`max_expansions`/`max_depth` cannot be fixed
  by a better plan — the run is out of expansion budget — so retrying only
  burns tokens. Cap exhaustion is permanent (ADR-006 rows 4/15/17/19 precedent);
  only plan-attributable failures retry.
- **Validate the whole merged graph by calling `dag.Validate` on a synthesized
  definition.** Rejected: a merged graph contains `#`-instance ids that the
  authored-id regex rejects, and whole-definition checks like `no_entry_step`/
  `isolated_step` do not apply to a fragment; `ValidateExpansion` reuses the
  per-step and cycle primitives directly and maps every issue path back to the
  plan document.
