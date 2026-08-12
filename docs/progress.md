# Progress log

Per-ticket implementation history: what each ticket actually delivered, the
non-obvious decisions made along the way, and any deferred quirks. This is the
project's memory between sessions — **read the sections relevant to the code
you are about to touch before starting a ticket that builds on earlier work.**

Conventions: one `##` section per milestone, one `###` subsection per ticket,
appended as tickets complete. High-level current status (which milestone, next
ticket, open loose ends) lives in `CLAUDE.md`; sequencing and acceptance
criteria live in `ROADMAP.md`; design decisions live in `docs/adr/`. This file
carries the detail that none of those capture.

## Milestone 0 — Foundation & architecture

### 0.1 — Repo scaffold & tooling ✅

Project named **agentloom**, module path `github.com/mathcslearner/agentloom`,
directory skeleton, `Makefile` (`lint` / `test` / `test-integration` / `fmt`,
pinned golangci-lint auto-installed into `./bin`), golangci-lint v2 config,
Apache-2.0 license, README.

### 0.2 — CI pipeline (one acceptance box still open)

`.github/workflows/ci.yml` runs parallel lint (go vet + golangci-lint, version
pinned in sync with the Makefile) and `go test -race` jobs on PRs and pushes
to main, with setup-go caching and a README status badge. **Still open:** the
one-time red-path check (a deliberately failing test fails CI, verified once
then removed) has to be done on GitHub.

### 0.3 — Architecture overview document ✅

`docs/architecture.md` (components + Mermaid diagrams, execution data flow,
tech-stack rationale, glossary) and the doc index `docs/README.md`.

### 0.4 — ADR template, ADR-001, ADR-002 ✅

`docs/adr/` with `template.md` (Nygard-style sections, numbering/supersession
conventions), ADR-001 (service boundaries: exactly two deployables, shared
internal packages, datastores as the compatibility surface), ADR-002
(scheduling model: no central scheduler, completing worker computes readiness
in the completion tx, outbox dispatch, escape criteria for a dedicated
scheduler, sharded-streams scale lever), and the index `docs/adr/README.md`.

### 0.5 — Config & structured logging foundation ✅

`internal/config` (env-driven config with `AGENTLOOM_*` vars, defaults < env
precedence, injectable env lookup, all invalid values reported in one joined
error; `LogConfig` sub-config), `internal/obs/log` (slog JSON/text logger
factory, canonical field constants + typed attr helpers for
`run_id`/`step_id`/`attempt`/`worker_id`/`trace_id`, nil-safe
`Into`/`From`/`With` context helpers), the `cmd/demo` throwaway (deleted in
M4; logs the build version on startup), and `internal/version`
(ldflags-injected build version, consumed by the real binaries from M4).

## Milestone 1 — Workflow definition core ✅ (complete)

All exit criteria met: fixture suite passes, 10k-node graph under 100ms,
generated JSON Schema published.

### 1.1 — ADR-003: workflow definition format ✅

`docs/adr/003-workflow-definition-format.md` decides the JSON contract —
top-level shape (`schema_version` int, `name`, `params` declarations, `steps`,
`edges`, opaque `ui`), the full step-type catalog with per-type typed `config`
(JSON example each; `#` and `.` reserved in step IDs for M13/M14 instance
naming and CEL paths), edge semantics (optional `when` CEL, parallel fan-out
by default, `branch` = first-match-in-declaration-order firing rule on its
out-edges with at most one trailing default), loop edges (`type: loop`,
required `condition` + `max_iterations`, graph-minus-loop-edges must be a DAG,
`to` must be an ancestor of `from`, executed by unrolling),
readiness/skip-propagation/join semantics (`join all`: skipped parents
satisfy, skipped only if all parents skipped; `join any`: first success fires;
CEL eval errors are recorded failures, never `false`), versioning (integer
bumped on breaking changes only; additive fields don't bump; strict
unknown-field rejection everywhere except the byte-for-byte round-tripped `ui`
subtree), limits table (10k steps / 20k edges / 1 MiB, etc.), and per-ticket
enforcement points for 1.2–1.5.

### 1.2 — Definition types & JSON codec ✅

`internal/dag` with the definition Go structs (typed per-step-type configs
behind a `StepConfig` interface + registry; pointer `Temperature` so explicit
0 survives; `Edge.Type` normalized to `normal` on decode, omitted on encode),
a hand-rolled strict decoder (`Decode`: schema_version gated first and
reported alone; unknown fields recursively, wrong types, bad enums, and
missing required keys all collected into one joined error with JSON paths like
`steps[2].config.max_tokens`; opaque payload fields compacted for byte-stable
re-encoding), a canonical encoder (`Encode`: fixed field order, defaults
omitted, no HTML escaping, `ui` subtree spliced byte-for-byte as captured),
JSON Schema generation (invopop/jsonschema from the same structs, per-type
config union as `oneOf` on Step, committed at
`docs/schema/workflow-definition.v1.json`) wired into `make generate` with a
CI drift-check step plus an in-package drift test, and a fixture corpus under
`internal/dag/testdata/` (valid round-trip fixtures incl. a kitchen-sink of
all 11 step types; one invalid fixture per decode-error class).

### 1.3 — Structural validation ✅

`internal/dag/validate.go` with `Validate` — pure, runs on decoded or
programmatically built definitions, reports every violation in one pass as
`ValidationIssue`s carrying stable snake_case codes, severity (warnings don't
block), and JSON paths, `errors.As`-reachable through the joined error. Rules:
step-ID syntax/uniqueness, registered types, edge endpoints exist, per-type
required config (llm/planner = model + exactly one of prompt|messages, etc.),
loop-edge rules (condition + max_iterations 1..100 required; loop-only fields
rejected on normal edges; `when` rejected on loop edges), branch out-edge
rules (≥1 out-edge; only a trailing default may omit `when`),
at-least-one-entry-step, isolated-step warnings, and the ADR-003 limits table
(the 1 MiB size gate lives at the top of `Decode`, reported alone). ADR-003
gained a 1.3 clarifications subsection (orphans = isolated-step warning;
reachability needs no 1.3 rule since non-loop cycles are rejected in 1.4).
Fixture corpus extended with `internal/dag/testdata/invalid_structural/`
(decode-clean, validation-failing; one per rule; count/size limits generated
in tests) plus `valid/isolated_step.json` (warning fixture).

### 1.4 — Graph algorithms: cycles, topology, readiness ✅

`internal/dag/graph.go` (`Graph` adjacency view via `NewGraph` — loop edges
indexed separately, excluded from adjacency; deterministic Kahn `TopoOrder`;
`Ancestors` reachability; iterative three-color-DFS cycle finder reporting one
finding per back edge with the full step path) and `internal/dag/ready.go`
(`Graph.ReadySteps(completed, skipped, failed)` — ADR-003
readiness/skip-propagation/join semantics as a pure function: single
topo-order pass for the skip fixpoint, edge resolutions derived from step
state (completed→fired, skipped→skipped, failed/pending→unresolved; runtime
`when`/branch outcomes enter by seeding the skipped set), non-join and
`join all` share the all-resolved-plus-one-fired rule, `join any` fires on
first success, failed parents block, results in declaration order, errors on
unknown/overlapping IDs and cyclic graphs). `Validate` gained a graph phase
(gated on no duplicate-ID/unknown-endpoint issues) with codes `cycle_detected`
and `loop_edge_not_ancestor`; ADR-003 gained a 1.4 clarifications subsection
(cycle-reporting granularity, step-level ReadySteps API with per-edge outcomes
as the engine seam). Tests: property-based via `pgregory.net/rapid` (random
DAG progressions checked against a naive fixpoint reference; monotonicity,
no-unmet-deps, termination; topo-order and cycle-rejection properties),
fixtures `invalid_structural/{cycle,loop_edge_not_ancestor}.json`, and a
10k-step/19.8k-edge synthetic benchmark — Validate ~9ms + ReadySteps ~0.6ms,
with a <100ms gate test (skipped under `-race`).

### 1.5 — CEL edge conditions ✅

