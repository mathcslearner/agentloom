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
  step.
- **[conditional_branch.json](conditional_branch.json)** — exclusive
  routing: a classifier feeds a `branch` step whose out-edges fire
  first-match-in-declaration-order — two `when`-conditioned arms plus a
  trailing default. The arms reconverge on a `join all`; the un-taken arms
  are *skipped*, and skipped parents satisfy the join (skip propagation).
- **[critic_loop.json](critic_loop.json)** — bounded writer↔critic
  refinement via a marked **loop edge** (`type: loop`, `condition`,
  `max_iterations`) — the only sanctioned kind of cycle, executed by
  unrolling (M14) — plus a conditioned exit edge.
- **[kitchen_sink.json](kitchen_sink.json)** — one coherent
  research-and-publish pipeline exercising every construct: all 14 step
  types, both join modes (`any` and `all`), conditioned and unconditioned
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