`internal/dag/cel.go` (cel-go v0.31) — shared `sync.OnceValues` environment
declaring exactly `output` (dyn) and `run` (`map(string, dyn)`, only
`run.params` populated; standard CEL lib/macros, no custom functions),
`CompileExpr` (compile + typecheck; joined `*ExprError`s with 1-based
line:col; result type must be bool-or-dyn else `*ExprNotBoolError`), and
`CompiledExpr.Eval(output, params)` returning the routing bool or a typed
`*EvalError` (missing field, type error, or dyn-non-bool result; per ADR-003
never coerced to `false` — the engine's completion tx (M4) calls it and
records failures; classification is ADR-006's). `Validate` compiles every
normal-edge `when` and loop-edge `condition` (skipping over-length
expressions, already rejected; ~46µs/expr compile cost, benchmarked) with new
codes `invalid_expression` (one issue per CEL error, position-prefixed
message) and `expression_not_boolean`. Environment documented in
`docs/expressions.md` (indexed in `docs/README.md`); ADR-003 gained a 1.5
clarifications subsection. Fixtures:
`invalid_structural/{when_syntax_error,when_undeclared_ref,when_not_boolean,condition_syntax_error}.json`,
an expression error added to `multi_error_structural.json`, and
`valid/expressions.json` (has()/run.params/loop-condition kitchen-sink).

### 1.6 — Canonical example definitions & golden fixtures ✅

`examples/definitions/` at the repo root with five canonical documents —
`linear.json` (straight chain), `fanout.json` (parallel fan-out into
`join all`; the M6 `ctl submit` demo target), `conditional_branch.json`
(branch with two `when` arms + trailing default reconverging on a join),
`critic_loop.json` (writer↔critic loop edge with conditioned exit), and
`kitchen_sink.json` (one coherent research-and-publish pipeline exercising all
11 step types, both join modes, conditioned/unconditioned edges with `has()`
guards, a loop edge, all five param types, and a `ui` block) — plus a
`README.md` walkthrough. Since strict JSON has no comments, each example's
"header comment" is its top-level `description` field. Golden-wired into the
M1 suite via `internal/dag/examples_test.go`: the file list is pinned (drift
fails), every example must decode, validate with zero issues (warnings
included), and round-trip losslessly (shared `assertLosslessRoundTrip` helper
extracted from the 1.2 test), and a coverage test pins kitchen-sink to the
registered step/param-type catalogs (exposed through `export_test.go`) so
adding a type without extending the example fails CI; M17.2 consumes the same
files directly.

### Post-M1 audit & hardening pass (2026-08-08)

Verified every 1.1–1.6 acceptance criterion against the code and landed a
hardening pass: empty `params: {}` now decodes to a nil map so
decode→encode→decode stays lossless (new fixture `valid/empty_params.json`);
`checkGraph` ignores edges with unresolved endpoints so a ghost endpoint can
no longer cascade into a spurious `no_entry_step` or suppress an
isolated-step warning (the `unknown_edge_endpoint` fixture now also expects
two isolated-step warnings, via the new `structuralWarnCases` side-table); an
internal test pins `stepTypes` (schema/doc order) to `stepConfigTypes`
(decoder registry) so the two step-type catalogs can't drift; the
invalid-decode corpus expectations are pinned bidirectionally to the fixture
directory; the rapid readiness driver also seeds runtime skips (engine
when-false/branch outcomes) and a test covers the config-less join falling
back to the all-shaped rule; the kitchen-sink coverage test additionally pins
`has()` usage; ADR-003 records duplicate-JSON-key last-wins decoding as an
accepted limitation; `docs/expressions.md` clarifies that only
`invalid_expression` carries `line:col` positions (`expression_not_boolean`
has none).

**Known deferred quirk:** an explicit `max_iterations: 0` on a loop edge
reports `loop_field_required` rather than a range violation, because 0 is
indistinguishable from absent without a decoder-level presence flag — the
rejection itself is correct; revisit only if the message ever matters.

## Milestone 2 — Durable state: Postgres persistence

### 2.1 — Docker Compose dev stack ✅

Root `docker-compose.yml` (Compose v2, top-level `name: agentloom`, no
obsolete `version:` key) with `postgres:16-alpine` (pg_isready healthcheck
with initdb `start_period`) and `redis:7-alpine` (AOF via `--appendonly yes`
so down/up durability doesn't depend on RDB snapshot timing; `redis-cli ping`
healthcheck), both on named volumes (`postgres-data`, `redis-data`); dev-only
credentials and host ports default inline via `${VAR:-default}` interpolation
so `make up` works on a clean checkout with no `.env`, with `.env.example` as
the committed override template (`POSTGRES_USER/PASSWORD/DB`,
`AGENTLOOM_POSTGRES_PORT`, `AGENTLOOM_REDIS_PORT`; `.gitignore` gained
`!.env.example` since `.env.*` would otherwise swallow it); Make targets `up`
(`docker compose up -d --wait` — healthchecks gate the exit code), `down`
(volumes kept), `psql`/`redis-cli` (exec into the containers; psql resolves
user/db from the container env so overrides Just Work), and `nuke`
(`down -v --remove-orphans`, gated on typing literal `yes`, declining exits
non-zero); README gained a dev-stack quickstart (incl. the `make nuke`
destruction warning) and status/layout freshening. Verified end-to-end
locally: clean boot to healthy, data survives `make down && make up`
(Postgres row + Redis key), nuke decline/accept paths, post-nuke boot is
empty, and port overrides work. No api/worker services yet — those join the
compose file in M4.

### 2.2 — Migration tooling & integration-test harness ✅

**Migrations.** golang-migrate v4 as a library (iofs source over `embed.FS`,
pgx/v5 database driver) wrapped in `internal/store/migrate.go`: `Migrator`
with `Up` (all pending; nothing-pending is a no-op), `Down` (**one step**,
via `Steps(-1)` — not golang-migrate's roll-back-everything `Down`), `Force`
(recovery; -1 = "nothing applied"), `Version` (with an explicit `applied`
bool instead of leaking `ErrNilVersion`), `Close`. Migration files live in
`internal/store/migrations/` with sequential `NNNN_` numbering; ships
`0001_baseline` (a `schema_baseline` marker table) purely to prove the
machinery — schema v1 arrives in 2.3. Non-obvious: golang-migrate's pgx/v5
driver registers URL scheme **`pgx5`**, so `toMigrateDSN` rewrites
`postgres://` → `pgx5://` (the driver restores it before connecting);
keyword/value DSNs are rejected with a clear error, and errors never echo
the DSN (it can embed credentials). Dirty state is surfaced as typed
`*store.DirtyError` naming the version and the `migrate force` recovery
path. Constructor `NewMigratorFS` takes an injected `fs.FS` so tests can
feed broken migrations.

**CLI.** `cmd/migrate` (plain flag parsing; cobra waits for `ctl` in M6):
`up` / `down` / `status` / `force <ver>` / `new <name>` (snake_case-enforced,
next sequence number, writes the `.up.sql`/`.down.sql` pair — must run from
the repo root). Target DSN from new config sub-config
`PostgresConfig{DSN}` (`AGENTLOOM_POSTGRES_DSN`, default = compose stack;
passed through opaquely — the driver produces better errors than any
up-front validation). Make targets `migrate-up`, `migrate-down`,
`migrate-new name=...`.

**Harness.** `internal/store/storetest`: `NewDB(tb)` (fresh database,
migrated, pooled) and `NewEmptyDB(tb)` (no migrations; returns the DSN too,
for the migration tests themselves); databases are named
`agentloom_test_<random>` and dropped on cleanup with
`DROP DATABASE ... WITH (FORCE)`. Non-obvious: `CREATE DATABASE` is
serialized via a Postgres **advisory lock**, because concurrent creates
cloning template1 can fail and `go test` runs packages as separate OS
processes — a Go mutex can't cover that. Admin DSN from
`AGENTLOOM_TEST_POSTGRES_DSN` (deliberately *not* `AGENTLOOM_POSTGRES_DSN`,
so re-pointing dev tools never silently redirects tests), default = compose
stack; unreachable Postgres **fails** with a `make up` hint rather than
skipping, since `-tags integration` is explicit opt-in. Per-test migration
is fast while the schema is tiny; a template-database fast path is the noted
optimization once 2.3's schema lands.

**Wiring.** `make test-integration` = `go test -race -tags integration ./...`
(tagged tests live next to their code; `test/` keeps cross-cutting suites —
currently `test/smoke`, a dependency-free Redis `PING` over TCP so a broken
service definition is caught before M3). CI gains a `test-integration` job
with postgres:16-alpine + redis:7-alpine **service containers** (images,
credentials, and healthchecks mirroring docker-compose.yml).
`.golangci.yml` sets `run.build-tags: [integration]` so tagged files are
linted. The Makefile now **auto-loads `.env`** (`include .env` + `export`)
so compose and the Go tooling agree on one override file; `.env.example`
documents the three new vars (`AGENTLOOM_POSTGRES_DSN`,
`AGENTLOOM_TEST_POSTGRES_DSN`, `AGENTLOOM_TEST_REDIS_ADDR`).

Verified locally against the compose stack: migrate round-trip
(up → no-op up → down → up), dirty-state error + `force -1` recovery, CLI
paths (status/up/down/new, valid and invalid), parallel-isolation test, no
leftover test databases, and the full suite green with a host Postgres
squatting on 5432 via `.env` port overrides (which is also what surfaced
the need for Makefile `.env` loading). Integration tests double-run unit
tests under the tag — accepted redundancy for a simple `./...` invocation.

### 2.3 — ADR-004 & core schema v1 ✅

**ADR-004** (`docs/adr/004-persistence-model.md`): state-machine tables +
append-only event log, explicitly *not* event sourcing (events are written
in the same tx as the transition they record and never replayed — replay
would smear ADR-002's "committed or not" recovery story). Statuses are
**TEXT + named CHECK constraints, not native enums** — the vocabulary
grows in ≥4 later milestones and `ALTER TYPE ... ADD VALUE` has
transactional restrictions, while drop-and-re-add of a named CHECK is
plain transactional DDL (that recipe is the documented evolution path).
Full allowed-transition matrix for runs and steps, including
future-milestone rows (parked/cancelling/awaiting_human/dead_lettered…)
tagged with their owning milestone so ADR-006/017 refine rather than
contradict. Runs are created directly as `running` (no pending-run state —
instantiation marks entry steps ready in the creating tx).

**Dependency bookkeeping** deliberately goes beyond the ticket's literal
"`remaining_deps` counter": **two counters + per-edge resolutions**.
`remaining_deps` (unresolved incoming normal edges) alone cannot express
`join any`, distinguish ready from skip-propagation, or make retried
completion txs idempotent. So `run_steps` carries `remaining_deps` +
`fired_deps`, and `run_edges` carries `resolution`
(unresolved|fired|skipped) as the idempotency/audit record (retried txs
only touch still-unresolved edges — no double-decrement). The ADR maps
each `dag.ReadySteps` rule to its counter form (ready = `remaining=0 ∧
fired≥1`; join-any = `fired≥1`; skipped = `remaining=0 ∧ fired=0`; failed
parents leave edges unresolved → dependents permanently blocked, matching
M1). Loop edges are excluded from all bookkeeping.

**Schema v1** (`0002_core_schema_v1`): `workflow_definitions` (unique
name+version, immutable rows), `runs` (definition **snapshot JSONB** on
the run + nullable RESTRICT FK to the registry; `graph_version`;
`next_seq` — per-run event seq allocated via `UPDATE … RETURNING`,
serializing appends on the run row those txs already touch for the
aggregate counters `steps_total/succeeded/failed/skipped`), `run_steps`
(PK `(run_id, step_id)`, step_id TEXT because M13/M14 instances are
`{id}#k`; materialized `step_type`+`config`; `claim_id` fencing token),
`run_edges` (PK `(run_id, ordinal)` — **ordinal preserves declaration
order, which the branch first-match rule needs**; no FK on endpoints by
design, expansion revalidates in-tx), `step_attempts` (composite FK,
`outcome` nullable TEXT with no CHECK — taxonomy is ADR-006's), `events`
(PK `(run_id, seq)`), `task_outbox` (identity PK = drain order; **drained
rows are deleted**, row-exists ⇔ dispatch-pending; no FK so dispatch
never blocks on run-row churn). Hot-path indexes: partial
`run_steps (status, updated_at) WHERE status IN ('ready','running')` for
the reconciler, `runs (status, created_at DESC)`, unique partial
`runs (idempotency_token) WHERE NOT NULL`. Lifecycle timestamps are
app-written from the injected clock; only `created_at` defaults to
`now()` (time-injectable invariant).

**Tests.** `TestMigrateUpDownRoundTrip` was reworked — it silently assumed
the latest migration is the baseline (one-step `Down` then asserting
`schema_baseline` gone), which 0002 broke; it now asserts on the newest
migration's tables, checks one-step Down leaves earlier migrations
untouched, and **walks every down migration to zero** so the harness stays
honest as migrations accumulate (`latestVersion` const to bump per
migration). New `schema_v1_integration_test.go`: tables + named indexes
exist, CHECK/unique/FK constraints behave (invalid statuses, duplicate
`(run_id, seq)` event, duplicate idempotency token + two-NULLs-allowed,
duplicate `(name, version)`, attempt-without-step), run delete cascades
the whole subtree, definition-with-runs delete RESTRICTed. The
`schema_baseline` marker table from 0001 stays (storetest's isolation
canary). Deferred: the storetest template-database fast path noted in 2.2
(worth doing now that per-test migration applies a real schema).

### 2.4 — Store layer & transaction helpers ✅

**sqlc.** Pinned via `go run github.com/sqlc-dev/sqlc/cmd/sqlc@$(SQLC_VERSION)`
(v1.30.0, Makefile var — no global install), configured in root `sqlc.yaml`:
schema = the migrations directory (sqlc understands golang-migrate naming and
skips `.down.sql`), queries in `internal/store/queries/*.sql`, output package
`internal/store/gen`. Type overrides keep `pgtype` entirely out of the store
API: `uuid` → `github.com/google/uuid.UUID` (nullable → pointer), `jsonb` →
`json.RawMessage` (nil ⇔ NULL), `timestamptz` → `time.Time` / `*time.Time`
(the override db_type must be spelled bare `timestamptz`;
`pg_catalog.timestamptz` silently doesn't match), plus
`emit_pointers_for_null_types` for the remaining nullable scalars. No pgx
UUID codec is registered — `uuid.UUID`'s `sql.Scanner`/`driver.Valuer`
fallback is correct; native-codec registration is a measured-need
optimization (M19). `make generate` runs sqlc after the JSON-Schema gen; the
CI drift step's diff now also covers `internal/store/gen`; the gen directory
is excluded from golangci-lint linters *and* formatters.

**Design decisions.** (1) **Generated types are the domain types** — repos
return `gen.Run`, `gen.WorkflowDefinition`, etc. directly; a parallel
hand-written struct set would be pure duplication today. (2) **No generic
Update methods**: the "U" of CRUD is deliberately absent from runs/steps —
every status/counter mutation must be a 2.6 guarded CAS, and a plain
`UpdateRunStatus` would be exactly the unguarded write path the invariants
forbid. `events` has no update *or* delete queries (append-only enforced by
API surface, per ADR-004). (3) The ticket's list names six repos but the
schema has seven tables — **run_edges lives on `StepRepo`**
(CreateEdge/ListEdgesByRun): instantiation and expansion always write steps
and edges as one graph. (4) Named sqlc args (`@ids::bigint[]`,
`@token::text`) where positional inference produced `dollar_1` or pointer
params.

**Layer.** `Store` = pgxpool + embedded `repos`; `Open(ctx, dsn)` (parse →
pool → ping; parse errors never echo the DSN — it can embed credentials) and
`NewFromPool` (what tests use over storetest pools). `Querier` interface
bundles the six repo accessors and is implemented by both `Store` (pool
execution) and the handle `WithTx` passes its callback (tx execution) via
one `repos` struct over `gen.Queries`. Repos map errors through one
`wrapErr`: `pgx.ErrNoRows` → `ErrNotFound` (also deletes that matched
nothing), unique/FK violations → `*ConflictError` carrying the constraint
name (`pgerrcode` promoted to a direct dep). Creation ergonomics: zero run
ID → random UUID; nil params/payload → `{}` (explicit INSERT would bypass
the column defaults); empty step status → `pending`; zero graph_version →
1; empty edge_type → `normal`. `status.go` mirrors the ADR-004 v1 status
vocabulary as constants (CHECK constraints stay authoritative).
`RunRepo.AllocateEventSeq` is the ADR-004 seq primitive
(`UPDATE … RETURNING next_seq`) for 2.5/2.6 to call in-tx with `Append`.

**WithTx.** `WithTx(ctx, fn func(ctx, Querier) error)`: commit iff fn nil;
fn error → rollback, error wrapped with `%w` (typed errors survive —
tested through a ConflictError); panic → rollback via the deferred
safety-net then propagate. Rollback contexts use `context.WithoutCancel` so
a context cancelled mid-fn can't also doom the rollback. **Nested use is
rejected**, not savepointed: the callback ctx carries a marker and a nested
call fails fast with `ErrNestedTx` (without harming the outer tx);
composing code shares the outer Querier.

**Tests** (integration tag, per-test DBs via storetest): definitions CRUD
(round-trip, duplicate name+version, RESTRICT delete with runs on record,
double-delete → ErrNotFound), runs CRUD (token lookup + duplicate-token
conflict, caller-supplied id honored, newest-first list, cascade delete of
the whole subtree), graph round-trip (repo defaults, ordinal-ordered edge
list, loop-edge predicates, duplicate step PK conflict), attempts
(attempt_no ordering, attempt-without-step FK conflict), event seq
allocation (1,2,3…; missing run → ErrNotFound; duplicate seq conflict;
backfill-from-last_seq shape), outbox (drain order, delete-as-drain), and
the WithTx matrix (commit, error rollback, panic rollback+propagate,
nested rejection, cancelled ctx, and a 2.5-in-miniature composed tx:
run+step+seq+event+outbox atomically). Gotcha worth remembering: Postgres
jsonb does not preserve formatting/key order, so JSON round-trip
assertions must compare semantically, not byte-wise. Deferred (again): the
storetest template-database fast path from 2.2.

### 2.5 — Atomic run instantiation ✅

**API.** `Store.CreateRun(ctx, CreateRunArgs)` in
`internal/store/instantiate.go`: takes a decoded `*dag.Definition`
(CreateRun runs `dag.Validate` itself and rejects error-severity issues
before opening the transaction — instantiation's correctness rests on the
structural invariants Validate proves), optional `RunID`/`DefinitionID`,
opaque `Params`, optional `IdempotencyToken`, and a **required injected
`Now`** (becomes `started_at`; runs are created directly as `running` per
ADR-004, so creation is the start — no clock field on Store, time flows in
per call). Returns `CreateRunResult{Run, EntrySteps, Reused}`.
Param-*value* validation against the definition's ParamSpecs is
**deferred to M6** (the submission API); 2.5 stores params opaquely.

**Planning before the tx.** All derivation is pure and precedes the
transaction: snapshot via `dag.Encode`; **entry steps via
`dag.NewGraph` + `ReadySteps(nil, nil, nil)`** — the reference
implementation ADR-004's counters must mirror, not a hand-rolled
"no incoming normal edges" count (a defensive check errors if ReadySteps
ever reports newly-skipped steps at instantiation); `remaining_deps` =
incoming **normal**-edge count (loop edges excluded, so a step whose only
incoming edge is a loop edge — critic_loop's `draft` — is an entry step).

**The transaction** (via `WithTx`): run row (status `running`,
`steps_total`, snapshot, token, `started_at`) → batch `run_steps`
(entry steps `ready`, others `pending`; `config` materialized by
`json.Marshal` of the typed step config, nil ⇒ NULL) → batch `run_edges`
(`ordinal` = declaration index; empty `when`/`condition`/zero
`max_iterations` ⇒ NULL) → events (`run_created` seq 1, payload
`{name, steps_total}`, then `step_ready` per entry step in declaration
order, payload `{step_id}`, each seq via `AllocateEventSeq` in-tx) →
one `task_outbox` row per entry step (reason `step_ready`). Batch inserts
are new sqlc **`:copyfrom`** queries `CreateRunSteps`/`CreateRunEdges`
(pgx binary COPY; regenerating added `CopyFrom` to the gen `DBTX`
interface), surfaced as `StepRepo.CreateBatch`/`CreateEdgeBatch` with the
same per-row defaulting as the single-row methods. New event-type
constants `EventRunCreated`/`EventStepReady` in `status.go`. One `slog`
info line on success (`run_id` via the canonical field helper;
steps_total/entry_steps as plain ints).

**Idempotency.** Token present → pre-check `GetByIdempotencyToken`
(round-trip saver only); the **unique partial index is the authority**: a
racing insert loses with a `ConflictError` naming
`runs_idempotency_token_key`, which CreateRun catches, re-fetches by
token, and returns as the sequential path would. `Reused: true` marks
both paths; `EntrySteps` on the reused path is **re-derived from the
stored snapshot** (not the submitted definition), so the result shape
doesn't depend on which racer won. Conflicts on other constraints
propagate untouched.

**Failure injection.** Unexported package hook
`instantiateFailpoint func(stage) error` consulted after each tx phase
(`after_run_insert`/`after_steps`/`after_edges`/`after_events`); nil in
production, armed via `SetInstantiateFailpoint(tb, fn)` in
`export_test.go` (which also re-exports the stage names). The hook is a
package global — tests arming it must not run parallel with other
CreateRun callers, so `TestCreateRunAllOrNothing` is deliberately not
`t.Parallel()` (Go runs sequential top-level tests to completion while
parallel ones are still suspended).

**Tests** (`instantiate_integration_test.go`): fanout fixture end-to-end
(counters `start:0, branches:1, gather:3, synthesize:1`, entry-only ready,
snapshot round-trip, ordinal-ordered edges all `unresolved`, event
seq/payloads, single outbox row); loop-edge exclusion + predicate
materialization via critic_loop; all five canonical fixtures instantiate;
sequential + racing idempotency (8 goroutines, exactly one non-Reused
winner, one run row); all-or-nothing at every failpoint stage plus a
panic variant (zero rows across all five tables) and a recovery check;
rejections (nil definition, zero Now, invalid definition) write nothing.
Local-dev gotcha: bare `go test -tags integration` doesn't load `.env` —
use `make test-integration` (the Makefile exports it) or source `.env`
when a host Postgres squats on 5432.

Deferred (again): the storetest template-database fast path from 2.2.

### 2.6 — Guarded state transitions (CAS) ✅

**API.** `internal/store/transitions.go`: package-level functions — one per
ADR-004 matrix row owned by 2.6 — each taking the `Querier` a `WithTx`
callback received plus an args struct with a required injected `Now`:
`ClaimStep` (ready → running: fresh `claim_id` generated inside and
returned on the row, `attempt_count`++, attempt row inserted,
`started_at` stamped on first claim only via COALESCE), `SucceedStep` /
`FailStep` (running → terminal, **fenced by `claim_id`**; output/error
persisted, attempt closed via new `FinishStepAttempt` query, run
aggregate bumped), `ResolveEdge` (bookkeeping, below), `ReadyStep`
(pending → ready; guard `fired_deps ≥ 1 AND (remaining_deps = 0 OR
join_any)`), `SkipStep` (pending → skipped; `remaining = fired = 0`),
`SucceedRun` (guard on aggregates: `succeeded + skipped = total AND
failed = 0` — a `COUNT(*)` scan was rejected as hot-path cost; the
counters are maintained by these same transitions in the same txs),
`FailRun` (v1 minimum guard `steps_failed ≥ 1`; *when* to halt is
ADR-006/M5 policy). Queries live in `queries/transitions.sql` and are
**deliberately absent from the public repo interfaces** — same reasoning
as 2.4's no-generic-Update rule: a repo-level raw CAS would let callers
skip the event append. Transitions are the only mutation surface.

**Design decisions.** (1) **In-tx execution is enforced dynamically**:
each function checks the `txMarker` ctx (installed by `WithTx`) and
type-asserts the Querier to the unexported `repos` value only `WithTx`
hands out; a pool-backed `*Store` or a bypassed Querier fails fast with
`ErrNoTx`. Atomicity of CAS + attempt + aggregates + event is structural,
not conventional. (2) **Lock ordering: run row first.** Every
transition's first statement is its event-seq allocation
(`AllocateEventSeq`, an UPDATE on the run row) — without this, a claim
(step → run lock order) racing a composed completion fan-out (run → step)
deadlocks. Uniform run → step → edge ordering makes M4.3's composed
transactions deadlock-free by construction; the per-run serialization it
implies is the trade ADR-004 already accepted. A rolled-back transition
returns its seq (the increment rolls back too), so the log stays
gap-free. (3) **`ReadyStep` takes `JoinAny` from the caller** rather than
parsing `config` JSONB in SQL — the engine holds the decoded definition;
the store stays dumb about step semantics. (4) **`ReadyStep` writes no
outbox row**: instantiation (2.5), unpark (M5.6), and the reconciler
(M4.4) all outbox `ready` steps outside any transition, so dispatch
composes at the caller. (5) **CEL evaluation stays out**: `ResolveEdge`
takes the fired/skipped verdict; evaluation and the branch first-match
rule are M4.3's.

**Typed errors** (`errors.go`): sentinel `ErrConflict`;
`*TransitionError` unwraps to it carrying entity, from/to, and a
`ConflictReason` — `wrong_status` (illegal edge or lost race),
`claim_mismatch` (fencing; carries **both** claim IDs for M4.5's zombie
logging), `guard_failed` (status matched, predicate didn't — e.g.
premature rollup). Diagnosis = re-read the row in the same tx after a
0-row UPDATE; missing row/run → `ErrNotFound` (the run check falls out of
seq allocation). New sentinel `ErrNoTx`.

**ResolveEdge bookkeeping.** `resolution = 'unresolved'` in the WHERE is
the idempotency mechanism (ADR-004): re-resolving to the same verdict is
a **no-op success** (`Resolved: false`, counters untouched — retried
completion txs can't double-decrement); a *conflicting* verdict is an
error (retries must be deterministic); loop edges and missing edges
error; a dangling `to_step` (edges have no FK) surfaces as `ErrNotFound`,
not silent success. On success the dependent's updated row comes back so
the caller can decide ready/skip next. No event — edge resolution isn't
in the matrix or the v1 event vocabulary.

**Events.** Six new type constants (`step_claimed`, `step_succeeded`,
`step_failed`, `step_skipped`, `run_succeeded`, `run_failed`); payloads
v1-minimal (`{step_id, claim_id, attempt_no}` / `{step_id, attempt_no}` /
`{step_id}` / `{}`); 2.5's `stepReadyPayload` renamed `stepIDPayload`
(shared with `step_skipped`).

**Tests** (`transitions_integration_test.go`, +`errors_test.go` unit):
16-goroutine claim race (one winner, losers typed `wrong_status`, one
attempt/claim/event); full step matrix (5 transitions × 6 statuses,
illegal cells assert unchanged status *and* no event); run matrix +
guards; fencing (stale claim → `claim_mismatch` with both IDs; duplicate
completion after real completion → `wrong_status` = ACK-and-drop);
atomicity from outside (transition inside a tx that then errors leaves
zero trace, `next_seq` unwound); fanout lifecycle end-to-end **including
one 4.3-shaped composed transaction** (succeed + 3×resolve + 3×ready in
one `WithTx`), idempotent re-resolve, join-all gating, final aggregates
(6,0,0) and gap-free event log; skip propagation a→b→c with all-skipped
tail still `SucceedRun`-able; join-any readiness + late-firing absorption;
ResolveEdge rejections (loop edge, conflicting verdict, missing, dangling
target); `ErrNoTx` / zero-`Now` rejection. Shared helper
`assertCountersMatchEdges` checks the two ADR-004 bookkeeping
representations agree after every flow (the desync risk its Consequences
section flags).

Deferred (again): the storetest template-database fast path from 2.2.

### Post-M2 audit & hardening pass (2026-08-08)

Verified every 2.1–2.6 acceptance criterion and exit criterion against the
code (including a live `make down && make up` durability check), then
landed a hardening pass. The headline fix is a **latent event-log
corruption in the 2.6 transitions**: the event seq was allocated *before*
the guarded CAS, so a conflict swallowed inside a composed transaction —
which `ReadyStep`'s own doc comment invites for join-any late firings, and
which M4.3's completion transaction is shaped to do — would have committed
an advanced `next_seq` with no event row, a permanent backfill gap. Every
transition now takes the run-row lock first via a new `LockRun`
(`SELECT … FOR UPDATE`) and allocates the seq **after** the CAS succeeds
(inside `appendEvent`), so a rejected transition writes nothing and
dropping typed conflicts is safe by construction;
`TestDroppedConflictKeepsLogGapFree` pins the contract with an M4.3-shaped
transaction. The same change brought `ResolveEdge` — previously the one
function skipping the run lock, a latent deadlock against concurrent
claims for any caller that didn't happen to hold the lock already — under
the uniform run → step → edge ordering. ADR-004's event-sequencing section
now records both rules.

**`run_steps.updated_at` is now app-written on insert** (new column in the
`CreateRunStep`/`CreateRunSteps` queries; the repos reject a zero
`UpdatedAt` like transitions reject a zero `Now`): it previously fell to
the schema's `now()` default, which put freshly instantiated steps outside
the test clock's control — and that column feeds the reconciler's
staleness index (M4.4). ADR-004's timestamp section documents the policy.

**Smaller hardening:** `ApplyEdgeResolution` gained a `remaining_deps > 0`
guard so a counter underflow (unresolved edge into a drained step — graph
corruption) surfaces as a typed diagnosis instead of a raw CHECK
violation; the instantiation failpoint is an `atomic.Pointer` and gained
the missing `after_outbox` stage; `Migrator.Down` on an unmigrated
database says "nothing to roll back" instead of leaking golang-migrate's
internals (which surface as `fs.ErrNotExist`, not only `ErrNilVersion`);
`cmd/migrate` validates the command and `force`'s argument *before*
opening a database connection; `ReadyStep`/`SkipStep` log like the other
transitions; `CreateRunArgs.IdempotencyToken` documents that the token is
the sole identity (the definition is not compared).

**Tests closing audit gaps:** the run transition matrix now asserts
unchanged status *and* no event on every rejection and exercises the
`failed` from-status; `WithTx` has a cancel-mid-callback test (the
`context.WithoutCancel` rollback path); `store.Open` is tested (parse
failure and ping failure never echo credentials; live round-trip);
`toMigrateDSN` has table-driven unit tests; `cmd/migrate` has unit tests
for usage errors and `newMigration` (dir now injected).

**CI:** the integration job's service containers were replaced with
`make up` — GH Actions services can't pass a container command, so CI
Redis silently ran without AOF and Postgres without `start_period`;
booting the actual compose file mirrors local dev by construction. Needs
the one-time GitHub verification (same errand as ticket 0.2's red-path
check).

**Doc corrections (ADR-004):** `retrying` was reserved but had no matrix
rows — added `failed → retrying → ready` and `dead_lettered → ready` (M5);
the three milestone-tag listings for reserved statuses now agree (M5 =
step statuses, M5.6 = run statuses); the failed-parent blocking rule
records its `join any` exception; the skip counter form records its
zero-indegree side condition (matters for M13 expansion); the
`runs (status, created_at DESC)` index no longer claims to serve
unfiltered listing; a new consequence records that step transitions carry
no run-status guard until M5.6's cancellation sweep. README's layout
section caught up with 2.4–2.6; the `cmd/migrate` nolint cites the real
gosec rule (G306, not the nonexistent "G703").

**Still deferred, deliberately:** the storetest template-database fast
path (third deferral) — the full integration suite runs in ~4s, and
cross-process template lifecycle management (hash-keyed templates,
stale-template cleanup, no-connections-during-clone) is real flake surface
for a modest win; revisit when suite time actually hurts. The 1.5
`max_iterations: 0` message quirk stands.

## Milestone 3 — Queue & lease layer (Redis Streams)

### 3.1 — ADR-005: dispatch & lease protocol ✅

Docs-only ticket:
[`docs/adr/005-dispatch-lease-protocol.md`](adr/005-dispatch-lease-protocol.md)
(+ README index row). Fixes the full queue protocol before `internal/queue`
exists; 3.2–3.6 and M4's worker implement against it. Decisions worth
knowing before touching those tickets:

**Envelope = pointer, not payload.** Flat stream field–value pairs (`v`,
`run_id`, `step_id`, `reason`, reserved `traceparent`/`tracestate` for
M7, optional informational `enqueued_at_ms`) — no step config or input,
ever, so duplicates are boring and Redis holds no second truth.
Versioning: additive-within-version (decoders ignore unknown fields),
`v`-bump on incompatible change, consumers-decode-before-producers-emit;
unknown/malformed envelopes are **not ACKed** — they ride the delivery
count into the poison path rather than being dropped silently.

**Lease = PEL entry; the two-layer split is normative.** PEL answers
liveness (claim `XREADGROUP >`, heartbeat `XCLAIM JUSTID` to self at
~TTL/3 — JUSTID is load-bearing, it keeps the delivery counter a pure
poison signal; reclaim `XAUTOCLAIM` min-idle = lease TTL, which *does*
increment the counter); Postgres `claim_id` answers correctness. A
spurious reclaim wastes one execution but can never corrupt state.
Consumer names are per-incarnation (restart = new consumer, no self-PEL
replay; recovery is uniformly the reclaim path), which is why the
orphan janitor (`XGROUP DELCONSUMER`, zero-pending guard) exists.

**ACK discipline table + takeover rule.** ACK only after the consuming
Postgres tx commits; enumerated ACK-and-drop cases (terminal step,
`running` on a *fresh* delivery, dangling ref). `running` on a
*reclaimed* delivery = lease-expiry takeover (`running → ready` clearing
`claim_id`, then fresh claim) — duplicate-vs-crash is distinguished **by
delivery path**, not guessing. One accepted race documented: a crashed
duplicate's later reclaim can steal a live claim — rare, bounded, fenced.

**Crash matrix W1–W5/P1–P2/R1** each cell with recovery + where it's
proven (3.3/3.4/3.6/4.2/4.4/4.5/4.7/5.8), per the M3 exit criterion.
Every recovery reduces to "redeliver, claim CAS decides" or "reconciler
re-outboxes from Postgres." R1(c) (Redis loss + dead worker) is the slow
path: reconciler staleness sweep with threshold ≫ TTL because
`updated_at` moves on transitions, not heartbeats — recorded as an
accepted consequence, with heartbeat-to-Postgres explicitly rejected.

**Delayed delivery** (`sched:delayed` ZSET): Lua pop+`XADD` is atomic,
`now` is caller-supplied (injectable clock — the script never reads Redis
time); ZSET member semantics ("identical envelope re-ZADD moves the fire
time, doesn't duplicate") recorded as the tenant contract — M5.2 must
make retry envelopes distinct (e.g. attempt number) if it ever needs two
pending dispatches. Delayed entries are scheduling state, not truth;
every tenant needs re-derivable Postgres state behind them.

**Tuning defaults** (lease TTL 30s, heartbeat TTL/3 ± 20% jitter, poison
threshold 5, reclaimer TTL/2, promoter 1s, janitor 10m/1h) live in the
ADR's table; 3.2–3.5 wire them through config. One deliberate exception
to the injectable-clock invariant: PEL idle time is Redis-server-side —
tests shrink TTLs instead of faking that clock.

No code changed; `make test` green as a sanity check. Deferred (fourth
time): the storetest template-database fast path.

### 3.2 — Stream primitives: producer & group bootstrap ✅

First code in `internal/queue`: the dispatch-side primitives of ADR-005
that 3.3–3.5 build on. `Open` (go-redis v9 client + ping, mirroring
`store.Open`'s connect-and-verify shape); `Queue` bound to one stream +
group pair (`New(client, stream, group)`, empty names default to
`steps:ready`/`workers` — parameterized because M19 sharding and test
isolation both want per-instance names); `Enqueue` (validate → encode →
`XADD`, no trimming — at-least-once by design, dedup is the claim CAS's
job); `EnsureGroup`; `Stats`; the envelope codec; `NewConsumerName`.
New config sub-config `RedisConfig{Addr}` (`AGENTLOOM_REDIS_ADDR`,
default = compose stack, passed through opaquely like the Postgres DSN);
`.env.example` documents it alongside the pre-existing test-only
`AGENTLOOM_TEST_REDIS_ADDR`.

**Envelope codec.** Flat field–value pairs per ADR-005, `Encode()
(map[string]any, error)` validates required fields so a malformed envelope
never reaches the wire; empty optional fields are omitted, not written as
`""`. `DecodeEnvelope` takes `map[string]any` (the `XMessage.Values`
shape): missing/non-integer `v` → `*MalformedEnvelopeError{Field}`;
well-formed `v != 1` → `*UnknownVersionError{Version}`; both unwrap to
sentinel `ErrBadEnvelope`, the one thing 3.3's no-ACK branch needs to
test. Unknown fields are ignored (additive-evolution rule) and — decided
here — **`reason` is presence-checked but not vocabulary-checked**:
enforcing the enum at decode would make every future reason (M4.4, M5.2,
M5.4, M5.6, M15) a breaking decoder change, contradicting
additive-within-version. Producer-side constants (`ReasonStepReady`) keep
the vocabulary honest. `EnqueuedAt` is caller-supplied — the queue library
holds no clock at all, satisfying the injectable-time invariant by
construction.

**Consumer-name format (decision ADR-005 delegated to 3.2):**
`<hostname>-<pid>-<8-hex-crypto-random>`. Only the random suffix carries
the per-incarnation uniqueness guarantee; host/pid exist for operators
reading `XINFO CONSUMERS` mid-incident.

**Group bootstrap.** `XGROUP CREATE ... 0 MKSTREAM` — start ID **`0`, not
`$`**, because outbox drain can race worker startup and pre-boot entries
must still be delivered (pinned by test). `BUSYGROUP` reply = success;
go-redis surfaces Redis error replies as plain errors, so this is a
string-prefix match by necessity (same for `NOGROUP` in `Stats`).
Idempotency verified to not reset an existing group's last-delivered-id.

**Introspection.** `Stats` = `XLEN` (ready depth) + `XPENDING` summary
(total + per-consumer pending, sorted for determinism) — the M7 metric /
M13 system-stats surface. A missing group fails with typed `ErrNoGroup`
rather than fabricating zeros.

**Tests.** Unit: codec round-trips (table + `rapid` property at ms
precision), every malformed-field case with `errors.As` on the field,
unknown-version, unknown-field tolerance, unknown-reason acceptance,
consumer-name uniqueness. Integration (compose Redis, unique
`agentloom-test:<random>:steps:ready` keys per test, deleted on cleanup —
a local helper 3.6's `queuetest` harness will absorb): produce→
`XREADGROUP`→decode round-trip, pre-group-creation delivery, 16-goroutine
`EnsureGroup` race (exactly one group), re-ensure leaves
last-delivered-id untouched, stats before/after unacked reads with
per-consumer breakdown, and malformed/unknown-version entries `XADD`ed
raw and decoded off the real wire.

Deliberately no logging in this ticket: `Enqueue` is a library call whose
callers (the M4 drainer, reconciler, promoter) own the hot-path logs, and
the loops that warrant them arrive in 3.3+. Deferred (fifth time): the
storetest template-database fast path.

### 3.3 — Consumer loop with ack/nack semantics ✅

`Consumer` in `internal/queue`: a blocking `XREADGROUP` batch loop
(`q.NewConsumer(name, handler, cfg)` + `Run(ctx)`) feeding deliveries to
a `Handler func(ctx, Delivery) error` one at a time. Nil return → `XACK`;
error → no ACK (entry stays in the PEL for reclaim); panic → recovered
via `safeHandle` (unit-tested in isolation) and treated as an error, so
one poisoned message can never kill the loop. Decode failures (unknown
version / malformed, ticket 3.2's typed errors) never reach the handler
and are never acked, per ADR-005's no-silent-drop rule. New
`config.QueueConfig{ConsumerBatch, ConsumerBlock}`
(`AGENTLOOM_QUEUE_CONSUMER_BATCH`/`_BLOCK`, defaults 16/5s per the
ADR-005 tuning table); `queue.ConsumerConfig` zero values fall back to
the same defaults.

**One per-message path, pre-wired for 3.4.** `process(ctx, msg,
deliveryCount)` is the single decode→handle→ack path; `Run` feeds it
fresh reads and 3.4's reclaimer will feed it `XAUTOCLAIM`ed entries. This
matters because an entry reclaimed to consumer B is *already delivered* —
B's `XREADGROUP >` loop will never see it, so redelivery can only enter
through this path. The kill-pre-ACK test replays a reclaimed entry
through it via a test-only export (`export_test.go`), exactly as the
reclaimer will in production code.

**Delivery count without a round-trip.** `XREADGROUP >` only delivers
never-before-delivered entries, so the fresh path passes a constant 1 —
no `XPENDING` lookup per batch. Real counts arrive with the reclaim path
(3.4); `Delivery.DeliveryCount` surfaces the contract now so it does not
change later.

**Shutdown = drain, then detached ACK.** Handling is synchronous in
`Run`'s loop, so cancellation mid-handler naturally waits for the handler
to return; the subsequent `XACK` runs on a
`context.WithoutCancel`-derived timeout context so a handler that
succeeds during shutdown still acks instead of redelivering finished
work. Pinned by test: `Run` returns only after the in-flight handler
finishes, and the PEL is empty afterwards.

**Shutdown latency is bounded by Block, not instant.** go-redis does not
interrupt a blocked `XREADGROUP` on context cancellation — the loop
observes cancellation between blocking reads. This is ADR-005's stated
rationale for the 5s default ("keeps shutdown latency and liveness
checks bounded"); the test pins the bounded contract with a 200ms Block.
Read errors back off (`ErrorBackoff`, default 1s) instead of hot-spinning;
that timer is the package's one deliberate real-time wait — tests tune it
down rather than injecting a clock, the same convention ADR-005 uses for
lease TTLs.

**Layering catch recorded for future tickets:** `config` must not import
`queue` (queue logs through `internal/obs/log`, which imports `config`),
so the 16/5s defaults are duplicated in both packages with keep-in-sync
comments rather than shared constants.

**Tests.** Unit: config defaulting/env parsing (including invalid batch
and duration values), `safeHandle` panic containment with stack capture,
consumer-name generation, nil-handler panic. Integration: 40-envelope
multi-batch happy path (each handled exactly once, `DeliveryCount` 1,
PEL drained); handler error leaves the entry pending; scripted panic on
one entry leaves it pending while the next entry in the same batch is
still handled; malformed raw entry never reaches the handler and stays
pending; shutdown drains the in-flight handler and still acks its
success; idle-block shutdown bounded by the configured Block; and the
flagship kill-pre-ACK redelivery test — consumer A killed after delivery,
PEL entry intact with delivery count 1, `XAUTOCLAIM` hands it to B with
count 2 (asserted via `XPENDING`), B completes and acks through the
shared path, PEL empty.

A note left in `Run` for 3.4: a full batch becomes PEL leases at read
time, but queued entries sit un-heartbeated behind the sequential handler
— safe (reclaim duplicates die at the claim CAS) but a real batch-size ×
lease-TTL tuning interaction. Deferred (sixth time): the storetest
template-database fast path.

### 3.4 — Lease heartbeat & reclaimer ✅

The full lease protocol of ADR-005 on top of 3.3's consumer:
heartbeater (`heartbeat.go`), reclaimer + poison path (`reclaim.go`),
orphan-consumer janitor (`janitor.go`). `ConsumerConfig` grows `LeaseTTL`
(30s), `HeartbeatInterval`, `ReclaimInterval`, `PoisonThreshold` (5),
`PoisonHandler`, `JanitorInterval` (10m), `JanitorIdleThreshold` (1h);
`config.QueueConfig` mirrors them (`AGENTLOOM_QUEUE_LEASE_TTL` etc.), with
the existing keep-in-sync duplication (config ↛ queue import rule). The
duplicated env-parsing in `applyEnv` was refactored into
`applyPositiveInt`/`applyPositiveDuration` helpers while adding the six
new variables.

**Intervals derive from the TTL by default.** `HeartbeatInterval`/
`ReclaimInterval` zero-values derive `LeaseTTL/3` and `LeaseTTL/2` (in
both config and queue layers), so a TTL-only override preserves ADR-005's
two-missed-beats-still-precede-expiry margin; explicit overrides remain
possible. Jitter is ±20% recomputed per beat (`math/rand/v2`).

**Duties run inside `Run`'s loop, not as goroutines.** The reclaimer and
janitor are deadline-checked between blocking reads (the read's `BLOCK` is
capped at the next due duty so an idle consumer's reclaim cadence is
governed by `ReclaimInterval`, not `Block`). Deliberate: 3.3's contract is
strictly serialized handler invocations per consumer, and a concurrent
reclaim goroutine would either break that or hold freshly-claimed leases
un-heartbeated behind a mutex. A consumer busy in a long handler simply
doesn't reclaim — any idle consumer in the fleet does (leaderless recovery
per ADR-005). The heartbeater is the one goroutine, started inside
`process` around the handler call (so fresh and reclaimed deliveries are
heartbeated identically), fully joined before the ack decision, and
issuing `XCLAIM JUSTID` on a detached context so a handler draining
through shutdown keeps its lease. Heartbeat failure is logged, never
fatal — R1(b)'s prescribed behavior (the Postgres `claim_id` fence is the
correctness layer).

**Delivery counts after `XAUTOCLAIM` need a lookup.** go-redis's
`XAutoClaim` returns messages without retry counts, so the reclaimer
issues one pipelined per-entry `XPENDING` — exact regardless of what else
the consumer holds, unlike a single range query, which interleaved entries
could truncate. An ID missing from the lookup means the previous holder's
completion acked it between claim and lookup (`XACK` needs no ownership) —
skipped, work already done. The `XAUTOCLAIM` cursor persists across ticks
(bounded per-tick batch = `Batch`, full PEL coverage over successive
passes; pinned by a 10-entries-batch-4 sweep test).

**Poison contract.** Checked on the reclaim path only (`count >
PoisonThreshold`, before decode): diverted entries go to
`PoisonHandler(ctx, PoisonMessage)` — `Values` always carries the raw
entry and `Envelope` is nil when undecodable, which is how malformed
envelopes (a poison source by design) finally get dead-lettered with
contents preserved. Nil callback return acks (M5.4 wires dead-lettering
before that ack); error or a contained panic leaves the entry pending for
the next pass. No callback configured → log-and-leave-pending: visible
spin, never a silent drop.

**Janitor.** `XINFO CONSUMERS` → `XGROUP DELCONSUMER` for consumers that
aren't self with zero pending entries and idle beyond the threshold. The
zero-pending guard is the safety argument (DELCONSUMER drops PEL state);
delete races between workers are benign no-ops.

**Tests.** Unit: config defaulting/env parsing for the six new knobs,
TTL-derivation ratios, jitter bounds (1000 samples in [0.8i, 1.2i]).
Integration (all at tuned-down TTLs of 300–500ms, per the ADR-005
convention — the Redis server's idle clock is the one clock consumed
rather than injected): the flagship kill-mid-task test (A dies, B's
reclaimer completes the entry within TTL + ε with `DeliveryCount` 2 —
the reclaim-increments half of the JUSTID criterion); heartbeated 3.5×TTL
task never reclaimed while a hungry reclaimer ticks next to it, with
mid-task PEL sampling asserting owner stays A and count stays 1 (the
JUSTID-doesn't-inflate half); the error→reclaim→error ladder walking an
entry into the poison callback at exactly threshold+1 with the handler
invoked exactly threshold times; malformed-envelope poison with raw
contents preserved and nil `Envelope`; nil-callback poison staying
visibly pending while the loop serves other entries; cursor sweep;
janitor deletes the idle empty consumer but spares both the
pending-holding one and itself.

**Flake fixed during development:** the heartbeat test originally started
consumers A and B together and B could win the fresh-delivery race; B now
joins only after A holds the entry. Deferred (seventh time): the
storetest template-database fast path.

### 3.5 — Delayed delivery (ZSET promoter) ✅

`Delayed` in `internal/queue` (`delayed.go`): the ADR-005 delayed-delivery
contract. `q.NewDelayed(key)` binds a sorted set (empty key →
`sched:delayed`; parameterized like the stream because tests want unique
keys and M19 stream sharding will need per-shard delayed keys — a
promoter XADDs to its own queue's stream). `Schedule(ctx, env, fireAt)`
validates via the envelope codec (a malformed envelope never reaches the
ZSET), rejects a zero `fireAt` (the store layer's zero-`Now` guard
pattern), and plain-`ZADD`s member = encoded envelope, score = fire-at
epoch ms. `PromoteDue(ctx, now, limit)` runs one atomic Lua pass; `Len`
(`ZCARD`) feeds M7 depth and 3.6's delayed-empty quiescence assertion.
New `ConsumerConfig{PromoterTick, DelayedKey}` +
`config.QueueConfig.PromoterTick` (`AGENTLOOM_QUEUE_PROMOTER_TICK`,
default 1s per the tuning table, usual keep-in-sync duplication);
`.env.example` documents it.

**Member encoding.** A ZSET member is one string but the envelope codec
speaks flat field–value pairs, so the member is the JSON object of
`Encode()`'s map — `json.Marshal` sorts map keys, making identical
envelopes byte-identical, which is exactly the property ZADD's
move-the-fire-time dedup semantics rest on (pinned by a determinism unit
test and a move-fire-time integration test). The Lua script
`cjson.decode`s the member back into `XADD` args, so a promoted stream
entry is identical to what `Enqueue` would have written (pinned by
decode-off-the-real-wire assertions).

**The Lua script** (`ZRANGEBYSCORE … WITHSCORES LIMIT 0 batch` → per
member `XADD` + `ZREM`) is the atomicity guarantee: no crash point
between removed-from-set and added-to-stream, and concurrent promoters
serialize on script execution — the 8-goroutine × 200-entry stress test
asserts zero loss and zero duplication. `now` is `ARGV`, passed from the
caller's injectable clock; the script never reads Redis server time, so
the due/not-due tests drive promotion with fully synthetic instants.

**Quarantine decision (recorded in ADR-005).** A member that does not
decode to a flat string→string object is `RPUSH`ed to `<key>:malformed`
and `ZREM`ed instead of promoted. Rationale: only `Schedule` writes the
set so such a member should be unreachable, but an unguarded
`cjson.decode`/`XADD` failure aborts the script mid-pass, and a bad
member's due score would re-select it first on every tick — wedging every
promoter in the fleet forever. Quarantine preserves raw contents for an
operator (no-silent-drop rule, same shape as the poison path's
raw-`Values` guarantee); the consumer duty logs quarantines at error
level.

**Latency observable (M7 hook).** The script returns the promoted
members' scores; `PromoteResult{Promoted, Quarantined, MaxLag}` computes
`MaxLag = max(now − fireAt)`, asserted exact in the fake-clock test and
logged (`count`, `max_lag`) by the consumer duty — the M7 histogram slots
in behind this.

**Consumer wiring.** The promoter is a third in-loop duty next to the
reclaimer and janitor (`nextPromote`, included in the block-cap
computation so an idle consumer's promote cadence is governed by
`PromoterTick`, not `Block`) — preserving 3.3's strictly-serialized
handler contract. The duty passes `time.Now()` as `now`, consistent with
how `Run` already schedules duties; fake-clock coverage targets
`PromoteDue` directly. Promotion errors log and retry next tick, like the
reclaimer.

**Tests.** Unit: member determinism ×20, member→`DecodeEnvelope`
round-trip, encode-time validation, key defaulting +
`MalformedKey` derivation, zero-`fireAt`/zero-`now` guards (nil-client
proof they precede any Redis command), `PromoterTick`
defaulting, config env parsing. Integration: fake-clock due/not-due with
exact `MaxLag` (1s early → lag 1s; on time → 0); move-fire-time (two
`Schedule`s of one envelope → `ZCARD` 1, nothing at the old fire time,
one promotion at the new); batch bound (10 due, limit 4 → exactly the 4
oldest promote); the concurrency stress; 3-variant malformed quarantine
(non-JSON, `{}`, JSON array) with raw contents preserved while the valid
member still promotes; end-to-end consumer test (tuned-down 50ms tick →
scheduled entry reaches the handler and acks, delayed set empty).

Deferred (eighth time): the storetest template-database fast path.

### 3.6 — Queue chaos harness ✅

`internal/queue/queuetest`, the chaos harness the M4/M5 chaos tickets
build on, closing M3. Shaped like `storetest`: a plain untagged package
importing `testing`, connecting via `AGENTLOOM_TEST_REDIS_ADDR`
(fail-fast, never skip), with its own integration-tagged self-tests.
Each `Harness` owns uniquely prefixed keys (stream, delayed set,
quarantine list; deleted on cleanup) and a shared `Journal` every spawned
consumer records into. All four M3 integration test files were refactored
onto it — the local helpers (`newTestQueue`, `rawClient`, `readOne`,
`startConsumer`, `fastCfg`/`leaseCfg`, `waitForStats`, `pelSnapshot`,
`newTestDelayed`, `streamEnvelopes`) are gone; every asserted contract is
unchanged.

**The one production change: `ConsumerConfig.PhaseHook`.** Crash cell W4
(die after work, before ACK) cannot be provoked from outside the queue
package — after a successful handler the ack is unconditional and runs on
a detached context, so no cancellation can suppress it, and
`export_test.go` is invisible to a sibling package. `process` therefore
calls an optional hook at two instrumented points (`PhasePreHandle` after
decode, `PhasePreAck` after handler success + heartbeat stop); a non-nil
error aborts the message un-acked. Nil in production, documented as
test instrumentation; because `process` is the single per-message path,
the hook covers fresh and reclaimed deliveries alike. Mid-handle needs no
hook — a scripted `Hang` plus the kill switch composes it.

**Kill switches.** `Spawn` runs `Consumer.Run` under a per-consumer
cancelable context; `Kill()` cancels and joins (idempotent, registered as
cleanup). `KillAt(phase, step)` arms a one-shot kill delivered through
the PhaseHook: on match it cancels the consumer and returns an error, so
the message aborts and the loop dies exactly as a crashed process would —
`Killed()` signals the trigger. Spawn also defaults an empty
`cfg.DelayedKey` to the harness's isolated delayed set: the previous
tests' consumers silently promoted from the *global* `sched:delayed` on
the shared test Redis — benign only because nothing ever scheduled into
it; a promoter XADDs to its own queue's stream, so two parallel tests
sharing the default key would promote each other's entries onto the
wrong streams. The harness closes that latent hazard.

**Scripts and journal.** `Script.Handle` is a `queue.Handler`: per-step
action sequences (`Succeed`/`Fail`/`PanicWith`/`Hang`) consumed one per
invocation, last action sticky, `Default` for unscripted steps, `Release`
to unblock hangs (close-once semantics — a pre-release passes straight
through). Bespoke handlers remain first-class (`Spawn` takes any
handler): the refactored M3 tests keep their channel-based handlers where
the handler *is* the test. The journaling wrapper records at invocation
start (End zero while running — which is the "handler is mid-flight"
synchronization signal `WaitJournal` exposes) and completes the record on
return, re-raising panics so `safeHandle` containment stays the behavior
under test.

**Assertions.** `IsQuiescent`/`WaitQuiescent` check the ticket's trio:
stream drained (group `lag == 0` via `XINFO GROUPS` — `Stats.Length`
can't express drained because entries are never trimmed; compose Redis is
7.x so lag is present), PEL empty, delayed set empty. The quarantine list
is deliberately *not* part of quiescence (chaos tests plant malformed
members on purpose); it is asserted via `MalformedMembers`.
`RequireHandledOncePerClaim` pins the precise exactly-once claim: no two
invocations share (entry ID, delivery count). Re-execution across claims
is *correct* at-least-once behavior — the pre-ack crash proves it: the
handler legitimately runs twice, once per claim, and the claim CAS (M4)
is what turns that into effectively-once. `WaitQuiescent`/`WaitJournal`
failures call `DumpDiagnostics` (stats, PEL detail, delayed members with
scores, quarantine, full journal) — the diagnostic-state dump 5.8
requires.

**Self-tests.** Unit (no Redis): script sequencing/stickiness/default,
hang release + cancel, panic re-raise, journal recording, the
duplicate-claim detector, plus — in the queue package — pre-handle abort
with a nil client (proving the abort precedes any Redis command) and
`Phase.String`. Integration: `TestKillAtPreHandle` (W2: handler never
runs, lease survives, B reclaims at count 2), `TestKillAtPreAck` (W4,
the cell that motivated the hook: work done, ack suppressed, redelivered,
handled once per claim), `TestKillMidHandle` (hang + kill → ctx.Err nack
→ reclaim consumes the next scripted action), `TestQuiescenceInvariants`
(walks one entry through delayed → undelivered → unacked, asserting the
probe catches each), `TestScriptedLadderUnderReclaim` (fail → panic →
succeed across real reclaims).

**Refactor note.** `TestKillPreAckRedelivery` (3.3) deliberately keeps
its manual `XAUTOCLAIM` + exported-`Process` replay rather than using
`KillAt(PhasePreAck)`: it pins the export's contract that reclaimed
entries enter the shared per-message path; the harness self-test covers
the true W4 crash end to end.

Deferred (ninth time): the storetest template-database fast path.

### 3.7 — Post-M3 audit: stream retention & heartbeat ownership guard ✅

A full audit of M3 against the roadmap and ADR-005 (every acceptance
criterion re-verified, lint + unit + integration suites re-run green)
confirmed the milestone complete but surfaced two protocol gaps, fixed
here as an audit-addendum ticket.

**Gap (a): unbounded stream growth.** `XACK` removes an entry from the
PEL but not from the stream, and nothing anywhere trimmed — every
envelope ever enqueued stayed in Redis memory forever, and `Stats.Length`
was a counter that never decreased rather than a depth. Fix: `TrimAcked`
on `Queue` (`trim.go`) + a fourth consumer duty (`TrimInterval`, default
1m; `AGENTLOOM_QUEUE_TRIM_INTERVAL`, usual config keep-in-sync
duplication, `.env.example` documented). Threshold = the group's
smallest pending entry ID when the PEL is non-empty, else the successor
of last-delivered (`successorStreamID`, sequence-overflow handled);
everything below it is delivered-and-acked by construction. The
threshold is monotonically safe against concurrent consumers —
`XREADGROUP >` only moves forward and `XCLAIM`/`XAUTOCLAIM` only touch
existing PEL entries, so nothing below a snapshot can become pending
again; a stale snapshot merely trims less. **Exact** (non-`~`) `XTRIM
MINID`, deliberately: per-pass cost is bounded by entries acked since the
last pass, and determinism keeps tests and depth metrics honest. A
missing group/stream is typed `ErrNoGroup` (never a fabricated no-op).
One subtlety verified by test rather than assumed: group `lag` — the
3.6 quiescence probe's drained signal — stays computable after trims,
because deletions never exceed last-delivered (Redis only NULLs lag when
possibly-unread entries were deleted).

**Gap (b): zombie heartbeat stole reclaimed leases back.** The 3.4
heartbeat was `XCLAIM JUSTID` with min-idle 0, which unconditionally
transfers ownership: a consumer stalled past TTL whose entry a reclaimer
legitimately took over would, on resuming, silently steal the entry back
on its next beat — and the two heartbeaters would flap ownership. Safe
(whoever completes first acks; the loser is fenced by `claim_id`) but
invisible in logs and contrary to ADR-005's R1(b) "heartbeat starts
failing" narrative. Fix: the beat is now a small Lua script — `XPENDING`
the single entry, claim only if the owner is still the beater,
atomically (no check-then-claim race). Outcomes: still-owner → idle
reset, keep beating; displaced → warn with the new owner's name, stop
the heartbeater (execution continues; the fence decides); gone
(acked/deleted) → warn, stop. Transport errors keep the beater alive as
before. The heartbeater goroutine loop now exits early when a beat
reports the lease definitively lost.

**Also:** corrected `Stats.Length`'s doc comment (it counts undelivered +
in-flight + acked-not-yet-trimmed, not "ready depth"), and updated
`Run`'s duty documentation and startup log (`trim_interval` field).

**Tests.** Unit: `successorStreamID` table incl. sequence-overflow
rollover and malformed IDs (a first cut let a non-integer timestamp
through — caught by the test, fixed by always validating both parts);
config defaulting/env parsing for the new knob; `ConsumerConfig`
defaults. Integration: a hand-driven retention walk (pending entry pins
the threshold at every stage; undelivered entry survives an empty-PEL
trim; quiescence probe still detects a fresh undelivered entry *after*
trims — the lag property); `ErrNoGroup` without a group; consumer-duty
wiring (short `TrimInterval` physically empties the stream unattended);
and the displacement test — entry force-`XCLAIM`ed to a thief while the
holder's fast heartbeats run, owner asserted stable across ~10 beat
intervals (guarded beats refuse to steal back), then the displaced
handler completes and acks anyway (`XACK` needs no ownership).

**Storetest template-database fast path: decision recorded, deferral
retired.** Deferred nine consecutive tickets; the integration suite
currently runs in single-digit seconds, so the optimization has no
payoff yet. Decision: not doing it until integration-suite runtime
actually hurts (revisit when a full `make test-integration` pass is
slow enough to disrupt the loop, e.g. >1–2 minutes). Dropped from the
CLAUDE.md loose-ends list; this entry is the record.

## Milestone 4 — Distributed execution MVP

### 4.1 — Executor interface v0 & test executors ✅

**What shipped.** `internal/exec`: the executor SPI v0 — `Executor`
(`Type() string`, `Execute(ctx, StepContext) (Output, error)`), an
instance-based `Registry` (`NewRegistry`/`Register`/`Get`; misses are a
typed `*UnknownTypeError` unwrapping to `ErrUnknownType`), and the four
test executors: `noop` (empty output), `echo` (config `input`, falling
back to the rendered input), `sleep` (Go duration string, ctx-aware
wait), `fail_n_times` (fails attempts 1..n, succeeds after).
`Builtins()` returns the four pre-registered for 4.2's worker wiring.

**Catalog expansion (the one scope call).** `sleep` and `fail_n_times`
were added to the dag step-type catalog — constants, `SleepConfig` /
`FailNTimesConfig`, `stepConfigTypes` + `stepTypes` entries, validation
rules, kitchen-sink coverage (`flaky_probe` feeding the `any` join,
`cooldown` between approve and publish), regenerated JSON Schema.
ADR-003 pre-authorized exactly this ("further test executors register
the same way in M4 without touching this ADR"), and 4.7 submits
definitions made of `sleep` steps, so they had to be first-class types.
The catalog's sync tests (catalog↔registry, kitchen-sink coverage,
schema drift) policed every touchpoint as designed.

**Non-obvious decisions.**
- `StepContext.Config` is **raw JSON** (as materialized into
  `run_steps.config`, ADR-004), decoded inside each executor via the new
  exported `dag.DecodeStepConfig` (same strictness + normalization as
  `Decode`; nil raw → nil config) through a generic `configAs[T]` helper.
  The worker stays ignorant of config shapes — the seam where M8's
  per-plugin config schemas will slot in. Decode failures are a typed
  `*InvalidConfigError` unwrapping to `ErrInvalidConfig`; since configs
  were validated at submit time, hitting one at execution time means
  corrupt stored state or version skew — permanent failure, never a
  retry.
- `sleep.duration` is a Go duration string, checked **parseable and
  positive at validation time** (new code `config_field_invalid`) — same
  reasoning as compiling CEL at validation: an accepted definition must
  not explode mid-run on a malformed literal. The executor re-checks
  (defense in depth); its wait is injectable (`SleepExecutor.sleep`
  field), so unit tests never sleep for real, honoring the
  time-is-injectable invariant.
- `fail_n_times.n` requires `n >= 1`; `0` reads as absent (the
  `Edge.MaxIterations` convention) and would be `noop` anyway. The
  executor consults **only** `StepContext.Attempt` (1-based, from the
  attempt row) — no in-process state — so its failure schedule holds
  across crashes and reclaims, which is what M5's retry fixtures need.
- `Output` is a struct wrapping `Data json.RawMessage`, not an alias, so
  M8 can add usage/artifact fields without breaking implementors.
- `noop` never decodes its config at all (even unknown fields pass) —
  ADR-003 says it takes none, and rejecting garbage there would be
  validation's job, not the executor's.

**Tests.** All pure unit (no process/datastore boundary crossed):
registry lookup/duplicate/empty/nil registration, `Builtins` coverage,
per-executor behavior incl. sleep cancellation surfacing `ctx.Err()` and
`fail_n_times` statelessness across executor values; dag side:
`DecodeStepConfig` (typed decode, normalization, nil, unknown
type/field, non-object), validation table rows for both new types.

**Deferred.** `fail_n_times`'s deliberate failure is a plain error —
M5's retry-class taxonomy will decide how executor errors map to
retryable/permanent. Executor-level structured logging is minimal
(`fail_n_times` logs its deliberate failures); the hot-path logging duty
lands with the worker pipeline (4.2/4.3) where run/step/attempt fields
get stamped.

### 4.2 — Worker skeleton: consume → claim ✅

**What shipped.** The first deployable worker. `internal/engine` (the
ADR-002 execution pipeline's home): `Engine` (store + executor registry +
injectable clock + `worker_id`) whose `Handle` is a `queue.Handler`
implementing the claim path — one `WithTx` containing exactly
`store.ClaimStep`, then ADR-005's ACK discipline applied to the outcome
via a pure, unit-tested classifier (`classifyClaimFailure`). On a won
claim the executor runs with a `StepContext` built from the claimed
`run_steps` row (raw config, nil input — rendering is M6 — and the
durable attempt number); the lease heartbeater needs no new wiring
because 3.4's consumer already beats around every handler invocation.
`cmd/worker`: env-only config, JSON logs, store/Redis wiring on the
ADR-005 default stream+group, `exec.Builtins()`, one consumer whose name
doubles as `worker_id`, SIGINT/SIGTERM graceful drain, and health
logging (startup/stop lines plus a periodic liveness line with stream
depth and PEL size, `AGENTLOOM_WORKER_HEALTH_INTERVAL`, default 1m —
new `config.WorkerConfig`).

**The ACK decision table as implemented.** Claim won → execute → ACK.
Wrong-status from a terminal state → ACK-and-drop (duplicate of finished
work, crash cell W4). Wrong-status `running` on a *fresh* delivery
(delivery count 1) → ACK-and-drop (concurrent duplicate; the holder's own
entry covers the crash case). Wrong-status `running` on a *reclaimed*
delivery → **no ACK**: lease-expiry takeover (`running → ready` clearing
`claim_id`) is ticket 4.5's store transition, so until it lands the entry
stays pending and rises visibly toward the poison path — never silently
dropped. `ErrNotFound` → ACK-and-drop (dangling reference). Anything else
(unexpected status/reason, transport failure) → no ACK, redeliver.

**Non-obvious decisions.**
- **ACK-after-claim-decided is a deliberate v0 policy.** With no
  completion transaction yet, a claimed step stays `running` and the
  executor's result (success *or* failure) is only logged — redelivery
  could only bounce off the running-status CAS, so retrying buys
  nothing. 4.3 replaces this tail wholesale and moves the ACK after the
  completion tx commits. Executor-registry misses and config-decode
  failures are likewise log-and-drop (version skew/corrupt state —
  permanent, per 4.1's reasoning).
- The engine is transport-agnostic: it sees `queue.Delivery`, never
  Redis. The fresh-vs-reclaim distinction rides `DeliveryCount` (1 =
  fresh by XREADGROUP `>` semantics), which ADR-005 already fixed as the
  duplicate-vs-crash discriminator.
- The `config.QueueConfig → queue.ConsumerConfig` mapping lives in
  `cmd/worker` (config cannot import queue — documented cycle);
  `PoisonHandler` stays nil on purpose (3.4's visible-spin default until
  M5.4 wires dead-lettering). One consumer per process; parallelism is
  worker replicas (the M19 lever is sharding, not in-process fan-out).

**Tests.** Unit: the classifier's full decision table (including the
fresh/reclaimed running split), `New` validation, config mapping
(pinning that `PoisonHandler`/`PhaseHook` stay nil), `WorkerConfig`
loading. Integration (first suite composing both harnesses —
`storetest` Postgres + `queuetest` Redis): 8 duplicate envelopes of one
ready step consumed by two engine-backed consumers (`Batch: 1` to spread
entries) → exactly one attempt row, one `step_claimed` event, one
executor invocation (counting noop-typed executor — the registry being
instance-based made the probe trivial), everything acked to quiescence;
duplicate of an out-of-band-completed step → dropped with zero side
effects (output compared semantically — Postgres jsonb does not preserve
whitespace); dangling run id → dropped, loop healthy; `cmd/worker`
full lifecycle in-process against the compose stack (start, health line,
cancel → clean nil return, stop logs).

**Deferred.** Reclaimed-`running` deliveries spin (visibly) until 4.5's
takeover transition; the claimed-but-stuck-`running` steps this ticket
can strand are exactly what 4.4's reconciler and 4.3's completion tx
exist to close. The start/stop test uses the global `steps:ready` stream
(names are not yet configurable) — fine against a dev stack, worth a
knob if it ever bites CI.

### 4.3 — Execute & complete pipeline (readiness fan-out) ✅

**What shipped.** The completion tail (`internal/engine/complete.go`):
after the executor returns, one transaction settles everything —
claim-fenced `running → succeeded` with the output persisted
(`store.SucceedStep`), out-edge resolution, skip propagation, readiness
fan-out with outbox rows for newly-ready steps, and the run rollup
attempt — with the ACK moved after commit, closing ADR-005's
"completion/failure transition committed → ACK" row. Edge verdicts are
computed by `planEdges`, a pure pre-transaction function (unit-tested
without a database): non-branch sources fire every edge whose `when` is
absent or true (all-matching); branch sources apply ADR-003's
first-match-in-ordinal-order rule with the trailing `when`-less default,
and deliberately do not evaluate predicates past the match (their errors
could not matter). Loop edges are excluded everywhere. Inside the
transaction, `fanOut` chases a worklist to the fixed point: each
`ResolveEdge` returns the target's updated counters; `fired_deps ≥ 1 ∧
(remaining_deps = 0 ∨ join-any)` → `ReadyStep` + outbox row;
`remaining_deps = 0 ∧ fired_deps = 0` → `SkipStep` and the skipped
step's own out-edges join the worklist all-skipped (propagation). The
2.6 transition primitives compose exactly as designed — no store
mutation surface changed. New store read: `ListRunEdgesFromStep`
(ordinal-ordered, sqlc + `StepRepo`).

**Failure completion (the scope call beyond the ticket's headline).**
Executor errors — and registry misses / config-decode failures, which
are deterministic version-skew/corrupt-state cases — now land a real
failure transaction (`FailStep` with `{"message": …}` recorded, then
`FailRun`, conflicts dropped) instead of 4.2's log-and-strand. ADR-005's
ACK table already prescribed this for 4.3, and without it a failed
executor left the step `running` forever. `FailRun` after any step
failure is the v1-minimum rollup 2.6 built; *when* a failure halts a run
is still ADR-006's (M5) — the failed step's out-edges stay unresolved,
so dependents block, never skip.

**Non-obvious decisions.**
- **CEL evaluation errors fail the step** (ADR-003: "recorded as a
  step-level failure of the completing step's transition; never coerced
  to false"): `planEdges` errors — including predicates over a nil
  output — reroute the success path into `completeFailure`. Same for
  run-params that no longer decode: deterministic content failures must
  not become redelivery loops.
- **Join-any absorption is status-based, not conflict-based:** the
  worklist checks the counter-updated target row it already holds (under
  the run lock) and simply skips non-`pending` targets, so a late firing
  resolves its edge but never re-dispatches. Exactly-once readiness is
  asserted via `step_ready` event counts.
- **"Nudge the dispatcher" became a seam:** `engine.WithDispatchNudge`
  (no-op default) fires post-commit when outbox rows were written; 4.4
  wires its drain loop's wake channel here. The integration suite stands
  in with a test-local drainer (List → Enqueue → Delete-after-enqueue —
  at-least-once by construction; duplicates ACK-drop at claim).
- **Single-transaction proof** reuses 2.5's failpoint pattern: a
  package-global hook (`SetCompleteFailpoint`, export_test) aborts the
  transaction at `after_step_transition` / `after_fan_out` /
  `after_outbox`; the test drives `Handle` directly (no queue) and
  asserts byte-exact pre-completion state — claim intact, no output, no
  events, edges unresolved, counters untouched, outbox unchanged.
- **`join` and `branch` got trivial builtin executors** (pass-through of
  config input / rendered input, mirroring echo's convention and
  `BranchConfig`'s documented contract). Both are control-flow types
  whose semantics live in the engine (counters, edge firing), so their
  executors are legitimately trivial; join fixtures cannot run
  end-to-end without one. `Builtins()` now registers six.

**Tests.** Unit: `planEdges` (all-matching, first-match, default edge,
no-match-all-skip, post-match predicates unevaluated, loop exclusion,
typed `*dag.EvalError` on missing fields / nil output), `isJoinAny`
(mode table + corrupt-config error), `allSkippedVerdicts`, the two new
executors. Integration (2 workers, Batch 1, test drainer): linear,
fan-out/fan-in (join `all` readied exactly once, after every parent —
asserted by event seq), conditional branch with skip propagation
(statuses, skip counters, zero attempts/dispatches for skipped arms, all
8 edge resolutions pinned), join `any` late-firing absorption (one
`step_ready`, both edges still resolve fired), executor failure (step +
run failed, error on step and attempt, dependent never dispatched,
queue quiescent), and the three-stage failure-injection
single-transaction test. 4.2's claim-race test updated: the winner's
step (and single-step run) now ends `succeeded`.

**Deferred.** A completion transaction that fails transiently leaves the
step `running` with the entry un-ACKed — the redelivery then hits the
reclaimed-`running` no-ACK path until 4.5's takeover lands (4.2's known
spin, unchanged). Failure policy, retries, and DLQ are M5;
`docs/architecture.md`'s realized execution walkthrough is deferred to
the milestone exit (4.7).

### 4.4 — Outbox dispatcher & reconciler ✅

**What shipped.** The two dispatch duties every worker runs (ADR-002 —
no central scheduler). `internal/engine/dispatch.go`: `Dispatcher`, the
outbox drain loop — one transaction per pass doing exactly ADR-005's
producer sequence: `Outbox().ListForDrain` (new repo method, `SELECT …
ORDER BY id LIMIT n FOR UPDATE SKIP LOCKED`, guarded by `ErrNoTx` outside
`WithTx`) → `XADD` per row via the `Enqueuer` seam → `Delete` of the rows
that reached the stream → commit. `Run` waits on a ticker (the latency
backstop) or the cap-1 coalescing `Nudge` channel, now wired into 4.3's
`WithDispatchNudge` seam, and drains to empty on every wake; errors are
logged and retried next wake, never fatal. `internal/engine/reconcile.go`:
`Reconciler`, a jittered (±20%) periodic sweep in one transaction under a
fleet-wide `pg_try_advisory_xact_lock` — losers skip (`Skipped`), so N
workers cost one sweep per interval. The sweep re-outboxes steps stuck in
`ready` past `ReadyStale` that have no pending outbox row (new reason
`reconcile_ready`, healing ADR-005 P2/R1(a)), then flags — warn/error
logs plus a `ReconcileResult` — stale-`running` steps past `RunningStale`
(R1(c) dead-worker suspects) and runs `running` with no live step (an
impossible state). A sweep that wrote rows fires the dispatcher's nudge.
Store side: `internal/store/reconcile.go` (`TryReconcileLock`,
`ListStaleReadySteps` with the anti-join, `ListStaleRunningSteps`,
`ListStalledRuns` — all requiring the WithTx Querier, like transitions)
over a new `queries/reconcile.sql`. `cmd/worker` starts both loops
(waited on at shutdown) with new `WorkerConfig` knobs:
`AGENTLOOM_WORKER_DISPATCH_INTERVAL`/`_BATCH` (1s/64) and
`AGENTLOOM_WORKER_RECONCILE_INTERVAL`/`_READY_STALE`/`_RUNNING_STALE`/
`_LIMIT` (30s/1m/5m/256).

**Non-obvious decisions.**
- **Partial-batch commit on enqueue failure.** An `Enqueue` error
  mid-batch stops the pass but still commits the deletes of rows whose
  XADD landed — rolling back would re-dispatch them all. The failed row
  itself is kept: whether its XADD landed is unknown (the P1 window), so
  it re-drains next pass and any duplicate ACK-drops at claim. A full
  rollback (crash) leaves every XADDed row in place — same P1 shape.
- **The reconciler heals via outbox rows, not direct XADD** — one
  dispatch path; the drainer stays the only component XADDing for
  dispatch. Re-outboxing appends no event and touches no step row:
  dispatch bookkeeping, not a transition (`ReadyStep`'s documented
  contract).
- **Idempotency is the anti-join, rate-bounding is layered.** A stuck
  step with a pending outbox row never re-qualifies, so repeated sweeps
  are no-ops until a drain happens; after that it costs at most one
  duplicate per threshold period. Advisory lock (one sweep fleet-wide) +
  jitter + per-scan `Limit` (hits are logged — no silent cap) complete
  the no-thundering-herd story.
- **Stale-`running` steps are flagged, not healed.** The heal needs
  4.5's `running → ready` takeover CAS; until then a re-outbox would be
  ACK-dropped as a fresh-delivery duplicate by 4.2's classifier. ADR-005's
  R1(c) cell and tuning table amended accordingly (reason vocabulary too:
  `reconcile_ready` is now v1); ROADMAP 4.5 records the upgrade.
- **The 4.3 integration suite now runs the production dispatcher** —
  `drainOutbox` (test-local List → Enqueue → Delete) replaced by
  `startDispatcher` + nudge-wired workers, so every existing fixture
  exercises the real drain path with contracts unchanged.

**Tests.** Unit: constructor validation for both types, `Nudge`
never-blocks under concurrency, jitter bounds, new `WorkerConfig`
fields (defaults/overrides/invalids). Store integration: `ListForDrain`
and all reconcile reads reject non-tx Queriers; anti-join semantics
(pending row → invisible; drained → visible past threshold only);
stale-running rows carry the holder's claim ID; stalled-run flag on
fabricated corruption (raw UPDATE); advisory-lock mutual exclusion and
release-on-commit. Engine integration: 4 concurrent drainers over 12
independent steps → exactly one stream entry per step, outbox empty
(SKIP LOCKED partitioning); P1 injected post-XADD failure → row kept,
re-drain produces the duplicate pair, two workers → exactly one
execution, duplicate ACK-dropped, run succeeds; the headline heal —
lost XADD after commit (`lostEnqueuer`) → step stuck ready with nothing
anywhere → sweep before threshold does nothing, past threshold
re-outboxes exactly once with `reconcile_ready` (second sweep no-op) →
real dispatcher + workers complete the run with one `step_ready` per
step and the healed reason visible on the wire; sweep under a held lock
→ `Skipped` with no work, heals after release; flags mutate nothing
(step keeps status + claim, corrupt run keeps its status, no rows
written).

**Deferred.** The stale-`running` takeover heal (4.5, recorded in
ROADMAP); reconciler/dispatcher metrics — sweep counts, drain latency
from `enqueued_at_ms` — are M7; delayed-set reconciliation has no v1
tenant (M5.2).

### 4.5 — Fencing enforcement (zombie writes) ✅

**What shipped.** The last piece of the effectively-once story: what
happens when a worker that lost its lease keeps writing. Store side, the
`running → ready` **takeover CAS** (`store.TakeoverStep`, ADR-004's M4
matrix row, now owned by 4.5): fenced on the *observed* holder's
`claim_id` — not just status — it clears the claim (the moment the zombie
loses its fence), closes the holder's dangling attempt row with the new
administrative outcome `lost`, and appends the new `step_reclaimed` event
(payload: step, displaced claim, stranded attempt number). Engine side,
three behaviors: (1) the **claim-path takeover** — a reclaimed delivery
(`DeliveryCount > 1`) of a `running` step now runs one transaction
composing `TakeoverStep` + `ClaimStep` and proceeds to execute, replacing
4.2's deliberate no-ACK spin (the classifier grew a three-way action:
ack-drop / redeliver / takeover); (2) **fenced completion abandon** — a
completion/failure transaction whose terminal CAS is rejected with a
typed conflict logs one distinct error carrying both claim IDs
(`claim_id_caller` / `claim_id_current`) and returns
`errFencedCompletion` so the consumer ACKs nothing (for a reclaimed entry
an ACK would delete the *new* holder's lease); (3) the **reconciler
heal** — stale-`running` steps (R1(c)) go from flag to takeover +
re-outbox with new reason `reconcile_running`, per-step conflicts dropped
(the step moved on — the fence did its job) without aborting the sweep,
`ReconcileResult.StaleRunning` renamed to `TakenOver`, nudge extended.

**Non-obvious decisions.**
- **The takeover is claim-guarded, not just status-guarded.** Both
  callers observe the holder's claim before taking over (the worker from
  the claim conflict's `CurrentClaimID`, the reconciler from its scan);
  guarding the CAS on it closes the ABA window where the step was taken
  over *and re-claimed by a live worker* between observation and CAS —
  without it a stale takeover could steal a live claim. To feed this,
  `stepConflict` now always reports `CurrentClaimID` (the row's claim at
  rejection time) and reports `CallerClaimID` for any claim-guarded
  rejection — a fenced zombie most often sees `wrong_status` (the new
  holder already completed), not `claim_mismatch`, and the ticket's
  both-IDs logging requirement covers that case too.
- **Worker takeover + re-claim is one transaction.** ADR-005 shows two
  CASes without mandating boundaries; one tx removes the intermediate
  ready-with-no-entry state, and the run-lock-first discipline makes the
  pair race-free. If the reconciler's takeover won in between
  (`wrong_status` from `ready`), the conflict is swallowed — a rejected
  transition writes nothing — and the claim proceeds on the entry in
  hand. A takeover finding the step terminal ACK-drops (ADR-005's race
  rule: the holder's completion committed between reclaim and takeover).
- **The dangling attempt closes as `lost`.** Leaving it open would be
  indistinguishable from in-flight; `succeeded`/`failed` would lie.
  `lost` is administrative, deliberately outside ADR-006's future
  outcome taxonomy (recorded in ADR-004). It is also what makes 4.7's
  "attempt history proves the reclaim" legible: attempt 1 = dead claim /
  `lost`, attempt 2 = fresh claim / `succeeded`.
- **Abandon is uniform: any terminal-CAS conflict → no ACK.** Also on
  `wrong_status` from `ready` (a false-positive reconciler takeover of a
  live worker): the zombie's own entry goes stale after its heartbeater
  stops, redelivers, and bounces off the claim CAS — self-healing, no
  special case.
- **No outbox schema change for "keyed idempotently per transition":**
  successor outbox rows exist only inside a committed completion tx, and
  the fence admits exactly one completion per step — dispatch-exactly-once
  is asserted on `step_ready` event counts.
- **Reconciler sweeps now take multiple run locks in one tx** (takeover
  locks the run first, like every transition). No deadlock: claim and
  completion transactions hold locks on a single run (no cycle possible),
  and the advisory lock serializes sweeps against each other. Documented
  in the package comment.

**Tests.** Unit: both classifiers' full decision tables (claim: takeover
carries the holder claim, nil-claim running is corrupt-state redeliver;
takeover: newer-holder mismatch and completed-in-between both ack-drop).
Store integration: `TakeoverStep` joined the transition matrix (legal
only from `running`); a dedicated suite covering takeover → re-claim →
complete (attempt history `lost`→`succeeded`, `started_at` preserved,
`step_reclaimed` in the event log), the stale-claim fence (both IDs on
the error), and the completed-before-takeover race. Engine integration:
the ticket's headline — worker A claims and stalls (its `Handle` driven
directly over a PEL entry it never heartbeats: a perfectly silent
holder), B reclaims/takes over/completes the run, A resumes and is
rejected — state reflects B's output only, successors dispatched exactly
once (event-log asserted), the rejection error names both claim IDs, no
ACK from A (error return), queue quiescent; the claim-path takeover after
a dead worker (delivery count 2 → takeover → run completes, two claims
journal-asserted); the reconciler heal end-to-end (nothing before the
threshold, exactly one takeover + `reconcile_running` row past it, second
sweep a no-op, healed run completes with the reason visible on the wire);
the impossible-state flag test slimmed to stalled runs only.

**Deferred.** ADR-005's known duplicate-reclaim takeover race (duplicate
entry's reader crashes pre-drop, later reclaimed while the real holder
lives) remains accepted — bounded to one wasted execution, absorbed by
fencing now and side-effect idempotency in M5.5. Graceful-drain semantics
for an executor interrupted by shutdown (today it records a real failure)
are M5's retry/timeout work.

### 4.6 — Minimal ingest API & `ctl` CLI ✅

**What shipped.** The first client-facing surface, welding 2.5's atomic
instantiation to the M4 execution engine. `internal/api` (chi router, dev
mode — no auth until M6): `POST /v1/runs` accepts an inline definition
XOR a stored `definition_id` ref plus opaque `params` and an
`idempotency_token` (201 on create, 200 + `reused` on token replay, per
CreateRun's semantics); `GET /v1/runs/{id}` returns the run rollup, every
step with its full attempt history, and every edge with its resolution —
attempts come from the new run-wide `AttemptRepo.ListByRun`
(`ListRunStepAttempts`, one query instead of per-step); `GET /healthz`
pings Postgres (new `Store.Ping`). Definition problems answer 400 with
the error envelope `{"error": {code, message, issues}}` where `issues`
carries M1's path-qualified findings verbatim — `DefinitionIssues`
flattens the dag package's joined `*DecodeError`/`*ValidationIssue` trees
into wire shape (exported: ctl's local validate renders the same shape).
The API holds **no Redis client** (ADR-002): submission writes outbox
rows; the worker fleet's dispatchers drain them. `cmd/api`: env-only
config (new `config.APIConfig` — `AGENTLOOM_API_ADDR`/`_READ_TIMEOUT`/
`_WRITE_TIMEOUT`/`_IDLE_TIMEOUT`/`_SHUTDOWN_TIMEOUT`), graceful
SIGINT/SIGTERM drain, and a `ready` channel so the in-process test can
learn a `:0` listener's port. `cmd/demo` deleted as its header promised.
Cobra `cmd/ctl` (pure HTTP client; base URL via `--api` or
`AGENTLOOM_API_URL`): `validate <file>` (local decode + M1 validation,
all findings printed path-qualified, exit 1 on error severity), `submit
<file> [--params json] [--token t]` (stdout carries exactly the run id so
`ctl watch "$(ctl submit …)"` composes; context and rendered 400 issues
go to stderr), `watch <run-id> [--interval] [--timeout]` (poll
`GET /v1/runs/{id}`, re-render the status tree on change — Kahn
topological order over normal edges, depth-indented, glyph + attempt
counts + failure messages — exit 0 on `succeeded`, 1 on `failed`).
Deploy: `deploy/dockerfiles/Dockerfile`, one multi-stage build with
`api`/`worker`/`migrate` targets (ldflags-stamped version, non-root
alpine runtime; hardening deferred to M20); compose gains the three
services under the **`app` profile** — one-shot `migrate` job gating
`api` (healthchecked on `/healthz`, published on `AGENTLOOM_API_PORT`)
and `worker` (`deploy.replicas: 2`) — via `make up-app`, keeping
`make up`, `make test-integration`, and the CI integration job
stores-only.

**Non-obvious decisions.**
- **`llm`/`tool`/`retrieve` became dev-stub executors** in `Builtins()`
  (`internal/exec/devstub.go`): the ticket's acceptance runs
  `examples/definitions/fanout.json`, which carries all three types, but
  their real executors arrive with M8's plugin SPI and M9's provider
  layer — and since 4.3 a registry miss is a real `FailStep`/`FailRun`.
  Each stub succeeds immediately with a deterministic, config-derived
  output carrying a `"stub": true` marker; no network, keys, or state.
  Same types, real semantics replace them in place later; ROADMAP 4.6
  records the scope addition.
- **The compose `app` profile is a deliberate deviation** from the
  ticket's literal "docker compose up": default services would force Go
  image builds into every `make up` and the CI integration job (which
  shares the compose file precisely so envs match). The acceptance box
  records the amended command.
- **ctl prints the run id alone on stdout.** Everything human goes to
  stderr, so the ticket's `ctl submit … && ctl watch …` loop is
  scriptable as `ctl watch "$(ctl submit …)"` without parsing.
- **A stored-ref decode failure is a 500, not a 400** — a spec that was
  valid when stored but no longer decodes is server-side corruption or
  version skew, not a client error. Inline submissions re-validate from
  scratch and 400 with the full issue list (warnings included).
- **GET is three pool reads, deliberately not one transaction**: a run
  mid-flight may show a step slightly newer than its rollup counters;
  the watch loop's next poll heals it. Documented on the handler.

**Tests.** Unit: the three stubs (deterministic outputs, messages-take-
last, required-field misses → `ErrInvalidConfig`), `APIConfig`
defaults/overrides/invalids, `DefinitionIssues` flattening (decode +
validate paths, path preservation), ctl against `httptest` fakes
(validate over the canonical fixtures + invalid/malformed files, submit
stdout contract + params validation + rendered 400 issues, watch until
succeeded / failed-exit / env-var base URL, topological render order).
Integration (`internal/api`, storetest + httptest): healthz; submit +
GET round-trip (fanout counters, entry `ready`, edges unresolved);
path-qualified 400 (`edges[0].to` / `unknown_edge_endpoint` asserted on
the wire); malformed-request table (bad JSON, unknown field, XOR
violations, bad/unknown `definition_id`); idempotency token replay
(201 → 200 `reused`, same run); stored-ref submission recording
`definition_id` provenance; GET misses (404 unknown, 400 bad UUID). The
headline (storetest + queuetest + production dispatcher + two
builtin-registry workers): fanout.json in through POST, executed to
`succeeded`, every step 1 attempt with outcome on the wire, all edges
fired, stub-marked outputs on the AI-native steps, outbox drained, queue
quiescent, no duplicate handling per claim. Acceptance verified live on
compose: `make up-app` → `ctl submit examples/definitions/fanout.json`
→ `ctl watch` reached `succeeded` across the 2 worker replicas.

**Deferred.** OpenAPI contract (6.6), auth (6.1–6.2), param-value
validation against ParamSpecs (M6), run listing/cancel endpoints (6.5).
`ctl` links the store package transitively (shared wire types live in
`internal/api`); splitting a leaf types package can ride M6.6's contract
work if binary size ever matters.

### 4.7 — Flagship crash-recovery integration test & demo ✅

**What shipped.** The headline guarantee, proven against real process
death — closing crash-matrix cell W3's last "proven by" and M4 itself.
`test/crash`, the first suite in the planned top-level chaos home: its
`TestMain` builds `cmd/worker` once, and every scenario spawns **real
worker subprocesses** and kills them with SIGKILL — the one crash shape
the in-process suites cannot produce (a goroutine cannot be killed;
cancelling a context makes the sleep executor return an error, which is
the *failure* path, not a crash — heartbeats must simply stop).
Scenario 1 (`TestWorkerSIGKILLMidStepRecovered`): two workers run
counter → sleep(4s) → counter; the test polls until the sleep step is
`running`, reads the lease holder from the PEL (`XPENDING` + envelope
decode; consumer name == worker_id, parsed from each subprocess's
"worker started" JSON log line), SIGKILLs that process, and asserts the
survivor reclaims → takes over → re-executes → completes: attempt
history exactly `lost` → `succeeded` on distinct claims, one
`step_reclaimed` event, one `step_ready`/`step_succeeded` per step, each
counter file exactly one line, queue quiescent, outbox empty.
Scenario 2 (`TestFullFleetRestartResumesFromLastCompletedStep`):
SIGKILL the *entire* fleet mid-run, prove the run freezes (the orphaned
entry's PEL idle provably exceeds the TTL while the step stays `running`
with its attempt open — expiry alone moves nothing), then start fresh
workers: the run resumes from the last completed step, pre-crash steps
keeping their single attempt and single counter line. Packaging:
`make demo-crash` runs `scripts/demo-crash.sh` against compose — Act 1
kills the lease-holding worker container (holder found via
`redis-cli XPENDING` + container-log matching), narrates the reclaim
countdown, watches to completion, and prints the per-step attempt table;
Act 2 SIGKILLs api + every worker mid-run and boots the stack back up to
show restart resume. The script asserts its own claims (exits non-zero
unless both runs succeed with the exact expected attempt shapes) and is
documented in `docs/demos/crash-recovery.md` with a mechanism-to-ADR map.
Verified live on compose.

**Production enablers (deliberately small).**
- **Queue names became config** (`AGENTLOOM_QUEUE_STREAM`/`_GROUP`/
  `_DELAYED_KEY` on `config.QueueConfig`, empty = ADR-005 defaults;
  tuning-table row added): 4.2's recorded "worth a knob if it ever bites
  CI" bit — subprocess workers on a shared Redis need per-test keys. The
  crash suite passes queuetest's isolated names straight through, so it
  composes with every other parallel integration test.
- **`counter` became the fifth test step type** (dag catalog + schema
  regen + kitchen-sink coverage; `exec.CounterExecutor` appends one
  `attempt=N` line to the file at `config.path`, `O_APPEND` for
  cross-process integrity). The ticket's "echo-side-effect counter"
  cannot be an in-process wrapper when workers are subprocesses; a real
  externally observable effect was needed. `StepContext` carries no step
  identity (4.1's minimal SPI, unchanged), so distinctness comes from
  per-step paths. This is also M5's exit-criterion probe ("side-effect
  counter proves no double-fire") arriving one milestone early.
- `storetest.NewDBWithDSN` (NewDB that also returns the DSN, for
  subprocess env); compose passes `AGENTLOOM_QUEUE_LEASE_TTL` through to
  workers (default preserved) so the demo can shorten the lease without
  editing the file.

**Non-obvious decisions.**
- **The reconciler is pushed out of the picture** (10m interval in the
  suite's worker env) so the reclaim path is the *single* recovery
  mechanism under test — the reconciler heal has its own 4.5 suite, and
  a test where either mechanism could win proves neither.
- **Synchronization is poll-until-state, never sleep**: on Postgres
  (step/run status, attempt counts) and Redis (PEL holder, PEL idle
  exceeding the TTL — the freeze proof). The only wall-clock waits are
  the workload itself (the sleep executor) and ADR-005's convention of
  shrinking the lease TTL (1s) rather than faking the Redis idle clock.
- **Kill timing is made safe by construction**: the killed step sleeps
  4s and the kill lands within ~100ms of observing `running`, so it is
  always mid-execution; the sleep step is the crash target precisely
  because re-executing it is harmless, while the counters bracketing it
  prove completed work is never re-run.
- **Demo kills are SIGKILL too** — `docker compose restart` would
  SIGTERM, the consumer would drain the in-flight handler, and the
  interrupted executor would record a real failure (4.5's recorded
  graceful-drain gap, owned by M5). Crash recovery is about the worker
  that never said goodbye; the doc says so explicitly.
- **Attempt history + PEL, not new schema, prove "A claimed, B
  completed"**: the holder was killed while provably owning the entry,
  and only one worker survived to make attempt 2 — no `worker_id`
  column needed (4.5's `lost`-outcome design carried the legibility).

**Tests.** The suite is the deliverable (above); the enablers got unit
coverage in their homes: config knob defaults/overrides + worker
config-mapping pin (`DelayedKey`), counter executor (append/accumulate
semantics, attempt stamping, config-vs-I/O error classification), dag
validation of `counter.path`, kitchen-sink coverage test extended by
registration. Full `make test-integration` green; both crash scenarios
run in parallel (~7s each) against the shared stack.

**Deferred.** Graceful-drain semantics for interrupted executors (M5, as
recorded at 4.5); the demo assumes an otherwise idle stack (holder
lookup reads the fleet PEL) — fine for a demo, and the CI suite is fully
isolated; api-process participation in the CI restart scenario (the api
is stateless and its restart is exercised by the demo's Act 2 — the CI
scenario proves the part that can lose state, the fleet).

### Post-M4 audit & hardening pass (2026-08-10)

A full audit of M4 against its acceptance criteria confirmed every
ticket's deliverables real and tested (including the milestone-exit
architecture walkthrough, which 4.3 deferred to 4.7 and 4.7 delivered in
commit ef88fa1 without recording it — noted here to close the ledger).
The audit surfaced one real bug, one reproducible CI flake, two behavior
gaps that contradicted recorded design notes, and assorted drift — all
fixed in this pass.

**Bugs fixed.**
- **`cmd/worker` deadlocked instead of exiting on consumer bootstrap
  failure**: `defer wg.Wait()` ran before anything canceled the context
  the dispatcher/reconciler loops wait on, so an `EnsureGroup` error
  (wrong-type stream key, Redis ACL) hung the process holding its pools.
  Fixed by deferring a `cancel` *after* `wg.Wait` (LIFO runs it first);
  the health loop joined the WaitGroup while there (it was the one
  unjoined goroutine). Regression test plants a wrong-type stream key
  and asserts `run` returns the error promptly.
- **`TestConsumerTrimsAckedEntries` flaked under parallel-package CI
  load** (~1 in 20 under contention, reproduced locally): the test
  spawns a consumer without `EnsureGroup`, the group is created
  asynchronously inside `Consumer.Run`, and the harness's `Stats` treated
  the `ErrNoGroup` race as fatal. Fixed in the harness — `WaitStats` now
  treats `ErrNoGroup` as not-yet-ready and keeps polling, curing every
  future spawn-then-wait test at once.
- **`ctl watch --interval 0` panicked** on `time.NewTicker`; now rejected
  up front with a proper error (unit-tested).

**Behavior gaps closed.**
- **Deterministic in-tx content failures no longer redeliver forever.**
  `isJoinAny` hitting a corrupt join-target config aborted the completion
  transaction into a redelivery loop — which, post-4.5, *re-executes* the
  step via takeover once per delivery until the poison threshold. A
  `*dag.DecodeError` surfacing from the transaction now reroutes to
  `completeFailure` (real `FailStep`/`FailRun`, ACK), matching the 4.3
  design note. `ResolveEdge`'s graph-integrity errors deliberately stay
  on the loud redeliver-to-poison path: bookkeeping corruption is not
  step content, and a `FailStep` over the same corrupt rows is no safer.
  Integration test corrupts a join config raw and asserts one attempt,
  step+run failed, queue quiescent.
- **The stale-`running` takeover no longer doubles a pending dispatch.**
  `ListStaleRunningSteps` grew a `has_pending_outbox` probe (the ready
  scan's anti-join, adapted); the healer still takes the step over but
  skips the `reconcile_running` re-outbox when an undrained row already
  carries the dispatch (the P1 shape sustained past the threshold).
  Tested: takeover with a pending row leaves exactly the original row.

**Hardening.**
- Panic containment: chi-level `recoverPanic` middleware in the API (500
  with the JSON envelope, `http.ErrAbortHandler` passed through) and
  `recoverPass` around dispatcher/reconciler passes (a pass panic becomes
  a logged retry, honoring the "must never kill the worker" contract the
  comments already claimed). `drainAll` also checks ctx between passes.
- Crash-suite determinism margin: `leaseTTL` 1s → 2s. The TTL is the
  stall budget a loaded CI runner gets before a spurious reclaim breaks
  the exactly-one-reclaim assertions; 2s doubles it for ~1s of test time.
- `scripts/demo-crash.sh` honors `AGENTLOOM_QUEUE_STREAM`/`_GROUP` (it
  sources `.env`, so an override there used to silently break the
  XPENDING holder lookup).

**Documented semantics (ADR-005 amended, config docs).** Three post-4.5
realities now recorded: `RunningStale` is a de-facto hard cap on step
wall-clock time (a live executor past it is taken over and its side
effects double — size it above the longest step until M5.3 timeouts); a
transient completion-tx failure now costs a re-execution (bounded by the
poison threshold), not just a redelivery; and the accepted
duplicate-reclaim race's likeliest trigger is now the reconciler's own
`reconcile_running` duplicate (same bound, named explicitly).

**Coverage added.** Exec-registry ↔ dag-catalog sync test with an
explicit deferred set (`map`/`planner`/`agent`/`human_approval`, ticket
refs) — adding a step type without deciding its executor story now fails;
`dag.StepTypes()` exported and `Registry.Types()` added to support it.
`cmd/api` got its missing lifecycle test (the `ready` channel 4.6's
entry described was dead code — now consumed: boot on `:0`, live healthz,
graceful drain). Nudge contracts pinned both ways (fires once on
readied/healed, never on no-ops). DrainOnce's rollback branch tested via
a context-canceling enqueuer (entry orphaned in stream + row kept → P1
duplicate pair absorbed at claim). Decode-corpus kitchen sink refreshed
with the three M4 step types and pinned to the catalog so it cannot go
stale again; schema test's `$defs` list extended likewise.

**Drift swept.** `queries/reconcile.sql` header ("never transitions
state" / "flag-only in 4.4" — false since 4.5, regenerated into gen/);
`exec.go` package doc (four → five test executors); the 0002 migration's
outcome-vocabulary comment (now names `lost`); README gained the
`make demo-crash` pointer; ROADMAP 4.1 notes the fifth executor, and
record-only audit notes landed on 5.2 (outbox index), 6.2 (API bind +
test-executor gating), and 6.5 (idempotency-token bound + fingerprint).

**Known-and-accepted (no code change).** `ReadyStale` re-dispatch under
sustained backlog amplifies with load (bounded by the advisory lock +
per-sweep Limit; correctness-safe); GETs are three pool reads by design;
`ctl` links pgx transitively (recorded at 4.6, M6.6); `errFencedCompletion`
is matched by no caller — kept as documentation of the no-ACK contract.

## Milestone 5 — Fault tolerance & execution control

### 5.1 — ADR-006 & retry policy schema ✅

**Delivered.** [ADR-006](adr/006-failure-taxonomy-and-retries.md)
(failure taxonomy & retry semantics), and the definition-contract half
of it in `internal/dag`: the per-step `retry` field, the top-level
`on_failure` workflow failure policy, bounds validation, regenerated
JSON Schema, and fixtures carrying explicit policies. No engine, store,
or queue code changed — 5.2–5.6 implement against the ADR.

**The ADR's decisions**, in brief: five error classes (`transient` /
`permanent` / `timeout` / `cancelled`, with `validation_failed`
reserved-and-rejected until M11) recorded as attempt outcomes; a
14-row taxonomy table mapping every 4.x failure path to class +
disposition, with rows 4–7 (registry miss, invalid config, CEL edge
failures, corrupt stored content) force-classified permanent;
**unclassified executor errors default to transient** (idempotency
keys make retry safe; the reverse default fails runs on network
blips); **`lost` attempts do not consume the retry budget** (crash
loops are bounded by the queue's delivery-count poison threshold —
the two counters never mix); backoff = `min(cap, initial ×
multiplier^(n−1))` with full jitter, served by 3.5's delayed ZSET
under new reason `retry`; `failed` becomes a routing state passed
through in the completion transaction (`→ retrying → ready` or
`→ dead_lettered`), with Postgres as the DLQ (a `dead_letters` table,
sources `retries_exhausted | permanent | poison`, requeue counting the
budget from a recorded baseline since attempt rows are immutable);
`fail_fast` (default, today's behavior) vs
`continue_independent_branches` (eager write-off of blocked
descendants to `cancelled` so the run rollup stays a counter check);
effective policies are materialized onto `run_steps` at instantiation
(5.2's migration) so failure paths never reparse the snapshot and
worker upgrades cannot change an in-flight run's policy.

**Schema changes** (all additive, `schema_version` stays 1 per
ADR-003): `Step.Retry *RetryPolicy` (`max_attempts`, `backoff`
{`initial`, `cap`, `multiplier`}, `jitter`, `retry_on`) and
`Definition.OnFailure` in `definition.go`/`retry.go`; `ErrorClass`
lives in dag (the contract references it; exec will alias it in 5.2).
Decode enforces shape + closed enums (unknown jitter mode, unknown
error class, unknown failure policy — `decodeRetry` mirroring the
JoinMode convention); Validate enforces the bounds table under two new
codes `retry_field_required` / `retry_field_invalid` (`max_attempts`
1–100 with 0-means-absent per the `max_iterations` convention;
**backoff `initial` and `cap` both required when a backoff block is
present**, parseable positive durations ≤ 1h/24h, cap ≥ initial;
multiplier 1–100; `retry_on` non-empty, duplicate-free, from
{transient, timeout} — `validation_failed` rejected with a
reserved-for-M11 message, `permanent`/`cancelled` as never-retryable).
Canonical encoding: `on_failure` after `description`, omitted when
absent (absent = fail_fast, the EdgeNormal convention); `retry` rides
Step's struct order after `config`.

**Tests.** Seven new decode-corpus fixtures (bad policy/jitter/class,
unknown fields at both nesting levels, wrong types, non-object) and
three structural fixtures (missing cap; a two-step bounds gauntlet;
retry_on abuse incl. the M11 reservation) — both corpus-coverage
pinning tests extend automatically; typed-decode test asserts full,
partial, and absent policies plus `on_failure`; a hand-built Validate
test covers codec-bypassing paths (unknown class string, empty backoff
block); the kitchen-sink construct-coverage test now also requires an
explicit `on_failure`, one full retry block, and one partial block, so
the fixtures cannot go stale. Schema `$defs` test pins RetryPolicy +
BackoffSpec. Full integration suite (compose stores) re-run green,
crash suite included.

**Fixtures.** `examples/definitions/kitchen_sink.json` gained
`on_failure: continue_independent_branches`, a full retry block on
`flaky_probe` (the step type built to exercise 5.2), and a partial
`max_attempts: 5` on `fetch_sources`; `linear.json` gained a minimal
policy on its network-bound `fetch`; testdata/valid/kitchen_sink.json
mirrors all three spellings for the roundtrip corpus; READMEs updated.

**Non-obvious decisions / deferred.**
- The classification *mechanism* (typed `ClassifiedError` in exec,
  default-transient rule) is specified in the ADR but deliberately not
  implemented — 5.2 lands it with its first consumer, keeping this
  ticket schema-only.
- `retry_on` decode accepts all five classes (they are the enum);
  which classes are *admissible* is validation — so the M11 unlock is
  a validator change, not a codec change.
- An explicit `"max_attempts": 0` is indistinguishable from an absent
  key (Go zero value) and silently means "engine default" — same
  accepted ambiguity as `max_iterations`/`fail_n_times.n`, recorded in
  the struct docs.
- An empty `"retry": {}` block is valid and means all-engine-defaults;
  no warning issued.
- ADR-003/004/005 needed no amendments: all three had reserved exactly
  the slots this ADR fills (the `retry` field, the M5 statuses and
  outcome vocabulary, the `retry`/`dlq_requeue` reasons).

### 5.2 — Retry engine with backoff ✅

**Delivered.** The runtime half of ADR-006 for retryable failures:
worker-side classification, the `retrying` step status with durable
scheduling state, exponential-backoff-with-full-jitter computation, the
delayed-ZSET re-dispatch (the delayed queue's first production tenant),
the run-status guard on claims, and reconciler coverage for the
failure-commit/delayed-schedule crash gap. Exhausted and never-retryable
failures land on the 4.x terminal path (`FailStep`+`FailRun`) with their
class recorded — 5.4 upgrades that path to `dead_lettered`.

**Migration 0003** (`retry_semantics`): `run_steps.retry_policy` (JSONB
NOT NULL — the *effective* policy, authored fields merged over engine
defaults, materialized at instantiation by `CreateRun` via the new
`dag.ResolveRetryPolicy`; existing rows backfilled with the default
policy) and `run_steps.next_attempt_at` (set while `retrying`); status
CHECK gains `retrying` (`dead_lettered`/`cancelled` wait for 5.4);
`step_attempts.outcome` gets ADR-004's postponed CHECK with the classed
vocabulary (`succeeded|transient|permanent|timeout|cancelled|lost`),
retiring the bare `failed` (backfilled to `permanent`); partial index
`run_steps_retrying_idx (next_attempt_at) WHERE status='retrying'`; and
`task_outbox(run_id, step_id)` — the post-M4 audit note, resolved because
the retry heal added a third anti-join over the outbox.

**Store.** `RetryStep` — the claim-fenced `running → retrying` CAS
(ADR-006's `failed` routing state is passed through *inside* the
transaction, never rested): closes the attempt with its class, records
the error, clears `claim_id`, stamps `next_attempt_at`, appends the new
`step_retry_scheduled` event, and deliberately does **not** bump
`steps_failed` (the rollup guard must pass when the retry succeeds).
`FailStepArgs` gained a required `Outcome` (the judged class — an
exhausted transient records `transient` even though the disposition is
terminal). `ClaimRunStep` widened: `ready`, or `retrying` with
`next_attempt_at <= now` — the backoff guard that bounces early duplicate
deliveries — clearing `next_attempt_at` on claim; `LockRun` now returns
the run's status and `ClaimStep` refuses steps of non-`running` runs with
the new typed reason `run_not_running` (the mechanism 5.6's park/cancel
reuses). `CountCountedFailures` (AttemptRepo) derives the durable retry
budget — outcomes `transient|timeout` only, `lost` excluded by
construction, the same derivation 5.4's requeue-baseline will reuse.
Reconciler reads grew `ListOverdueRetryingSteps` (due more than a
threshold ago, anti-join against pending outbox rows — the
`reconcile_ready` shape); `ListStalledRuns` counts `retrying` as live.
`StepRepo.Create`/`CreateBatch` default a nil `RetryPolicy` to the
serialized engine-default policy (the eventRepo nil-payload convention;
instantiation always materializes explicitly).

**Exec.** `ErrorClass` (alias of dag's), `ClassifiedError` with
`Transientf`/`Permanentf` constructors, `errors.As`-unwrappable anywhere
in a chain. Executors declare only transient/permanent; timeout and
cancelled stay engine-assigned (5.3/5.6), `validation_failed` reserved.

**Engine.** `classifyFailure` (pure): declared classes honored,
`ErrUnknownType`/`ErrInvalidConfig` force-permanent (taxonomy rows 4–5),
everything else default-transient (row 1); a mis-declared class
(reserved/engine-owned) falls back to transient and logs. Call sites that
know better pass the class explicitly — CEL edge failures, corrupt
params/config/join decodes (rows 6–7) are permanent. `retryDelay` (pure):
`min(cap, initial × multiplier^(n−1))` with overflow landing on the cap;
full jitter draws from an injected `[0,1)` source (`WithJitterRand`),
`none` exact. `completeFailure` became the router: one transaction counts
prior counted failures, and either `RetryStep` (class ∈ `retry_on` and
`prior+1 < max_attempts`; `n = prior+1` feeds the backoff) or
`FailStep`+`FailRun`. Post-commit, the retry route schedules the delayed
envelope — reason `retry`, **no EnqueuedAt** so successive retries of one
step encode to byte-identical ZSET members (ZADD move-the-fire-time
dedup, at most one pending retry dispatch per step) — through the new
`RetryScheduler` seam (`WithRetryScheduler`, satisfied by
`*queue.Delayed`; nil or a failed Schedule only logs: the delivery still
ACKs because the durable row carries the retry and the reconciler
re-dispatches). Claim classifier grew three ack-drop branches:
`run_not_running`, `retrying`-not-yet-due (fresh or reclaimed), and
retry-routed-between-reclaim-and-takeover; the post-takeover claim
rolls the takeover back when the run guard rejects. The reconciler
sweep gained the overdue-retrying scan + `reconcile_retry` outbox heal
(`ReconcilerConfig.RetryStale`, `ReconcileResult.RetriesHealed`).

**Config/wiring.** `WorkerConfig.ReconcileRetryStale`
(`AGENTLOOM_WORKER_RECONCILE_RETRY_STALE`, default 1m — measured from
`next_attempt_at`, needs only ≫ promoter tick, no lease-TTL margin);
`cmd/worker` wires `q.NewDelayed(cfg.Queue.DelayedKey)` as the engine's
scheduler (same key as the consumer's promoter duty, so any worker
promotes any worker's retries); `ctl watch` renders `retrying` (`↻`).

**Tests.** Unit: backoff math (exponential/cap-clamp/multiplier-1/full-
jitter bounds with pinned draws/corrupt-duration errors), classification
table, materialized-policy decode, `ResolveRetryPolicy` field-wise merge
(+ no aliasing of the authored slice, JSON round-trip), the new claim/
takeover classifier branches. Integration (fully injected time — engine
and reconciler on a shared fake clock, consumers' promoter tick parked at
1h, promotion driven by hand): the headline `TestRetrySucceedsWithinBudget`
(fail_n_times(2)/max 3/jitter none: next_attempt_at exactly +1s then +2s,
promotion refuses just-before-due, attempts transient/transient/succeeded,
`steps_failed` 0, two `step_retry_scheduled` events, quiescent);
exhaustion (max 2: terminal on the second counted failure, full error
history, class recorded in `run_steps.error`); declared-permanent
(one attempt, nothing scheduled); `TestRetryCrashGapHealedByReconciler`
(failing `RetryScheduler` — commit lands, delayed empty, entry ACKed;
sweep heals with one `reconcile_retry` row, second sweep idempotent,
drain → due claim → success). Store-level: `RetryStep` lifecycle (no
`steps_failed` bump, early claim bounces with From=`retrying`, due claim
issues attempt 2 with fresh fence and cleared `next_attempt_at`),
fencing + outcome-vocabulary guards, the run-status guard (rejects
before any write), and `CountCountedFailures` excluding `lost` and
`permanent`. Migration round-trip test walks 0003 down (retry columns
gone, 0002 tables intact). Full integration + crash suites green.

**Docs.** ADR-004 amended: `retrying` in the vocabulary; as-built matrix
rows (`running → retrying` direct CAS; `retrying → running` at claim,
guarded on `next_attempt_at` — delayed promotion writes nothing to
Postgres, so the reserved `failed → retrying → ready` hops collapsed);
run-status guard on the claim row; `run_steps` columns; outcome CHECK;
two new indexes. ADR-005 amended: reasons `retry` + `reconcile_retry`;
ACK-discipline rows for the retry commit, not-yet-due drops, and the
run-status guard; new crash cell **P3** (commit-then-schedule gap) and
R1(d) now pointing at the real tenant; the deliberate no-EnqueuedAt
dedup contract; tuning row for the retry-stale threshold. ADR-006 gained
an as-built note recording the collapsed transitions.

**Non-obvious decisions / deferred.**
- The conceptual `failed`/`ready` hops are not materialized: fewer
  states observable ⇒ fewer races; the claim guard doubles as the
  backoff enforcer.
- ACK-after-commit even when the delayed schedule fails: the row is the
  truth, the reconciler the backstop — a handler error there would
  un-ACK a consumed delivery and redeliver into an ack-drop loop.
- Exhaustion and not-retryable land on `failed` (still terminal until
  5.4), so `fail_fast` behavior is unchanged this ticket.
- The engine defaults live in `dag` (`ResolveRetryPolicy`) because the
  contract documents them; the store and engine share that one merge.
- A duplicate delivery of a *due* retrying step claims and executes —
  deliberate: it is exactly the self-heal path for a lost delayed entry,
  and the claim CAS still admits only one winner.
- Deferred to 5.3/5.4/5.6: `timeout`/`cancelled` class assignment,
  `dead_lettered` + the failure policy, poison wiring, and the
  dispatcher skipping terminal runs (today those rows drain, deliver,
  and ack-drop at the run-status guard — harmless, slightly wasteful).

### 5.3 — Step execution timeouts ✅

**Delivered.** Per-step execution timeouts end to end: the `timeout`
step-envelope field in the definition contract, its materialization onto
`run_steps` (migration 0004), and deadline enforcement in the engine's
execute path — the attempt records the `timeout` class (ADR-006 taxonomy
row 3, engine-assigned from context state) and routes through 5.2's
retry engine unchanged. ADR-006 gained an as-built "Step execution
timeouts" section documenting the mechanism and the watchdog/heartbeat
interplay.

**Contract** (`internal/dag`). `Step.Timeout` — a Go duration string,
sibling of `retry`, uniform across step types; absent = no timeout. The
clock bounds one *attempt* and starts when the executor starts (queue
wait and claim latency do not count). Strict decode admits the new key
(string-typed); Validate enforces parseable / positive / ≤ 24h
(`MaxStepTimeout`, same ceiling as `backoff.cap`) under new code
`timeout_field_invalid`; canonical encoding rides struct order with
omitempty; JSON Schema regenerated. Kitchen-sink fixtures (both corpora)
carry timeouts and the construct-coverage pin requires one.

**Store.** Migration 0004: `run_steps.timeout TEXT` nullable — the
duration string verbatim (the `ResolvedRetryPolicy` convention:
readable in the row, guaranteed parseable by submit-time validation),
NULL = no timeout so pre-5.3 rows need no backfill. Materialized at
instantiation alongside `config`/`retry_policy`; sqlc regenerated
(`gen.RunStep.Timeout *string`). The direct-create repo path defaults
nil (no timeout) — unlike `retry_policy` there is no merge to apply.

**Engine** (`internal/engine/timeout.go` + the `execute` wiring).
Enforcement is **synchronous cooperative cancellation**: `runExecutor`
wraps the executor call in `context.WithTimeout` when a timeout is set
and reports `expired` from context state after it returns. Deliberately
no detached goroutine that abandons the executor at the deadline — that
would allow the same step to run concurrently with its own retry inside
one process (side-effect double-fire) and permit real goroutine leaks;
synchronously there is nothing to leak by construction. A joined
watchdog observer goroutine logs when the deadline fires while the
executor is still running (the only sign of life from an executor that
ignores cancellation — which stalls its consumer visibly, heartbeat
alive, until `ReconcileRunningStale`'s cap takes over; that knob's
comment now says to size it above the largest configured timeout).
Routing rules: expired + executor error → `completeFailure` with
`ClassTimeout` (wrapping "step timed out after X"), bypassing
`classifyFailure` like the registry-miss path; parent cancellation is
*not* a timeout (`context.Canceled` ≠ `DeadlineExceeded`) and keeps its
4.x redeliver route until 5.6; success racing the deadline is honored
with a warning (finished work is never discarded — the fenced CAS
guards correctness). A corrupt materialized timeout (unparseable /
non-positive) lands a permanent failure completion, mirroring the
corrupt-retry-policy handling. Completions always run on the parent
ctx, never the expired child.

**Tests.** Unit (`timeout_test.go`): `stepTimeout` table,
`timedOut` discriminator, and `runExecutor` contracts — sleep(10s)
under 20ms cancelled at the deadline with the goroutine count settling
back to baseline (the no-leak acceptance criterion, stack dump on
failure), zero-timeout installs no deadline, success-after-deadline
preserved with `expired=true`, parent-cancel not judged timeout.
Integration (`timeout_integration_test.go`): headline
`TestTimeoutRetriesPerPolicy` — sleep(10s)/timeout=100ms/max_attempts=2
records outcome history exactly `[timeout, timeout]`, retries on 5.2's
injected-clock backoff (due at +1s, jitter none), exhausts to
step/run failed, and proves timeout ≠ crash: zero `step_reclaimed`
events, no `lost` outcomes, error payload carrying the message + class;
`TestTimeoutNotHitIsInert` — a generous timeout changes nothing. The
watchdog deadline is genuinely wall-clock-bound, so these follow the
queue package's timing convention (small real durations, sleep ≫
timeout) while backoff stays on the fake clock. Migration round-trip
test bumped to version 4 (one Down drops `timeout`, keeps 0003's
columns). dag corpus: `timeout_wrong_type.json` (decode) and
`timeout_bad_bounds.json` (structural, all three bound violations).
Full unit + integration + crash suites green; lint clean.

**Non-obvious decisions / deferred.**
- `timeout` lives on the step envelope (like `retry`), not in per-type
  config — it is uniform semantics the engine owns, and executors never
  see it (they just observe ctx cancellation, per ADR-006).
- Stored as TEXT duration string rather than integer millis for
  consistency with the retry policy's duration convention; the engine
  parses per execution (cheap, validated at submit).
- `expired` is judged *after* the executor returns rather than by
  racing a select: the class must reflect what actually happened to the
  context, and an executor may return a real error microseconds before
  the deadline — that stays its own class.
- Timeouts larger than `ReconcileRunningStale` never fire (takeover
  wins first). Documented in the knob's comment and ADR-006 rather than
  cross-validated at submit time — the bound is a deployment knob the
  definition layer cannot see.
- Deferred to 5.6: assigning `cancelled` on parent-context
  cancellation; today a shutdown mid-execution still redelivers (4.x
  behavior, unchanged).

### 5.4 — Dead-letter handling ✅

**Delivered.** ADR-006's terminal-failure half, end to end: the
`dead_lettered` and `cancelled` step statuses, the `dead_letters` table
(Postgres as the DLQ), the run disposition per the materialized workflow
failure policy (`fail_fast` vs `continue_independent_branches` with eager
write-off of blocked descendants), the poison-handler wiring that turns
3.4's delivery-count callback into a durable DLQ record + ACK, and the
internal requeue op (`Engine.Requeue`; API exposure lands in M6.5) with
run revival, descendant revival, and the budget-from-baseline re-arm.

**Migration 0005** (`dead_letters`): step-status CHECK gains
`dead_lettered` + `cancelled` (`failed` stays for pre-5.4 rows — retired
as a resting state, nothing writes it); `runs.on_failure` (TEXT NOT NULL
default `fail_fast` — the policy materialized at instantiation like
`retry_policy`, the dead-letter path never reparses the snapshot) and
`runs.steps_cancelled` (the write-off counter, keeping the rollup a
counter check); the `dead_letters` table — `(run_id, step_id, seq)` PK
with FK to `run_steps`, `seq` = per-step death count, `source`
(`retries_exhausted | permanent | poison`), nullable `class` (NULL for
poison — nothing judged it), `error`, `payload` (raw envelope, poison
only), `attempts_at_death` (the requeue baseline). Down migration
collapses `dead_lettered → failed`, `cancelled → pending`.

**Store.** `FailStep` is gone, replaced by **`DeadLetterStep`** — the
claim-fenced `running → dead_lettered` CAS (the conceptual `failed`
routing state passed through in-tx, exactly like `RetryStep`): closes the
attempt with its judged class, bumps `steps_failed`, inserts the
`dead_letters` row (seq allocated under the run lock), appends
`step_dead_lettered`. **`PoisonDeadLetterStep`** — unfenced, from any of
`{pending, ready, running, retrying}`: clears `claim_id` (a zombie
holder's completion then fences off), closes a running step's dangling
attempt as `lost`, records source `poison` with NULL class + raw payload.
**`CancelStep`** (`pending → cancelled`, `steps_cancelled`++, event
reason `upstream_dead_lettered`), **`ReviveStep`** (`cancelled →
pending`, the undo — legal because write-off never touches counters or
edges), **`RequeueStep`** (`dead_lettered → ready`, error/schedule state
cleared, `steps_failed`--, `step_requeued`), **`ResumeRun`** (`failed →
running`, `run_resumed` — requeue's revival; 5.6's unpark is separate),
and **`FailRunRollup`** (`running → failed` when all steps are terminal
with ≥1 failed — the continue-mode terminalizer). `runConflict` grew a
`want` parameter for the non-running from-statuses.
`CountCountedFailures` now counts attempts past
`MAX(dead_letters.attempts_at_death)` — the requeue re-arms the full
budget with attempt history untouched. New read surface: `DeadLetterRepo`
(list by step/run) and `StepRepo.ListReadyWithoutOutbox` (the requeue
re-dispatch set). The three reconciler step scans (`stale-ready`,
`stale-running`, `overdue-retrying`) now require the run to be `running`
— a failed run legitimately strands ready/running/retrying steps, and
healing them forever would churn re-outbox → deliver → ack-drop every
sweep.

**Engine** (`internal/engine/deadletter.go`). **`planWriteOff`** — a pure
fixed-point over the run's steps + edges: sources in `{dead_lettered,
cancelled, failed}` never resolve their out-edges; a pending non-join-any
target is impossible when any unresolved incoming edge is blocked; a
join-any only when `fired_deps = 0` and every unresolved incoming edge is
blocked (ADR-006's survival rule); newly-impossible steps propagate. A
corrupt join config errors out to the redeliver-to-poison path rather
than guessing. **`deadLetterDisposition`** (shared by the judged and
poison paths, in-tx): `fail_fast` → `FailRun` (conflict dropped);
`continue` → write-off walk + `FailRunRollup` attempt.
`completeFailure`'s terminal branch now dead-letters with source
`retries_exhausted` (retryable class out of budget) or `permanent`
(everything else: declared/forced permanent, class outside `retry_on`,
corrupt policy), reads `on_failure` in-tx under the run lock, and applies
the disposition. `attemptRunRollup` became the dual attempt —
`SucceedRun`, then on guard conflict `FailRunRollup` — so the last
terminal step of a partially-failed continue run lands the run `failed`.
**`HandlePoison`** (wired as `cmd/worker`'s `PoisonHandler`):
envelope-decodable poison dead-letters the step + disposition in one tx,
then ACKs — the one failure-path ACK, because the DLQ row is the durable
consumption; an undecodable envelope has no step identity to key a row
to, so it is logged loudly with its raw contents and consumed (ending the
pre-5.4 designed pending spin); already-terminal and dangling-reference
entries are consumed as stale; transport failures stay pending for the
next reclaim pass. **`Requeue`** — one tx: `RequeueStep`, `ResumeRun` if
the run was failed, revival recompute (rerun `planWriteOff` with
cancelled treated as pending and only the remaining dead-lettered steps
as seeds; cancelled steps outside the recomputed set revive), and
`dlq_requeue` outbox rows for every ready step with no pending dispatch
(the requeued step plus fail_fast siblings whose deliveries were
ack-dropped while the run was failed); post-commit dispatcher nudge. The
claim and takeover classifiers ack-drop on the two new terminal statuses
— without this, the delayed retry entry of a poison-dead-lettered
retrying step would redeliver forever and re-poison.

**Wiring.** `cmd/worker` passes `eng.HandlePoison` through
`consumerConfig` (the mapping test now asserts pass-through); `ctl watch`
renders `dead_lettered` (`†`) and `cancelled` (`⊘`) and prints the error
line for dead-lettered steps.

**Tests.** Unit: `planWriteOff` table (chain propagation, independent
branch, join-all vs join-any survival, transitive join-any death, legacy
`failed` source, resolved/loop edges ignored, corrupt join config), the
four new claim/takeover classifier branches. Store integration:
`DeadLetterStep` full-context (source vocabulary + fencing + counters +
event + baseline), `PoisonDeadLetterStep` per-status (running loses fence
and attempt, ready dies with zero attempts, terminal rejects typed),
die → requeue → die (seq 2, `attempts_at_death` 3, budget re-armed,
double-requeue conflict), `CancelStep`/`ReviveStep` counter symmetry +
`FailRunRollup` guard, `on_failure` materialization; the transition
matrix grew the three new transitions and the two new from-statuses;
migration round-trip walks version 5. Engine integration:
`TestContinueIndependentBranches` (dead-letter cancels exactly the
blocked descendant with reason recorded, independent branch delivers
partial results, run terminalizes failed once),
`TestPoisonMessageReachesDLQ` (panicking executor walks the entry to the
threshold; every judged-nothing attempt closed `lost`; raw envelope in
`payload`; queue quiesces instead of redelivering forever),
`TestRequeueReExecutesAndCompletesRun` (fail_fast death → requeue →
attempt 3 fails again but *retries* on the re-armed baseline instead of
re-dead-lettering → succeeds; run succeeded with `steps_failed` 0),
`TestRequeueRevivesWrittenOffDescendants` (continue-mode requeue revives
the cancelled descendant and the run completes clean). The 4.x/5.2/5.3
failure tests updated to the new terminal state. Full unit + integration
+ crash suites green; lint clean.

**Non-obvious decisions / deferred.**
- `steps_failed` counts `dead_lettered` (continuous with legacy `failed`
  rows); requeue decrements it — so a cured run passes the unchanged
  SucceedRun guard.
- Undecodable-envelope poison: log-with-contents + ACK, no DLQ row — the
  table is keyed to a real step, and an orphan row would be
  unrequeueable. Recorded in ADR-005's as-built note.
- Requeue revival is a full recompute, not provenance tracking — correct
  with multiple dead steps. 5.6 interaction noted: once run-cancel also
  writes `cancelled`, revival must not resurrect those (the recompute
  treats every cancelled step as revivable today; 5.6 will need a
  reason column or an equivalent guard).
- The dispatcher still drains outbox rows of failed runs (deliveries
  ack-drop at the run-status guard) — dispatch gating deferred to 5.6,
  which needs it uniformly for park.
- Poison on a `retrying` step dead-letters it even though its delayed
  retry entry is still out there — the claim classifier's new
  terminal-status drop consumes that entry harmlessly when it fires.

### 5.5 — Idempotency keys & side-effect journal ✅

**Delivered.** The "re-execution is safe" premise ADR-006 and ADR-005's
accepted races lean on, made real: a stable per-(run, step) idempotency
key on `StepContext`, the `internal/exec/effects` journal
(record-intent → execute → record-result over the new `side_effects`
table) that makes external side effects effectively-once across retries,
reclaims, and zombie takeovers, loud misuse detection, and the
`effectful_echo` proof executor wired through the dag catalog, the
builtin registry, and both kitchen-sink corpora.

**The key is derived, not stored:** `effects.Key(runID, stepID)` is a
UUIDv5 over a fixed project namespace (constant `keyNamespace` — must
never change; a golden test pins the derivation and says so) and
`run_id/step_id`. Stability across attempts/reclaims/takeovers is by
construction, no migration or lookup involved, and the token is opaque
and header-safe for M8's `http_request` `Idempotency-Key`. The engine
stamps `StepContext.IdempotencyKey` + a bound journal handle
(`StepContext.Effects`, interface `exec.EffectJournal` — `exec` stays
store-free; the concrete `*effects.StepJournal` lives in the new
subpackage) in `execute()`, so executors see effect identity without
ever seeing run/step IDs (4.1's minimal-SPI stance preserved).

**Migration 0006** (`side_effects`): `(run_id, step_id, effect_id)` PK
with composite FK to `run_steps` (ON DELETE CASCADE), `status`
(`intent | done`, CHECK: done requires `result_at`), `attempt` +
`claim_id` (diagnostics — who last held the intent, never fencing),
`result` JSONB, injected-clock timestamps. Store surface: repo
primitives only (`SideEffectRepo`: Get/GetForUpdate/InsertIntent/
TakeoverIntent/Complete/ListByStep) — the protocol lives in the effects
package, which runs each phase in its own `WithTx`, deliberately never
holding a transaction across the external call.

**Protocol semantics** (ADR-006 gained the as-built section):
- Done row at Begin → short-circuit: the stored result returns, `fn`
  never runs. This is the exactly-once half.
- Dangling intent at Begin → takeover + re-execute: the recorder died
  between intent and result, the effect state is unknowable. The
  residual at-least-once window; the idempotency key absorbs it at the
  external service. Two fresh recorders racing the first insert are
  handled by a one-retry loop (the loser's second pass locks the
  winner's row via FOR UPDATE).
- Complete is first-wins (UPDATE guarded on `status = 'intent'`): a
  zombie's late result loses and reads back the stored result — the
  journal stays single-valued; step-level claim fencing already rejects
  the zombie's outcome, so the journal is deliberately not claim-fenced.
- `fn` error → intent left dangling, error returned as-is (retry
  re-executes).
- Misuse (Complete with nil/consumed/done token, or no intent row —
  "execute without intent") panics in strict mode (new
  `AGENTLOOM_EFFECTS_STRICT`, `WorkerConfig.EffectsStrict`, default
  **true** while everything is dev/test; engine option
  `WithStrictEffects`) — the panic rides the consumer's containment into
  no-ACK → poison threshold → DLQ, loud and bounded. Non-strict returns
  `*MisuseError` wrapped in a permanent `ClassifiedError` → clean
  dead-letter, never a retry.

**`effectful_echo`** (dag `StepEffectfulEcho` + executor in
`Builtins()`): journals one file-append (`key=<idempotency key>
attempt=<n>` — the file alone proves single-fire and key stability) via
`Do`, echoes `input` as output, and — the important knob — `fail_times`
fails attempts 1..N transiently *after* the journal, so a retrying step
takes N+1 attempts while the counter gains exactly one line. Validation:
`path` required, `fail_times ≥ 0`; `input` gets the same `compactRaw`
normalization as echo/tool/branch (the round-trip corpus caught that).
Both kitchen sinks grew a `notify` step with `fail_times: 1` and a
3-attempt policy (construct-coverage pins enforced the extension).

**Headline integration tests** (`internal/engine/
effects_integration_test.go`): retry short-circuit on the fake clock
(attempt history `[transient, transient, succeeded]`, file exactly one
line, journal row `done` recorded by attempt 1); the kill/reclaim
acceptance — worker A journals then stalls holding its claim, B
reclaims/takes over/short-circuits/completes, A's resumed completion is
fenced, file has one line under the one derived key, attempt history
`[lost, succeeded]`; misuse both ways (non-strict → permanent DLQ row
naming the misuse; strict → panic → poison DLQ row). Plus the journal
protocol suite in `internal/exec/effects` (short-circuit, dangling-
intent takeover, first-wins race, all four misuse shapes incl. the
raw-SQL vanished-row case, strict panic) and the store-primitive suite
(status guards, PK/FK conflicts). Full unit + integration + crash
suites green; lint clean.

**Non-obvious decisions / deferred.**
- No events for journal writes: they are not step state transitions, the
  table is the audit record, and skipping them avoids run-lock ordering
  questions inside executor runtime. Recorded in ADR-004's table note.
- The exactly-once claim is scoped honestly in ADR-006: after the result
  commits, re-execution is impossible; the effect-fired/result-uncommitted
  gap is irreducible without external cooperation, which is exactly what
  the key provides. Tests prove the scenarios where the result committed.
- Strict-mode default is true (dev/test posture); production flips
  `AGENTLOOM_EFFECTS_STRICT=false` for the clean dead-letter path. M6.2's
  test-executor-gating decision should revisit the default alongside it.
- `Begin`/`Complete` primitives are exported on `*StepJournal` (not on
  the `exec.EffectJournal` interface) for future multi-part effects; the
  interface stays `Do`-only so well-behaved executors cannot misuse the
  protocol at all.
- The `counter` executor stays unjournaled on purpose — it measures how
  often the engine really executed (4.7's probe); `effectful_echo`
  measures that journaled effects fire once. The 5.8 chaos suite needs
  both.

### 5.6 — Cancel, park/resume, run deadlines ✅

**Delivered.** Run-level controls as engine ops (API exposure lands in
M6.5): cooperative **cancel** (`Engine.Cancel`), **park/unpark**
(`Engine.Park`/`Engine.Unpark`), and the optional **run wall-clock
deadline** (`max_wall_clock` on the definition envelope, enforced by the
reconciler). ADR-004's reserved run statuses `parked` / `cancelling` /
`cancelled` are realized by migration 0007, which also adds the typed
`park_reason` / `cancel_reason` columns and the nullable
`runs.deadline_at` with a partial scan index.

**Cancel converges through three mechanisms**, all serialized by the
run lock every transition takes first:
1. *The request*: one transaction CASes the run `running|parked →
   cancelling` (typed reason `manual` / `deadline_exceeded`, event
   `run_cancelling`), sweeps every claimless non-terminal step —
   pending, ready, retrying — to `cancelled` (the 5.4 write-off CAS with
   a broadened from-set; a retrying step's `next_attempt_at` clears;
   event reason `run_cancelled`), and attempts the finalization rollup
   `cancelling → cancelled` (all-terminal counter guard, event
   `run_cancelled`), which passes immediately when nothing was in
   flight.
2. *In-flight workers*: both completion transactions now read the run
   status under the run lock (`store.LockRunStatus`). On a cancelling
   run, a success is honored — output recorded — but fan-out is skipped
   (successors are already cancelled; the run must quiesce, not
   advance); a failure is not judged at all — no retry, no DLQ (ADR-006
   row 8) — the step settles through the new claim-fenced
   `store.CancelRunningStep` (`running → cancelled`, attempt outcome
   the administrative `cancelled`, never counted against the retry
   budget, executor error preserved). The latency bound is the
   *cancellation watch*: a joined poller goroutine per executor
   invocation (`WithCancelPollInterval`, config
   `AGENTLOOM_WORKER_CANCEL_POLL_INTERVAL`, default 10s ≈ heartbeat
   cadence) that reads run status unlocked (`RunRepo.GetStatus`) and
   cancels the executor context with a typed cause (`errRunCancelled`).
   The watch is pure latency — the in-transaction check is the
   authority, so a disabled watch (interval 0) is slower, never wrong.
3. *The reconciler*: a new scan for stale `running` steps of
   **cancelling** runs (the 5.4 run-status filter deliberately excludes
   them from the ordinary heal) settles dead workers' steps with
   takeover (attempt `lost`) + the sweep CAS + rollup — never a
   re-outbox: cancelled work is not re-dispatched.
Deliveries of a cancelling/cancelled run's steps are consumed by 5.2's
run-status claim guard (ack-drop) — that is what leaves no orphan PEL
entries. `Engine.Requeue` refuses cancelling/cancelled runs: a cancel is
terminal by operator intent.

**Park is a pure dispatch pause.** `running → parked` with a typed
reason (`manual` now; `budget_exceeded`/`awaiting_human` reserved in the
CHECK for M10/M15). The claim guard refuses parked runs; in-flight steps
settle normally and fan-out proceeds — newly-ready successors get
dispatched, their deliveries bounce and are consumed, and unpark
(`parked → running`, reason cleared) re-outboxes every ready step with
no pending dispatch row (5.4's `ListReadyWithoutOutbox`) under new
outbox reason `unpark`, then nudges the dispatcher. Overdue retrying
steps are deliberately left to the ordinary overdue-retrying scan, which
admits them again once the run is running. **Rollups fire from parked**:
`SucceedRun`/`FailRun`/`FailRunRollup` accept `running|parked`
(clearing `park_reason` on exit), so a parked run whose last in-flight
step lands terminalizes honestly instead of resting parked-but-done —
the rejected alternative (deferred disposition re-derived at unpark) was
more machinery for a less honest status. ADR-004's matrix carries the
new rows.

**The deadline** is contract + materialization + reconciler duty:
`Definition.MaxWallClock` (Go-duration string, positive, ≤ 30 days —
`dag.MaxRunWallClock` — new code `max_wall_clock_field_invalid`, JSON
Schema regenerated, kitchen-sink fixtures + construct pins extended);
instantiation stamps `deadline_at = created_at + max_wall_clock`; a
fourth reconciler scan (`ListDeadlineExceededRuns`, running|parked with
`deadline_at` past the injected now, served by the partial index) feeds
the same `cancelRunTx` sweep with reason `deadline_exceeded`. Parked
runs are eligible — the wall clock does not pause with dispatch.
`ListStalledRuns` also now flags cancelling runs with no running step
(impossible-state loudness), and `HandlePoison` attempts the cancel
rollup so a poison dead-letter can finalize a cancelling run.

**Tests.** Store suite (`runctl_integration_test.go`): every new
transition's guards/conflicts/events/counters, the broadened CancelStep
(pending/ready/retrying, matrix test extended with `alsoLegalFrom`),
CancelRunningStep fencing, rollups-from-parked, deadline
materialization, and both new scans. Engine suite
(`runctl_integration_test.go`): the headline mid-run cancel (in-flight
30s sleep interrupted by the watch within its poll interval, sweep
cancels the successor, attempt history exactly `[cancelled]`, zero
retry events, full queue quiescence — PEL/outbox/delayed all empty),
idle-run cancel finalizing in the request transaction,
success-racing-cancel honored (output recorded, fan-out skipped, run
cancelled), park → fleet-stops-claiming (successor readied by a parked
completion, its delivery consumed with zero attempts) → unpark →
completion, deadline-exceeded on the injected clock (idempotent second
sweep), and the cancelling-run crash heal (stalled holder; heal lands
`[lost]` + cancelled + run cancelled; the released zombie's completion
is fenced and its entry drains through reclaim into ack-drop). `ctl
watch` now treats `cancelled` as terminal (exit 1) and keeps polling
through `parked`/`cancelling`. Full unit + integration + crash suites
green; lint clean.

**Non-obvious decisions / deferred.**
- "Context cancellation at next heartbeat" is implemented as an
  engine-side status poller, not a queue-heartbeat hook: the queue is
  transport-only by design (it must not know the store), and the poll
  interval defaults to the heartbeat cadence, honoring the ticket's
  latency intent. Correctness never rests on the poller.
- Cancel/park reasons are validated in the store wrappers *and* CHECKed
  in the schema; the step-cancel event reason vocabulary gains
  `run_cancelled` alongside 5.4's `upstream_dead_lettered`.
- A success racing the cancel is honored (5.3's success-racing-deadline
  precedent): discarding done work would waste budget and re-run side
  effects. The nuance is per-step statuses on a cancelled run — some
  succeeded, the rest cancelled.
- The down-migration collapses `parked`/`cancelling` to `running` and
  `cancelled` to `failed` (nearest pre-5.6 readings; reasons and
  deadline dropped — lossy, like 0003's outcome collapse).
- `TestSchemaV1StatusChecks`'s out-of-vocabulary probe moved off
  `'parked'` (now legal) to a value no migration will ever admit.
- Step transitions still carry no run-status guard at the store layer
  (ADR-004's accepted trade, note updated): the *engine's* completion
  transactions branch on the locked status instead, and parked runs
  deliberately accept normal completions.

### 5.7 — Graceful shutdown & drain ✅

**Delivered.** SIGTERM is now a first-class lifecycle event, not a
simulated crash: the worker stops claiming, finishes everything it
already holds — heartbeating until done, acking after each completion
commits — and exits clean; a configurable drain deadline bounds the
whole affair by abandoning the remainder to natural lease expiry.

**The consumer's two-phase shutdown** (`internal/queue/consumer.go`) is
the core. The pre-5.7 worker shared one context between the read loop
and the handlers, so SIGTERM cancelled the in-flight executor mid-step,
its completion transaction failed on the cancelled context, and the
un-acked entry cost the fleet a reclaim → takeover → `lost` → re-execute
cycle per restart — exactly the churn the ticket eliminates. Now
cancelling Run's ctx is only the *soft* stop: fresh `XREADGROUP`s and
all periodic duties (reclaim, promoter, janitor, trim) stop, while
handlers run under a **work context** — `WithCancel(WithoutCancel(ctx))`
— that a watchdog goroutine cancels `ConsumerConfig.DrainTimeout` after
the soft stop. Heartbeats and ACKs were already detached (3.3/3.4), so
a draining step keeps its lease and acks normally. The drain deliberately
covers the *whole PEL in hand*, not just the running handler: the
unprocessed remainder of the last read batch and reclaimed entries
mid-pass route through the same new `deliver` path (the per-entry
early-return on cancellation is gone), because every delivered entry is
a lease this consumer owes a disposition — abandoning a 15-entry batch
remainder would cost a reclaim cycle each under rolling restarts.

**Dispositions and exit hygiene.** Each entry in hand at shutdown gets a
logged disposition — `drained` (completed + acked), `redeliver` (handler
error; stays pending as in normal operation), `abandoned` (drain
deadline; lease expires into reclaim) — plus a `shutdown drain complete`
summary with counts, and the watchdog narrates drain start and deadline
excess. A consumer exiting with an empty PEL deregisters itself
(`XGROUP DELCONSUMER`) — safe because after the soft stop nothing can be
assigned to it, so the emptiness check is stable — ending the
one-stranded-consumer-per-restart drip the janitor existed to mop up;
anything pending keeps it registered (DELCONSUMER drops PEL state).
Zero `DrainTimeout` (the internal/queue default; `withDefaults`
deliberately leaves it) preserves the pre-5.7 immediate-cancel semantics
*including* no deregistration: that mode is the queuetest kill switches'
crash simulation and must stay a faithful sudden death.

**Engine abandon path** (`internal/engine/claim.go`): when the handler's
own context is cancelled after the executor returns (only shutdown does
this — the 5.6 run-cancel watch cancels a child), `execute` returns a
clean `step abandoned at shutdown` error without attempting the doomed
completion transaction: nothing was decided, no ACK, the lease expires
into another worker's reclaim/takeover — the abandon path *is* the crash
path, so no new recovery machinery exists.

**Deployable wiring** (`cmd/worker`, `internal/config`): new knob
`AGENTLOOM_WORKER_DRAIN_TIMEOUT` (`WorkerConfig.DrainTimeout`, default
25s — inside K8s's default 30s `terminationGracePeriodSeconds` with exit
margin; must be positive, the production worker always drains) mapped
into `ConsumerConfig` by `consumerConfig`. The dispatcher, reconciler,
and health loops moved onto a `loopCtx` that deliberately **outlives
SIGTERM** until `consumer.Run` returns: the draining steps' completions
fan successors out through the outbox, and the draining worker's own
dispatcher is what hands them to survivors promptly (previously the
loops died with the signal). The post-M4-audit bootstrap-failure defer
ordering is preserved, and the worker narrates the sequence (signal
received → draining → drained → stopping loops → stopped).

**Tests.** Queue level (`drain_integration_test.go`, + a queuetest
`ConsumerHandle.Interrupt` that cancels without joining — the SIGTERM
analogue Kill's blocking join can't provide): the in-flight hang stays
blocked across the interrupt and succeeds on release (the discriminator
against pre-5.7 cancellation), acked + quiescent + self-deregistered; a
batch of four with the first hanging drains all four exactly once; a
300ms deadline abandons an unreleasable hang (resolves to
`context.Canceled`, entry stays pending, consumer stays registered).
Crash suite (`test/crash/drain_integration_test.go`, real processes +
real SIGTERM via the new `workerProc.terminate` asserting exit 0):
**rolling restart under continuous load** — a submitter instantiates a
counter → sleep → counter run every 250ms while both workers are
SIGTERMed (each provably holding a lease first) and replaced; at
quiescence every run succeeded with every step's history exactly
`[succeeded]`, zero `step_reclaimed` events fleet-wide, every counter
file exactly one line, outbox empty; and the **drain-timeout scenario**
— 6s sleep vs 500ms budget: the victim exits 0 in well under the sleep,
logging the abandoned step's disposition, and the survivor reclaims,
takes over, and completes with history `[lost, succeeded]` and exactly
one `step_reclaimed`. Existing SIGKILL/chaos suites untouched and green.

**Non-obvious decisions / deferred.**
- "In-flight" was interpreted as *the whole PEL in hand* (running
  handler + read-batch remainder + reclaimed entries), not just the
  running step — required by the zero-reclaim-churn acceptance under
  batched reads, and what makes the empty-PEL deregistration meaningful.
- No detached grace for a completion racing the hard deadline: the
  deadline is the operator's hard bound (K8s SIGKILLs next). A commit
  torn down mid-flight either lands (the entry redelivers and ack-drops
  at the claim CAS) or rolls back — both safe, documented in ADR-005's
  amendment as accepted residuals.
- The rolling-restart scenario pins lease TTL at 5s (vs the suite's 2s)
  and batch 4: no one crashes there, so reclaim latency is irrelevant,
  and the wider TTL keeps a CI stall from triggering a spurious —
  harmless but assertion-muddying — reclaim of a queued batch entry.
- The reclaimer leaves an over-threshold poison entry pending when the
  drain deadline has already passed (next reclaimer diverts it) rather
  than racing a doomed DLQ write.

### 5.8 — Sustained chaos suite ✅

**Delivered.** `TestSustainedChaos` in `test/crash` — the milestone-closing
scenario: a continuous submitter cycles five mixed fixtures (one every
250ms) against a 3-worker fleet of real `cmd/worker` subprocesses while
random SIGKILLs land every ~3s and the Redis instance is restarted once
mid-run; at quiescence every run sits at exactly its fixture's expected
terminal state, journaled side effects are exactly-once by idempotency
key, the queue is fully quiescent (3.6's `WaitQuiescent`: stream drained,
PEL empty, delayed empty — with `DumpDiagnostics` on failure), and the
outbox is empty. CI short mode is a 24s chaos window (~28s wall, ~104
runs, 7 kills, one ~2.5s blip), verified green across 5 consecutive
`-race` runs; `make test-chaos-long` scales the window via
`AGENTLOOM_CHAOS_DURATION` (default 5m).

**The fixture mix** (inline JSON like 4.7's `crashDef`, per-run effect
files under `t.TempDir()`): *chain* (counter → sleep → counter), *retry*
(`fail_n_times(n=2)` under a 100ms-base full-jitter policy — exercises the
delayed ZSET + promoter through the blip), *effectful*
(`effectful_echo(fail_times=1)` — the journal short-circuit under chaos),
*fanout* (two sleeps of different lengths into a `join all` — readiness
counters under crash), and *deadletter* (`fail_n_times(n=1000)`,
`max_attempts=2`, `fail_fast` — the "explainably dead-lettered"
population: run failed with exactly one `retries_exhausted` DLQ row).
Every expected outcome is kill-proof by construction: `lost` attempts are
excluded from the retry budget (5.2), `fail_n_times` keys off the durable
attempt number, and the poison threshold is raised to 50 so random kills
bumping delivery counts can never poison-divert a healthy step (poison
has its own 5.4 suite; a poison DLQ row here fails the test).

**The Redis blip: a dedicated `redis-chaos` compose service** (default
profile, port 6380, own AOF volume, `restart: always`), because
restarting the shared test Redis would break every integration test
running in parallel. The test bounces it by issuing `SHUTDOWN NOSAVE`
through the client — no docker CLI coupling — and Docker's restart policy
brings it back. Two non-obvious findings baked into the code: go-redis
maps the dropped connection on SHUTDOWN to a *nil* error, and its
client-level retries mask the sub-second downtime from a Ping poll
entirely, so the restart is proven by the server **run_id changing**
(regenerated every server start), never by observing downtime. NOSAVE +
appendfsync-everysec deliberately permits ~1s of AOF tail loss: lost
XADDs, delayed ZADDs, and PEL entries are exactly the gaps only the
reconciler can heal — so unlike 4.7 (reconciler exiled at 10m), this
suite runs it HOT (1s sweeps, 2s ready/retry staleness, 8s running
staleness) and convergence to quiescence is the proof it healed every
gap. New knobs for operators/tests: `AGENTLOOM_REDIS_CHAOS_PORT`
(compose) and `AGENTLOOM_TEST_CHAOS_REDIS_ADDR` (suite), documented in
`.env.example`; `queuetest.NewAt(tb, addr)` (New now delegates to it) is
the only harness addition — zero production-code changes.

**Assertion discipline — chaos-tolerant by design.** Nothing here pins
attempt histories, reclaim counts, or churn: random kill timing makes
those nondeterministic, and they are owned by 4.7/5.7. What is asserted
is exact where exactness is honest: journaled effects are exactly-once
**by idempotency key** (every line carries the derived
`effects.Key(run, step)`, distinct-keys == 1, plus exactly one `done`
`side_effects` row) — raw line count is only ≥1 because a kill inside the
intent→result window re-executes the effect, the journal's *documented*
residual at-least-once that the stable key exists to let the external
system absorb. Unjournaled `counter` files are bounded by
[1, attempts] — the at-least-once contrast that motivates the journal.
The kill choreography stays on the test goroutine (spawnWorker's Fatalf
is illegal elsewhere), sequenced kills → blip → kills so no worker spawn
races the outage (bootstrap pings Redis); the RNG seed is logged for
post-mortem; convergence timeout failures dump stuck runs' step states
plus full queue diagnostics.

**Verified.** Short mode green 5× consecutively under `-race` (~27s
each); full `make test-integration` green alongside the parallel crash/
drain scenarios; `SHUTDOWN NOSAVE` + `restart: always` behavior confirmed
against the live compose service (RestartCount increments, run_id
rotates).

**Deferred / notes.** The blip is single by design (the ticket's "one
Redis restart blip"); long mode lengthens the kill window but keeps one
blip. Postgres chaos (restart/failover) is out of scope until the ops
milestones. If CI runner contention ever flakes the *other* crash-suite
tests while chaos runs in parallel, the recorded fallback is dropping
`t.Parallel()` from `TestSustainedChaos` so it serializes within the
package.

## Milestone 6 — API server & auth

### 6.1 — ADR-007 & API key model ✅

**Delivered.** [ADR-007](adr/007-authn-authz-and-api-rate-limiting.md)
(authentication, authorization & API rate limiting — the design contract
for all of M6 plus the limiter M9 reuses), migration 0008 (`api_keys`),
the `APIKeyRepo` store layer, key mechanics + the admin-gated `/v1/keys`
management routes in `internal/api`, the `AGENTLOOM_API_ROOT_KEY`
bootstrap path through `config.APIConfig`/`cmd/api`/compose, and
`ctl keys create/list/revoke`.

**The ADR's decisions**, in brief: opaque bearer keys over JWT for v1
(service-to-service simplicity, instant revocation as a DB row; JWT/OIDC
backlogged for human SSO). Key = `sk_` + base64url(32 random bytes) —
46 chars; stored as hex SHA-256 of the full plaintext (UNIQUE) plus an
11-char clear lookup prefix (UNIQUE, `sk_` + 8 random chars, regenerate
on the ~impossible collision). **Fast hash, deliberately no KDF**: the
secret is 256 random bits, so bcrypt/argon2 would add per-request CPU
for zero threat-model gain; HMAC-with-pepper rejected as an unrotatable
second secret. Verification = one indexed read by prefix + constant-time
hash compare + revoked/expired predicates. Four scopes (`submit`,
`read`, `approve` reserved for M15, `admin` implying all), with the
route→scope table covering current routes, 6.5's planned lifecycle
endpoints, and the exempt probes. Lifecycle: create (TTL resolved
against the *server's* injected clock — clients never supply
timestamps), soft first-wins revoke (row kept for audit, idempotent
re-revoke), expiry judged at auth time, no sweeper, no un-revoke, no
rotation primitive. 401 collapses every credential failure into one
indistinguishable answer (never reveals whether a prefix exists);
403 names the missing scope (the caller already proved possession).
Bootstrap circularity broken by the env root key: `sk_`-shape-validated
and hashed at boot (plaintext discarded), implicit admin, logged as
`key_id="root"`, never a DB row — documented flow is set → mint a real
admin key → unset.

**Implementation shape.** `internal/api/auth.go` holds the key mechanics
(`generateKey` on an injected reader, `hashKey`, shape check,
constant-time compare) and a **scope-parameterized `requireScope`
middleware** — 6.1 mounts it only on the `/v1/keys` subtree (the ticket's
scope; anonymous ingest routes are unchanged), and 6.2's job becomes
mounting it everywhere per the ADR table plus the two parked audit items
(compose `0.0.0.0` bind, `counter` executor gating). Store layer is
plain CRUD by design (keys are not run state machines): `Create` (UUID
defaulting, conflict-typed unique violations), `GetByPrefix` (the auth
read), `List` newest-first, and `Revoke` whose `revoked_at IS NULL`
guard makes first-wins explicit — zero rows then disambiguated into
"already revoked" (idempotent success) vs `ErrNotFound` by one read.
The create endpoint bounds its regenerate-on-collision loop at 3 and
returns the plaintext in that one 201 body; listings project rows
through `KeyView`, which never carries hash or plaintext. `ctl` grew a
persistent `--key`/`AGENTLOOM_API_KEY` bearer flag on the shared client
(`clientFromCmd`) and the `keys` subtree; `keys create` prints the
plaintext **alone on stdout** (mirrors `ctl submit`'s run-id-on-stdout
composability: `KEY=$(ctl keys create …)`), all narration on stderr.

**Secret hygiene, tested two ways.** The integration suite boots the API
with a captured slog stream and a mutable clock, drives the whole
lifecycle (401 matrix incl. WWW-Authenticate, 403 with named scope for
a non-admin key, root→admin→scoped mint chain, TTL expiry via clock
advance, revoke idempotency, revoked/expired keys collapsing to 401),
then asserts every minted plaintext absent from all `api_keys` columns
and the full log capture — with the created key's prefix *present* as
the positive control proving the assertion isn't vacuous. Separately,
CI's lint job greps the repo for committed `sk_`-shaped literals
(`sk_[A-Za-z0-9_-]{30,}`) and fails on any hit — which forces the
discipline that tests and docs construct key-shaped strings at runtime
(`"sk_" + strings.Repeat(...)`, runtime entropy) rather than pasting
examples.

**Non-obvious decisions.** Key management went through the API rather
than a ctl→Postgres path because 4.6 decreed ctl a pure HTTP client — so
6.1 ships the root-key gate early (a keys API with no auth would be
absurd; an authless bootstrap window doubly so). `admin` implies all
scopes (route checks are "has X or admin"; least-privilege keys just
don't request admin). Expiry uses not-before semantics (`now >=
expires_at` is expired). The root key deliberately cannot be revoked by
the DB it bootstraps — unsetting the env var is its revocation.
`obs/log` gained the canonical `key_id` field; auth outcomes log key_id
+ prefix only.

**Verified.** Lint + `go test -race ./...` + full compose-backed
`-tags integration ./...` green; live round-trip against `make up-app`
(root key in `.env` → `ctl keys create/list/revoke` → revoked key 401s).

**Deferred / notes.** Per-request Postgres read for auth accepted at v1
traffic (read-through cache is the recorded later optimization, rejected
now because it reintroduces revocation lag). No `last_used_at` tracking
(a hot-path write per request; revisit with M7 metrics). Rate limiting
is design-only here — `internal/ratelimit` lands in 6.3, enforcement in
6.4. Scope enforcement on `/v1/runs` is 6.2, not here: submitting runs
stays anonymous for exactly one more ticket.

### 6.2 — Auth middleware ✅

**Delivered.** Scope enforcement on every `/v1` route per ADR-007's
route→scope table, closing the anonymous-ingest window 6.1 left open for
exactly one ticket: `submit` on `POST /v1/runs`, `read` on
`GET /v1/runs/{id}`, `admin` on the `/v1/keys` subtree, `/healthz`
exempt for probes. Plus the two post-M4-audit items this ticket owned:
the compose api port now binds `127.0.0.1` by default, and the
filesystem-writing test executors are no longer registered on production
workers by default.

**Wiring, as predicted by the ADR — thin.** 6.1's scope-parameterized
`requireScope` middleware (verifier, 401-collapse, 403-with-named-scope,
key_id logging) needed no behavioral change; 6.2 mounts it per-route
with chi's `With`. What it grew: the authenticated identity (key id +
scopes) is stamped into the request context via `identityInto`/
`identityFrom` — the hook 6.4's per-key rate limiting will read — and
the key_id is reported back **up** to `requestLog`'s one-line-per-
request log through a mutable `authStamp` slot installed before routing
(a context written inside the routing tree cannot flow upward, so the
slot pattern is the honest mechanism; same-goroutine write-then-read,
no locking). The stamp is filled right after authentication, so both
success and 403 request lines carry key_id; 401s have no identity to
carry. Deliberate edge, documented in the package doc: 404/405 fallback
responses under `/v1` stay anonymous — chi's NotFound/MethodNotAllowed
run outside the subtree middleware, and route existence is public
knowledge (the spec), so nothing leaks.

**Route-coverage drift guard.** `auth_routes_test.go` (unit, in-package)
spells out the route→scope table as data and `chi.Walk`s the live
router both directions: every mounted `/v1` route must be in the table,
every table row must be mounted, and anything outside `/v1` other than
`/healthz` fails. A 6.5 route added without deciding its scope — or
mounted without `requireScope` and matrix coverage — fails a unit test,
not a review. (The router walk needs no database: the handler is built
over a nil-pool store.)

**Test-executor gating (the audit decision).** `counter` and
`effectful_echo` both append to a submitter-chosen filesystem path —
fine as crash-suite instruments, indefensible on an authed deployment.
Decision: registration, not sandboxing. `exec.CoreBuiltins()` is
Builtins minus exactly those two; `cmd/worker` registers core unless
`config.WorkerConfig.TestExecutors` (`AGENTLOOM_WORKER_TEST_EXECUTORS`,
default **false**) opts the full set in, logging the mode in its startup
line. docker-compose.yml overrides to true (the compose stack is the
dev/demo environment; `make demo-crash` needs `counter`), and the
crash-suite harness sets it on spawned workers. A submitted step of an
unregistered type still validates — the dag catalog is definition shape,
not fleet capability — and dead-letters permanent at claim time via the
registry miss, landing visibly in the DLQ. A unit test pins the split as
a partition (core + 2 = Builtins) so a future executor can't silently
land on the wrong side; the registry↔catalog sync test still runs
against the full set and is untouched.

**Tests, mapping to the ACs.** `auth_routes_integration_test.go`
(`TestV1AuthMatrix`) drives every route in the table through the
credential matrix: missing header, non-Bearer scheme, malformed token,
unknown well-shaped key, forged credential (a real key's lookup prefix
with a fabricated suffix — prefix found, hash mismatch), and a revoked
fully-scoped key all collapse to the uniform 401 with the
WWW-Authenticate challenge; a valid key carrying every scope *except*
the route's gets the 403 whose envelope names the missing scope (proving
the check is on the right scope, not "any scope"); the exact scope and
admin-implies-all both succeed; the TTL'd key collapses to 401 on every
route after a clock advance; `/healthz` answers 200 anonymously
throughout. The log-discipline criterion extends 6.1's captured-slog
pattern: request lines and forbidden outcomes carry `key_id` (root's
pseudo-id included), `missing_scope` appears structured, and no minted
plaintext is anywhere in the capture — with the prefix as the positive
control. The 4.6 suite's helpers grew a bearer parameter and its
`newServer` now boots with a runtime root key and mints a submit+read
client key, exercising the bootstrap flow on every test.

**Ripples.** ctl needed no functional change (the shared client has sent
`Authorization: Bearer` on every request since 6.1) — only stale
only-keys-need-auth comments died. `scripts/demo-crash.sh` authenticates
as the stack's root credential, minting an ephemeral sk_-shaped value at
runtime when `.env` has none (constructed, never committed — the CI
secret grep stays clean) and passing it to compose, ctl, and curl.
README documents the bootstrap flow (root key into `.env` → `make
up-app` → `ctl keys create` → `AGENTLOOM_API_KEY`); `.env.example`
gained `AGENTLOOM_API_BIND` and `AGENTLOOM_WORKER_TEST_EXECUTORS`
stanzas and updated root-key/API-key prose; docs/demos/crash-recovery.md
notes the demo needs no key setup. ADR-007 gained the as-built 6.2
section.

**Verified.** Lint, `go test -race ./...`, and the full compose-backed
`-race -tags integration ./...` (crash + chaos suites included, running
real workers with the new env knob) all green.

**Deferred / notes.** `/readyz` and `/metrics` don't exist yet — the
exemption list gains them when M7 wires telemetry. The identity context
accessor is deliberately unexported (6.4 lives in the same package). The
unit route table and the integration matrix's route list are separate
literals by construction (external test package can't see the router);
the walk test catches router↔table drift, and cross-file drift is a
review concern flagged in both files' comments.
