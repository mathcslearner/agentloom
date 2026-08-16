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

### 6.3 — Redis token-bucket limiter (shared library) ✅

**Delivered.** `internal/ratelimit`, the generic atomic token bucket of
ADR-007's rate-limiting design: `New(redis.Cmdable)` wraps an existing
client (the `queue.New` shape), and `Acquire(ctx, Bucket{Key, Capacity,
RefillPerSec}, cost)` runs one Lua script — refill for elapsed time,
grant-or-deny, re-arm TTL, all in one atomic step — returning
`Result{Allowed, Remaining, RetryAfter}`. Deliberately tenant-agnostic:
key naming and cost semantics are the caller's parameters (6.4 keys per
API key and route class with cost 1; M9 keys per provider resource with
token costs). No config changes and no logging in the library — knobs
and deny/429 logging land with the tenants.

**The clock decision (the one real divergence from house convention).**
The script reads Redis `TIME` instead of taking a caller-injected now.
A bucket is *shared* state acquired from many API replicas and workers
with independently skewed clocks — a skewed caller passing its own now
could mint or destroy tokens for everyone sharing the bucket, so the
promoter-style injected-now contract would be actively wrong here. One
Redis = one clock (legal under Redis 7's effect replication). The
injectable-time invariant is honored through a test-only seam: ARGV
carries an optional time override, reachable only via the unexported
`acquireAt` exposed through `export_test.go` — the production `Acquire`
hardcodes the empty override, so no production path can skew a bucket.
Backwards time (a failover clock step, or an injected regression) clamps
elapsed to zero rather than minting tokens; a test pins it.

**State layout.** One hash per bucket key (`tokens`, `ts` in epoch µs —
TIME's native precision, exact in a float64 until ~2255), with **absent
key = full bucket**. That identity composes with the TTL rule: every
acquire re-arms `PEXPIRE` to time-to-full plus a 1s margin, so an idle
bucket expires exactly when its state becomes indistinguishable from no
state — per-key buckets self-clean instead of accumulating forever
(what keeps 6.4's per-key cardinality bounded). A persisted balance is
provably always below capacity (cost ≥ 1 on a grant, balance < cost on
a denial), so the TTL is always positive; a *late* expiry only fires
after the bucket would have refilled anyway, so expiry can never
over-grant. Rate-zero buckets (fixed quotas) `PERSIST` instead —
expiring their state would silently re-arm the quota. Bucket config
(capacity, rate) lives with the caller and rides along on every
acquire: limit changes take effect on the next request, and a capacity
shrink clamps the stored balance down (pinned by a test).

**Two float traps dodged, deliberately.** The balance is serialized
with `string.format('%.17g', …)` — Lua's `tostring` uses `%.14g`, which
silently corrupts the float64 round-trip and would make refill math
drift per acquire. And the script returns the balance as that same
string (not a Redis integer reply, which truncates), so Go parses the
exact float back; `Remaining` is its floor.

**Contract edges for M9.** `cost > capacity` is the typed
`ErrCostExceedsCapacity`, rejected Go-side before Redis — it can never
succeed, and M9 must perm-fail those instead of scheduling a delayed
requeue. A denial from a never-refilling bucket reports the
`RetryAfterNever` sentinel for the same wait-vs-never distinction.
`RetryAfter` is per-cost (ceil of exactly this cost's shortfall), so a
cheaper acquire may succeed sooner than a denied expensive one.

**Tests, mapping to the ACs.** (1) `TestStressNoOverGrant`: 32
goroutines × 100 racing cost-1 acquires against a rate-zero capacity-500
bucket grant *exactly* 500 and deny exactly 2700, under `-race`;
`TestStressVariableCost` re-proves it with mixed costs 1–20 (sum of
grants ≤ capacity, stored balance exactly capacity − granted). (2)
`TestAcquireRefillMathProp` (rapid): random buckets driven through
random op sequences — including zero-rate quotas, backwards and zero
time deltas, costs up to exactly capacity, and long idles hitting the
cap — against a pure-Go model mirroring the script's float64 operations
in the same order, comparing `Allowed`, the exact fractional balance,
`Remaining`, and `RetryAfter` for **exact equality** (Lua numbers are
IEEE-754 doubles; the %.17g round-trip makes bit-exactness a fair
demand — a single ULP of drift fails the property). Green at 2000
iterations. Deterministic `TestRetryAfterExact` pins hand-computed
refill/retry-after points on the injected clock, and doubles as the
denials-don't-consume proof. (3) `BenchmarkAcquire`: **~141µs/op
sequential, ~44µs/op at GOMAXPROCS parallelism** (Apple M2, dockerized
local Redis, ~19 allocs/op) — ≈7× under the 1ms local target. Unit
tests cover parameter validation (nil-client limiter proves rejection
precedes any Redis call) and every malformed script-reply shape.
Integration tests follow the queuetest isolation discipline (unique
`agentloom-test:` key prefixes, `AGENTLOOM_TEST_REDIS_ADDR`, deleted on
cleanup) via a local ~25-line helper — importing the queue harness for
a string constant and a client opener was the only reuse worth taking.

**Verified.** Lint, `go test -race ./...`, and the full compose-backed
`-race -tags integration ./...` all green.

**Deferred / notes.** No `Reset`-style introspection (time-to-full for
6.4's `X-RateLimit-Reset`) — 6.4 can derive it from `Remaining` and its
own configured rate, or grow the script's reply by one field if headers
want server-computed truth; decide there. Multi-bucket acquire (global
+ per-key in one round trip) also waits for 6.4 — if the two sequential
acquires measurably matter, the honest fix is a second Lua script taking
two keys, not client-side pipelining of a check-then-take. M7 metrics
hooks (grant/deny counters, acquire latency) stubbed per roadmap.

### 6.4 — Per-client API rate limiting middleware ✅

**Delivered.** Ticket 6.4's enforcement layer over 6.3's limiter: a
`rateLimit(class)` middleware in `internal/api/ratelimit.go`, mounted
after `requireScope` on every `/v1` route. Each request acquires cost 1
from the caller's per-`key_id` bucket for the route's class
(`<prefix>:<key_id>:<class>`; the root credential rides under `"root"`),
then from the API-wide global safety bucket (`<prefix>:global`). Either
denial is a **429** carrying `Retry-After` plus the envelope's new
contract code `rate_limited`; `X-RateLimit-Limit`/`-Remaining`/`-Reset`
go on every limited response, allowed or denied. Route→class mirrors
6.2's route→scope discipline exactly — the table lives beside a
chi.Walk coverage test (`ratelimit_routes_test.go`), so a new `/v1`
route cannot ship unclassified; `/healthz` and the 404/405 fallbacks
stay exempt for the same reasons they are exempt from auth. Because the
middleware sits after auth, credential failures consume no tokens — a
bad-key storm cannot starve anyone's bucket (pinned by test).

**Decisions (ADR-007 gained the 6.4 as-built section).**

- **Fail-open on Redis errors.** An `Acquire` failure logs at Error,
  fires the metrics hook, and lets the request through. Rate limits are
  protective, not correctness: Postgres stays the API's only hard
  dependency. Consequently `cmd/api`'s new Redis client — the first
  ever in the API deployable — is scoped to rate-limit buckets alone
  (ADR-002's no-dispatch rule untouched), opens without a boot-time
  dependency (go-redis dials lazily; the boot ping is advisory and only
  warns), and `/healthz` never touches Redis.
- **Per-key before global, sequentially.** An exhausted caller is
  rejected *without* touching the global bucket, so one abusive
  client's 429 storm cannot drain the fleet's shared budget — the
  stronger property. Accepted cost, documented rather than compensated:
  a global denial does not refund the per-key token already spent.
  6.3's deferred two-key atomic script stays deferred.
- **Headers always describe the caller's own class bucket** (stable
  meaning for clients), while `Retry-After` comes from whichever bucket
  denied, whole seconds rounded up so an honoring client never retries
  early. `X-RateLimit-Reset` takes the cheap branch of 6.3's deferred
  decision: derived as ceil((capacity − remaining)/refill) seconds to
  full — ≤ 1 token of imprecision, library reply untouched.
- **Refill must be strictly positive** in API config: a rate-zero API
  bucket would permanently brick a key (`RetryAfterNever`), which is
  never a sane API limit — fixed quotas remain an M9 shape. Validated
  at config parse *and* in `api.New`, so a bad limit fails boot instead
  of failing open per request.
- **Metrics stubbed for M7** as the `RateLimitMetrics` seam (per-bucket
  decisions + fail-open events, no-op default) — the "429 counters
  (hooks from 6.4)" the M7 roadmap names.

**Config & wiring.** `config.APIRateLimitConfig` on `APIConfig`:
`AGENTLOOM_API_RATELIMIT_ENABLED` (default true; false skips the Redis
client entirely), `_KEY_PREFIX` (default `ratelimit:api`; the per-test
isolation knob, same discipline as the queue key knobs), and
`_{SUBMIT,READ,ADMIN,GLOBAL}_{CAPACITY,REFILL_PER_SEC}` with dev-sane
defaults (submit 20/10, read 100/50, admin 10/2, global 500/250 —
submits stricter than reads, admin strictest, per ADR-007). Two new
config helpers (`applyPositiveInt64`, `applyPositiveFloat`). `api.New`
grew a `RateLimitOptions` parameter (zero value = disabled — the
existing tests' mode) with an `Acquirer` seam over `*ratelimit.Limiter`
so unit tests script grant/deny/error without Redis. Compose's api
service now gets `AGENTLOOM_REDIS_ADDR: redis:6379` and depends on
redis healthy.

**Tests.** Unit (fake acquirer): route→class coverage, threshold
exactness with the full header sequence, global-deny semantics,
fail-open, disabled mode, no-consumption on 401, bucket-key naming +
class isolation, metrics hook ordering, options validation, and the
header math (`resetSeconds`/`retryAfterSeconds`) pinned pointwise.
Integration (real limiter + store, per-test unique bucket prefixes
scan-deleted on cleanup): the headline AC — a read-scoped key driven
through a capacity-4 burst with exact `Remaining` countdown, 429
exactly at the threshold, recovery inside a bounded poll (real-time
refills are deliberate: the limiter's clock is Redis's, and its
fake-time seam is sealed inside the ratelimit package by design);
global-bucket protection with two keys each far under their own limits
(after a full-capacity refill wait, since setup traffic shares the
global bucket); per-key isolation; root-key limiting on the admin
class; credential-failure storms consuming nothing; and the submit
class throttling real run submissions end to end with reads unaffected.
Deny-log discipline asserted (429 line present, plaintext key absent).

**Verified.** Lint, `go test -race ./...`, and compose-backed
`-race -tags integration` for api/ratelimit/cmd-api all green.

**Deferred / notes.** The two sequential acquires (per-key, global) are
two Redis round trips per request (~90µs at parallelism) — fine at v1
traffic; if it ever matters the honest fix stays 6.3's two-key Lua
script. `X-RateLimit-Reset` server-computed truth (growing the script
reply) also waits for a real need. ctl gained no 429 backoff handling —
its poll cadence sits far under the read defaults; revisit with 6.5's
list endpoints if watch traffic grows.

### 6.5 — Run & definition lifecycle endpoints ✅

**Delivered.** The full lifecycle API: the definition registry
(`POST /v1/definitions`, `POST /v1/definitions/{name}/versions`,
`GET /v1/definitions` latest-per-name keyset listing,
`GET /v1/definitions/{id}` with the stored spec,
`GET /v1/definitions/{name}/versions`), the run list
(`GET /v1/runs` — keyset pagination + status/definition/time-range
filters), the four run-control endpoints (`POST /v1/runs/{id}/cancel`,
`…/park`, `…/unpark`, `…/steps/{sid}/requeue`) over the 5.4/5.6 engine
ops, and both post-M4-audit idempotency hardenings — the token now rides
the `Idempotency-Key` header, is length-bounded (400 instead of the old
btree-limit 500), and is fingerprinted to its payload so a mismatched
replay 409s instead of silently returning the original run. Scopes and
rate-limit classes land exactly on ADR-007's pre-assigned rows
(lifecycle + definition-create = submit, listings = read), enforced by
the existing walk-based coverage tests.

**Engine: `Control` extraction.** Cancel/Park/Unpark/Requeue moved onto
`engine.Control` (store + clock + optional nudge — their entire
dependency footprint); `Engine` embeds `*Control` so every existing call
site is untouched, and `engine.NewControl` gives the API a control
surface without an executor registry or effects journal. `api.New`
builds it internally over the same store/clock with a **nil nudge** —
deliberate: the ops' outbox rows (`unpark`, `dlq_requeue`) are drained
on the worker fleet's dispatch cadence, so ADR-002's "the API never
talks to Redis for dispatch" holds. Requeue's cancelled-run refusal
became the typed `engine.ErrRunNotRequeueable` so the handler can map it
to 409 (it was a bare `fmt.Errorf` that would have 500'd).

**Store.** Migration 0009: `runs.idempotency_fingerprint` (nullable
TEXT) plus the run-list keyset indexes — `runs (created_at DESC, id
DESC)` and partial `runs (definition_id, created_at DESC, id DESC)`.
`CreateRun` computes the fingerprint (hex SHA-256 over the canonical
snapshot, canonicalized params — key order and formatting normalized —
and the definition ref or an inline marker), stores it beside the token,
and both reuse paths (pre-check and lost-race conflict catch) compare it,
returning the new `*store.IdempotencyMismatchError` (unwraps to
`ErrConflict`) on divergence; NULL fingerprints (pre-0009 rows) are
grandfathered as unchecked reuse. `MaxIdempotencyTokenLength` (200) is
enforced at both layers. Registry ops: `Store.CreateDefinition`
(canonical `dag.Encode` spec at version 1; existing name surfaces the
`(name, version)` `*ConflictError`) and `Store.CreateDefinitionVersion`
(one transaction: per-name `pg_advisory_xact_lock` via the new
`DefinitionRepo.LockName`, then `NextVersion` MAX+1, then insert —
serialized allocation, so concurrent appenders get consecutive versions
with no retry loop; unseen name → `ErrNotFound`). New reads:
`ListRunsPage` (nullable status/definition/created-range filters, cursor
as a row-value comparison against the uniformly-descending order) and
`ListDefinitionsLatest` (`DISTINCT ON (name)`, keyset by name).

**API.** New envelope codes `conflict`, `idempotency_key_conflict`,
`step_not_found` (all contract). Lifecycle handlers share
`writeRunOpError` (TransitionError → 409 with the entity's actual
status in the message, ErrNotFound → 404, requeue disambiguates
run-vs-step 404 with one extra read). Cursors are opaque base64url
(JSON `{t, id}` for runs, the bare name for definitions); a garbage
cursor is a 400. `GET /v1/runs/{id}` now returns the run's
`dead_letters` (how a client discovers requeueable steps — payload
deliberately omitted from the wire) and `RunView` gained the 5.x
columns (`definition_id`, `on_failure`, `steps_cancelled`,
`park_reason`, `cancel_reason`, `deadline_at`). The submit body's
`idempotency_token` field is **gone** (pre-1.0 break, ctl moved in the
same change; 6.6 pins the contract). ctl grew `runs` (tabwriter page +
next-cursor hint on stderr), `cancel`, `park`, `unpark`, `requeue`, and
`--token` now sends the header.

**Tests.** Unit: cursor round-trips/garbage, both route tables extended
(the walk tests forced it), ctl command fakes including the
header-not-body submit contract. Store integration: fingerprint
semantics (reorder-tolerant, mismatch typed with the original run id,
ref-vs-inline distinct, legacy NULL grandfathered), token length bound,
registry semantics (canonical round-trip, conflict, unseen-name), and
8-way concurrent version allocation proving consecutive gap-free
versions. API integration: definitions round-trip + validation/conflict
table + latest-per-name pagination; run-list filters, parameter
validation table, and the acceptance keyset walk (12 seeded runs, page
size 4, 3 inserts between every page — every seeded run exactly once,
no duplicates); idempotency contract (400/200-reused/409 ×
params/definition/ref-form, legacy reuse); lifecycle contract (idle
cancel finalizes in the request tx, double-cancel 409, park/unpark
cycle with both 409s, miss table incl. run-vs-step 404 split, and the
requeue-on-cancelled-run 409 via a poison-manufactured dead letter);
auth matrix rows for all ten new routes (stateful probes mint fresh
runs per success invocation; the requeue probe's expected "success" is
the handler's 409 — auth passed). The e2e suite (production dispatcher
+ 2 workers + retry scheduler + 50ms promoter/cancel-poll): DLQ requeue
→ budget re-armed → succeeded on attempt 3 with `steps_failed` 0;
in-flight 30s sleep cancelled via the API with attempt history exactly
`[cancelled]`; park strands the successor (delivery consumed, 0
attempts), unpark re-dispatches exactly it, run succeeds. Full
`-race -tags integration ./...` (crash + chaos suites included) and
lint green.

**Non-obvious decisions / deferred.**
- Version allocation uses a per-name advisory lock instead of a
  retry-on-conflict loop: MAX+1 cannot lock rows that don't exist, and
  bounded retries flake under real contention. Cross-name hash
  collisions merely serialize two unrelated appends.
- `ListRuns`' order flipped to uniform `(created_at DESC, id DESC)` so
  one row-value predicate is exactly "strictly after the cursor";
  keyset stability under concurrent inserts then holds by construction
  (new rows sort before any issued cursor position).
- Definition create is not upsert: an existing name is a 409 pointing
  at the versions route — accidental fork vs deliberate version stays
  explicit.
- Idempotency tokens stay global (not per-key) — recorded in ADR-007;
  scoping later is an additive column, not a contract break.
- Deferred: ctl commands for the definitions registry (the API is the
  contract surface; ctl can grow `defs` alongside 6.6's docs), and 429
  backoff in ctl (unchanged from 6.4's note).

### 6.6 — OpenAPI contract & docs ✅

**What shipped.** The API contract, pinned. `api/openapi.yaml` is the
hand-maintained OpenAPI **3.1** spec covering all 16 `/v1` routes plus
`/healthz`: every operation with its scope + rate-limit class in the
description, request/response schemas mirroring `internal/api/types.go`
component-for-component, the `bearerAuth` security scheme (global, with
`security: []` on the health probe), the error envelope with the full
`ErrorCode` enum (renaming one is a contract break), the
`Idempotency-Key` header parameter (maxLength 200), reusable 4xx/5xx
responses carrying the `WWW-Authenticate` / `Retry-After` /
`X-RateLimit-*` headers, and examples on the operations that matter
(inline + by-ref submission validated against the real definition
schema, error envelope with path-qualified issues).

3.1 over 3.0 was the load-bearing choice: OpenAPI 3.1's schema dialect
*is* JSON Schema 2020-12, which is exactly what `internal/dag/gen`
emits — so the workflow-definition schema is a single external
`$ref: '../docs/schema/workflow-definition.v1.json'` (whose root
already points at `#/$defs/Definition`) instead of a hand-converted
inline copy that would drift. `make generate` keeps the referenced file
current with no extra step, and openapi-typescript (M17's client
generator) handles both 3.1 and local file refs.

Closed vocabularies became enums (run/step statuses incl. the legacy
`failed` resting state, attempt outcomes incl. `lost`, DLQ sources,
edge types/resolutions, scopes, park/cancel reasons, `on_failure`).
Request bodies are `additionalProperties: false` — matching the
handlers' `DisallowUnknownFields` — while responses stay open so
additive fields aren't a validator break. `params`/`output`/`error`
documents are deliberately typeless ("any JSON value" is the contract).
Spec path params are wire-style snake_case (`{run_id}`); chi's
camelCase names stay internal.

**Enforcement, two layers.** (1) `make openapi-lint` runs vacuum
(Go-native, version-pinned via `go run` like sqlc — no Node in CI) with
`--fail-severity warn`; `api/vacuum.ruleset.yaml` disables exactly
three recommended rules, each justified in-file (snake_case *is* the
contract; typeless = any-JSON is deliberate; per-property examples are
noise) — with everything else on, the spec lints 100/100, and the CI
lint job gained the step. (2) `TestOpenAPIRouteCoverage`
(`internal/api/openapi_test.go`) parses the spec's `paths` as plain
YAML (`go.yaml.in/yaml/v3`, promoted to a direct dep; a full OpenAPI
resolver would be a second source of truth — validity is vacuum's job)
and compares against `chi.Walk` **both directions** with param names
normalized, so a mounted-but-undocumented route and a
documented-but-unmounted path both fail; being a plain unit test it
rides the existing CI test job. `TestOpenAPIOperationContracts` pins
the conventions: operationId everywhere (M17's method names), a
schema'd 2xx on every operation (204 exempt), and 401/403/429
documented on every `/v1` operation.

**Docs.** `docs/api.md` — curl walkthroughs runnable against
`make up-app`: root-key auth bootstrap → mint scoped keys →
submit-with-`Idempotency-Key` (replay + 409 semantics) → run
inspection → keyset list walk with filters → definition registry
(create / version / submit-by-ref) → cancel/park/unpark → DLQ discovery
+ requeue → error envelope + rate-limit-header reading. Indexed in
`docs/README.md`; the stale "until the OpenAPI spec takes over"
comments in `api.go`/`types.go` now point at the spec as the live
contract.

**Verified.** `go test -race ./...`, `make lint`, `make openapi-lint`
all green; the drift test proven to fire in both directions by
mutation; the CI `sk_` grep stays clean (doc examples elide key
material).

**Non-obvious decisions / deferred.**
- Field-level schema drift (a `types.go` field vs its component schema)
  is *not* machine-checked — only route-level drift is. The honest
  fix is generating schemas from the Go types or contract tests
  against recorded responses; deferred until it hurts, recorded in the
  `types.go` comment ("change both together").
- vacuum's external-ref resolution handled the relative `$ref` without
  a base-path flag; if a future vacuum bump breaks it, `-b` is the
  knob.
- OpenAPI can't express custom scopes on an `http: bearer` scheme, so
  the route→scope table lives in operation descriptions (ADR-007
  remains the authority; the walk tests enforce it in code).
- ctl `defs` subcommands (deferred at 6.5 "alongside 6.6's docs")
  stay deferred — docs/api.md documents the raw routes; ctl growth is
  cosmetic and can ride any later ticket.

## Milestone 7 — Observability

### 7.1 — ADR-008 & telemetry wiring ✅

**Delivered.** [ADR-008](adr/008-observability-conventions.md)
(observability conventions — the contract 7.2–7.5 implement against),
`config.ObsConfig` (`AGENTLOOM_OBS_*`, everything off by default),
`internal/obs/metrics` (instance-scoped Prometheus registries + the
admin HTTP listener), `internal/obs/trace` (OTel SDK setup, OTLP/gRPC
export, no-op provider when disabled), telemetry wiring in both
deployables, and the compose `obs` profile (Prometheus, Grafana,
Jaeger) behind `make up-obs`.

**The ADR's decisions**, in brief: one metric namespace
`engine_<subsystem>_<name>[_<unit>]` on instance-scoped registries
(never the global default registry); an enumerated label-allowlist
table making "never `run_id`/`step_id` as labels" a diffable checklist
(`step_type`, `outcome`, `status`, `reason`, `class`, `source`,
`route`-as-chi-pattern, `method`, `code`, `duty`, `service`,
`version` — each with its bound); the log field dictionary extended
with `span_id` and `service` (constants + typed helpers in
`internal/obs/log`); and the trace propagation design 7.3 implements —
W3C tracecontext + baggage, root span at submission, **trace context
persisted on the run row** (7.3's migration) so reconciler heals /
requeue / unpark restore linkage instead of starting orphan traces,
attempt spans linked (not parented) across retries/reclaims, envelope
`traceparent`/`tracestate` population additive within version 1.

**Wiring.** `metrics.NewRegistry(service)` preloads the Go runtime +
process collectors and `engine_build_info{service, version} = 1` (the
proof-of-life gauge — every scrape proves the pipeline before 7.2
lands). `metrics.Listen`/`Serve` is the admin listener serving
`GET /metrics` + `GET /healthz`: bind failure is a boot error (fail
fast on misconfiguration), serve-time failures only log — telemetry
never takes the deployable down. It is deliberately not on the API's
public port (bearer-authed, route-coverage/OpenAPI-drift tested;
Prometheus doesn't present bearer keys) and is the worker's first
listener at all; in the worker it rides `loopCtx` so `/metrics` stays
scrapeable through the consumer's drain, in the API it outlives the
signal context the same way. `trace.Setup` installs the global
propagators either way; disabled it installs the no-op provider and
returns a free shutdown, enabled it builds OTLP/gRPC exporter → batch
processor → `ParentBased(TraceIDRatioBased)` sampler with
`service.name`/`service.version`/`service.instance.id` resources
(worker: consumer name; api: hostname), SDK errors routed to slog at
warn. The API handler is wrapped in `otelhttp` server spans when
enabled — named `HTTP <method>` only (the chi route pattern isn't
known that far out; 7.3 refines) — which is what makes "Jaeger
receives spans" true in this ticket.

**Compose.** Profile `obs`: Prometheus (5s scrape; static target
`api:9090`, `dns_sd_configs` type-A on `worker` — compose DNS returns
every replica IP, the one discovery mechanism that survives
`deploy.replicas: 2` without a docker-socket mount), Grafana
(datasource provisioned as code; dashboards are 7.5), Jaeger
all-in-one with `COLLECTOR_OTLP_ENABLED` (no collector hop; inserting
one later is a compose change). App services set
`AGENTLOOM_OBS_METRICS_ADDR=:9090` unconditionally (in-network only,
never published — also sidesteps the replica host-port collision);
OTel export stays `false` under plain `make up-app` (Jaeger absent)
and `make up-obs` boots both profiles with
`AGENTLOOM_OBS_OTEL_ENABLED=true`. `make down`/`nuke` cover the new
profile; `.env.example` documents the knobs.

**Verified.** `make lint` / `make test` / `make test-integration`
green. Unit: config parse matrix, registry-through-handler scrape,
admin bind/serve/shutdown lifecycle, noop-vs-recording provider both
ways, enabled Setup proven non-blocking against a dead endpoint with
bounded shutdown. Integration: both deployables' lifecycle tests boot
with the admin listener on `:0`, recover the real port from the boot
log line, and scrape `engine_build_info` with the right `service`
label. Live acceptance on `make up-obs`: Prometheus `/targets` shows
1 api + 2 worker targets up; fanout.json submitted through the API
lands `HTTP POST`/`HTTP GET` spans in Jaeger under `agentloom-api`.

**Non-obvious decisions / deferred.**
- Telemetry defaults *off* (empty metrics addr, OTel disabled) so bare
  `go test`, the harnesses, and the crash suite's subprocess workers
  bind no ports and dial nothing — the "cleanly disabled" AC is the
  default, not a mode.
- The semconv package version must match `resource.Default()`'s schema
  URL for the pinned otel SDK release or `resource.Merge` errors at
  Setup — noted in code; revisit on every otel bump.
- API span names are method-only in 7.1: otelhttp names spans before
  chi routes, so route patterns aren't available; 7.3 owns proper span
  naming (and the submission root span + queue propagation).
- The `RateLimitMetrics` seam (6.4) stays no-op; it is 7.2's "API
  request histograms + 429 counters" work, on this registry.
- Worker replica scrape relies on compose DNS A-record behavior; if it
  ever proves flaky the fallback is two named worker services (noted
  in the plan, not needed so far).

### 7.2 — Core engine metrics ✅

**Delivered.** The full 7.2 instrument set on the 7.1 registries:
queue ready depth / stream length / PEL size / delayed count, outbox
backlog + oldest age, per-row dispatch (drain) lag, reconcile-heal
counters, claim decisions, step duration by type + outcome, the
ready→running scheduling-latency histogram, retries by class,
takeovers, fencing rejections, the DLQ counter by source, run
completion latency by terminal status, the active-workers gauge, and
the API request histogram/counter plus the 6.4 `RateLimitMetrics`
seam's Prometheus implementation. Plus the `make smoke-metrics`
acceptance script and ADR-008's as-built inventory + label-allowlist
amendments.

**Shape.** Every instrument is declared in one file —
`internal/obs/metrics/instruments.go` (`NewWorkerMetrics` /
`NewAPIMetrics`) — so the cardinality audit is a single-file read;
the producing packages stay free of any prometheus dependency by
declaring narrow seams the instrument structs satisfy structurally:
`queue.ConsumerMetrics` (a `ConsumerConfig` field: reclaims, poison
diversions, promotions + `PromoteResult.MaxLag`, the hook 3.5 left
for exactly this), `engine.Metrics` (`WithMetrics` on the Engine,
`WithDispatcherMetrics`, `WithReconcilerMetrics`), and
`api.RequestMetrics` (a new variadic `api.Option` on `api.New`).
Defaults everywhere are no-op recorders — every existing test layer,
harness, and crash-suite subprocess keeps running with recording off,
byte-for-byte.

**Scheduling latency (the AC proof).** `store.ClaimStepWithOrigin`
extends the claim with one extra primary-key read *under the run lock
the transition already takes*, returning the pre-claim status and
`updated_at` (`ClaimOrigin`); `ClaimStep` stays a thin wrapper with
its contract untouched. The engine observes claim-time −
ready-`updated_at` — both injected clocks — and only when the origin
status was `ready`: a retrying→running claim's interval includes the
deliberate backoff and is excluded by design (asserted in the test).
The integration proof scripts a 7s delay between instantiation clock
and claim clock and asserts the histogram holds exactly one sample of
exactly 7.0s — no tolerance.

**Run completion latency** is terminal-transition time minus the
run's `started_at` (the injected instantiation clock), not
`created_at` — that column is a DB `now()` default, and mixing it
with the app clock produced garbage under fixed-clock tests (caught
by the first integration run). The rollup helpers
(`attemptRunRollup`, `attemptCancelRollup`) now return the terminal
run row so completion transactions, the poison handler, and the
cancel-settle path can all observe it; API-side cancel finalizations
(a cancel request that finds nothing in flight) go unrecorded — a
documented gap, workers record everything else.

**Gauges** are sampled by a new `cmd/worker` loop every
`AGENTLOOM_WORKER_METRICS_SAMPLE_INTERVAL` (default 10s, under the
15s scrape convention), running only when the admin listener is
configured; each source (queue stats, delayed ZCARD, outbox stats,
XINFO CONSUMERS) samples independently so one backend outage doesn't
blank the rest. Ready depth is the group lag from XINFO GROUPS —
`StreamStats` grew `Lag` plus the `ReadyDepth()` accessor that falls
back to `XLEN − PEL` when Redis reports the lag unknowable (go-redis
surfaces that as -1); active workers = consumers with idle ≤ 3× the
read block, via the new `Queue.ActiveConsumers`. The outbox side is
the new `OutboxRepo.Stats` (one aggregate query; COALESCE keeps sqlc
non-nullable, the repo maps the empty-table sentinel back to nil).

**API metrics** record in `requestLog` — it already measured status
and duration — using `chi.RouteContext(...).RoutePattern()` after
routing as the bounded `route` label; 404/405 fallbacks collapse to
`route="unmatched"` and unrecognized client verbs clamp to
`method="other"` so clients can never mint label values. The
duration histogram deliberately drops `code` (route×method×code×
buckets would blow the ~1,000-series budget); 429s live on the
counter. `cmd/api` wires `APIMetrics` into both the request option
and `RateLimitOptions.Metrics`, retiring the 6.4 no-op stub.

**Cardinality audit.** ADR-008's allowlist gained `result` (4),
`bucket` (2), `decision` (2) plus the unmatched/other clamp notes,
and an as-built inventory table of all 26 metrics. The audit is
executable: `TestInstrumentConformance` exercises every instrument on
a fresh registry and fails any metric outside the `engine_` namespace
or subsystem vocabulary, any counter without `_total`, any histogram
without `_seconds`, and any label key not in the allowlist. Interface
conformance is compile-time-asserted in the same test file.

**Verified.** `make lint` / `make test` / full
`make test-integration` green. New integration tests: the scripted-
delay scheduling proof (plus duplicate-delivery ack_drop leaving the
histogram untouched), retry-exhaustion driving retries/DLQ/run-failed
counters with exact 9s run latency and the retrying-claim exclusion,
dispatcher drain-lag observation, the reconcile-heal counter off a
provoked lost dispatch, and the API request + rate-limit counters
through the real router (429/401/404/201 label matrix). Live
acceptance: `make smoke-metrics` (fanouts + retry + dead-letter
against compose) — all 28 checks green, `engine_worker_active` = 2.

**Non-obvious decisions / deferred.**
- Instruments always exist on both deployables (recording on an
  unexposed registry is cheap); only the *sampler* is gated on the
  admin listener being configured.
- Step-duration outcomes derive from the executor invocation
  (`executionOutcome`, pure) rather than the durable attempt row —
  the rare divergence (e.g. success racing a run-cancel) was not
  worth threading transaction results back out.
- Dispatch lag compares the app clock against the row's DB-default
  `created_at`; ordinary clock skew accepted for a gauge-grade
  number (noted in code and ADR).
- `engine_reconcile_healed_total` (and other label-vec series) have
  no series until their first event; the smoke script asserts
  presence only for plain counters and movement where the workload
  guarantees it — crash-only paths are the chaos suite's business.
- Reconciler cancel-heals count toward `takeovers_total` but write
  no heal reason (they create no outbox row; the reason vocabulary
  is outbox reasons by construction).
- The queue seam records poison diversion after the handler consumed
  the message even if the XACK itself fails (the DLQ row is the
  durable consumption; the redelivered entry ack-drops).

### 7.3 — Distributed tracing across the queue ✅

**Delivered.** ADR-008's trace design realized end to end: W3C trace
context injected into task envelopes at enqueue with the run rooted at
the `POST /v1/runs` server span, workers starting a `step.attempt` span
per delivery with `step.claim` / `step.executor` / `step.completion` /
`queue.ack` children, retries and reclaim/takeovers linked (never
parented) to the attempt they re-execute, fan-in joins linking every
firing parent, `trace_id`/`span_id` stamped into the structured logs on
both deployables, and the API's otelhttp span renamed to
`<METHOD> <route pattern>` after routing (closing 7.1's placeholder).
Verified live by the new `make smoke-trace`: one Jaeger trace for a
retrying fan-out run, spans from both compose worker replicas, and the
retry rendered as a FOLLOWS_FROM reference.

**Shape.** Migration 0010 gives trace context three durable homes, all
TEXT nullable (NULL = no context — tracing off and pre-0010 rows behave
identically):

- `runs.trace_parent`/`trace_state` — the durable root, captured by the
  submit handler from the request span via `obstrace.Inject` and passed
  through `CreateRunArgs.Trace`.
- `task_outbox.trace_parent`/`trace_state` — the *enqueuing span's*
  context. Only writers inside a live span stamp it: the completion
  transaction's fan-out rows (`Outbox().CreateTraced` with the
  `step.completion` span, so a successor's attempt parents under the
  span that readied it) and instantiation's entry rows (the submission
  span). Every other writer — reconciler heals, unpark, dlq_requeue —
  still calls plain `Create` and was not touched: the drain read
  (`ListOutboxTasksForDrain`, now a join to `runs` with
  `FOR UPDATE OF t` so run rows are never locked) COALESCEs NULL to the
  run root, which is exactly ADR-008's "healed dispatches restore
  linkage from the run row".
- `run_steps.trace_span` — the current attempt's span context, stamped
  by the claim CAS (`ClaimStepArgs.TraceSpan`). The pre-claim read that
  7.2 added for scheduling latency now also surfaces the value being
  overwritten (`ClaimOrigin.PrevTraceSpan`), and that previous value is
  the single uniform link source: a due retry sees the failed attempt's
  span, a takeover (worker or reconciler-healed) sees the lost
  holder's. No link fields in the envelope, nothing handed over by the
  dead worker, and links survive crash-healed re-dispatch paths for
  free.

The attempt span lives in the queue consumer, not the engine — the ACK
must be a child of it and the ACK is the consumer's (`process` extracts
the envelope context, starts the span, and closes it after the ack
decision; the detached-context ack keeps its child span because trace
context is value-only and survives `WithoutCancel`). The engine
enriches the same span via context: `attempt`/`step_type` attributes,
the prev-attempt link, and — gated on `step_type = join` — links to all
firing parents from the new `Steps().ListFiringParentTraceSpans`. The
delayed retry envelope carries the *run root* (from `ClaimOrigin.RunTrace`,
threaded through `completeFailure` → `scheduleRetry`): it must not
parent to the failed attempt, and being constant per run it keeps
identical retries encoding to byte-identical delayed members — ADR-005's
ZADD move-the-fire-time dedup is undisturbed.

Tracers are injected: `queue.ConsumerConfig.TracerProvider` and
`engine.WithTracerProvider`, defaulting to the global provider (the
no-op unless `obs/trace.Setup` enabled export) — every existing test
layer runs span-free with zero changes. `internal/obs/trace` gained the
string-typed bridge (`Inject`/`Extract`/`SpanContext`/
`LinkFromTraceparent`/`WithLogContext`) on an *explicit* W3C propagator
instance, because the global propagator is a silent no-op until Setup
runs and the helpers must round-trip in tests that never install the
pipeline.

**Tests.** Unit: propagation-helper round-trips including malformed
traceparent tolerance (a broken stored context skips the link, never
fails execution). Hermetic integration (in-memory `tracetest` recorder
through the seams, no collector): the headline
`TestTraceSingleRunWithRetryLink` — every span of a submit→fail→retry→
succeed→successor run shares the root's trace id, the entry attempt
parents to the submission span, the retry attempt parents to the run
root and *links* to the failed attempt, the successor parents to the
completion span that readied it, all four child spans present, and the
captured slog output carries the run's `trace_id`/`span_id`;
`TestTraceJoinFanInLinks` (join attempt links both parents' attempts);
`TestTraceTakeoverLink` (a dead holder's claim stamped with a fabricated
span, reclaim + takeover, the new attempt links the lost span with
history `[lost, succeeded]`). Live: `make smoke-trace` green —
one trace, 2 worker processes, FOLLOWS_FROM retry link, plus a direct
Jaeger trace URL in the output.

**Non-obvious decisions / deferred.**
- `run_steps.trace_span` records the *attempt* span, captured before
  the `step.claim` child span starts — stamping from inside the claim
  transaction's context would point every later link at the claim
  child instead of the attempt (caught by the integration tests on
  first run).
- Envelope schema: zero decode changes — ADR-005 reserved
  `traceparent`/`tracestate` in version 1, so populating them is the
  additive evolution the acceptance criterion asks for; old envelopes
  simply start root spans.
- The join-parents query runs only for `step_type = join` and only
  when a valid span context is active, so the untraced hot path gains
  no reads.
- Poison diversions and undecodable envelopes get no attempt span
  (they never reach `process`'s post-decode section); the DLQ row and
  logs remain their observability. Fine to revisit if M7.5 dashboards
  want it.
- The API-side Control ops (cancel/park/unpark/requeue) write outbox
  rows with no live-span stamping (plain `Create`); their dispatches
  parent to the run root via the drain fallback — semantically right
  (they descend from no execution span) and free.
- `slog` capture in the headline test swaps the process-global default
  logger (the spawned consumer's context carries none), so that one
  test is deliberately not `t.Parallel()`.

### 7.4 — Per-step log capture & API ✅

**Delivered.** Durable per-step log capture end to end: migration 0011's
`step_logs` table (one size-capped ring per attempt), the
`internal/exec/steplog` capture package (slog tee + bounded drop-oldest
queue + async flusher), the `engine.WithStepLogs` seam stamping the tee
onto `StepContext.Logger` per attempt, `AGENTLOOM_WORKER_STEPLOG_*`
config (on by default, capture level info, cap 1000 lines/attempt),
cmd/worker lifecycle wiring, three new `engine_steplog_*` counters
under the new ADR-008 subsystem `steplog`, and
`GET /v1/runs/{id}/steps/{sid}/logs?attempt=&level=&cursor=&limit=`
(read scope/class, spec'd in `api/openapi.yaml` at lint 100/100).
Verified live on compose: a `fail_n_times` run's attempt-1 failure line
served with fields via curl.

**Non-obvious decisions.**

- **Per-attempt rings need no seq coordination.** Retries, reclaims,
  and takeovers all mint a new attempt number at claim (ADR-004), so an
  attempt's log stream has exactly one writer and `seq` is an atomic
  counter on the attempt's capture handler — shared by `With`/
  `WithGroup` derivatives, allocated only for records that pass the
  capture level. A displaced zombie's late flush lands under its old
  attempt number: harmless, preserved diagnostics.
- **The truncation marker is derived, never stored.** Both loss
  mechanisms (queue overflow pre-storage, ring-cap eviction
  post-storage) drop oldest, and every captured line consumes a seq
  either way — so `dropped_lines = max(seq) − count(*)` from one
  aggregate read, and seq gaps mark losses line-by-line. No marker row
  to keep transactionally consistent with the data it describes.
- **"Flooding cannot stall execution" is structural, not tuned.** The
  capture path is one mutex-guarded ring-buffer append — no DB call, no
  channel send that can block; a slow/wedged Postgres fills the queue
  and drops oldest while the executor runs on. Failed flushes drop
  their batch (log + counter) instead of retrying: a poisoned group (FK
  gone after run deletion, seq conflict from a protocol bug) must not
  wedge the flusher. The 10k-line flood test pins completion time,
  stored ≤ cap, and the marker.
- **Only the executor's logger is teed.** The engine's own pipeline
  lines (claim decisions, takeovers, completion logging) are worker
  diagnostics, not step logs; `execute()` builds `execLogger` for the
  `StepContext` alone and keeps the plain logger for itself. The base
  handler keeps its own level policy — a debug line can reach stdout
  while the durable side captures info+ (or vice versa).
- **Fields JSONB carries call-site attrs only.** The canonical
  correlation fields (run_id/step_id/attempt/trace_id) are columns; the
  capture handler starts attr-empty (the fanout hands the accumulated
  context attrs to the terminal side only), so nothing is stored twice.
  Errors stringify (`errors.New(...)` marshals uselessly as `{}`);
  unmarshalable values fall back to `fmt.Sprint`; message and fields
  each truncate at `MAX_LINE_BYTES` with explicit markers.
- **`attempt` beyond the step's history answers an empty page, not a
  404** — "no such attempt" and "attempt that logged nothing" are
  indistinguishable by design (empty rings write no rows), so the
  endpoint doesn't pretend otherwise. Never-attempted steps report
  `attempt: 0` with no lines. Missing run vs missing step keeps the
  requeue endpoint's two-code 404 shape.
- **Level vocabulary is closed at the schema** (`debug|info|warn|error`
  CHECK): capture canonicalizes arbitrary slog levels into the four
  bands, the API's `level=` is a minimum-severity filter implemented as
  a `level = ANY(suffix)` array (index-friendlier than rank CASE).
- **Logs are poll-based in v1** (ROADMAP's recorded non-goal): follow
  mode = polling the keyset cursor. The response shape (ascending seq,
  opaque cursor) is chosen so M18's log view can tail by cursor.

**Deferred / quirks.**

- The ring-cap `Trim` deletes by `seq <= max − cap`, so seq gaps from
  buffer drops can leave slightly *fewer* than cap rows — the cap is an
  upper bound, documented in the query.
- Flush timing uses a real ticker (like the dispatcher's drain loop);
  `logged_at` is the slog record's call-site wall clock. Both are
  diagnostics, deliberately outside the injectable-clock invariant —
  tests flush manually and don't assert on timestamps.
- `make smoke-metrics` was not extended to the `engine_steplog_*`
  counters (they're unit-conformance-covered and exercised by the
  engine integration tests); fold them in when 7.5 touches the smoke
  script anyway.
- The compose `up-app` stack runs with OTel off, so live-served lines
  carry no `trace_id` there; `make up-obs` stacks get it. The engine
  integration test pins the traced path with an in-memory recorder.

### 7.5 — Grafana dashboards & alert rules ✅

**Delivered.** M7's closing ticket: dashboards, alerts, and their
enforcement machinery, all provisioned as code.

- **Dashboards** — `deploy/observability/grafana/dashboards/engine.json`
  (`agentloom-engine`: throughput row — step completions/claims/
  dispatches/run completions per second by their label vocabularies;
  queue & outbox row — the four depths, backlog + oldest age,
  dispatch/promote lag p95; latency row — scheduling latency and step
  duration p50/p95/p99, step p95 by type, run p95 by status, the 7.4
  steplog pipeline; failures & healing row — DLQ by source, retries by
  class, the crash-path counters, reconciler heals; fleet row — active
  workers, scrape health, build info) and `api.json` (`agentloom-api`:
  RPS by route, responses by code, in-flight, 5xx, latency quantiles
  overall + p95 by route, 429s, rate-limit decisions, fail-open).
  Provisioned by a file provider (`provisioning/dashboards/
  dashboards.yml` → `/etc/grafana/dashboards`, mounted outside the
  grafana-data volume) with `allowUiUpdates: false`; the 7.1 datasource
  gained the stable `uid: prometheus` the panels reference. Fleet-wide
  gauges are aggregated `max()` — every worker replica samples the same
  values.
- **One production change** — `engine_api_requests_in_flight`, the
  ticket's API-dashboard "in-flight" panel had no backing metric:
  `api.RequestMetrics` grew `RequestStarted`/`RequestFinished`
  (unlabeled by design — route is only known after routing),
  bracketed in `requestLog` with a deferred finish, implemented as a
  gauge on `metrics.APIMetrics`, and integration-asserted to balance
  back to zero after the request matrix.
- **Alert rules** — `deploy/observability/prometheus-rules.yml`, loaded
  via `rule_files` + a compose mount: `QueueDepthGrowing` (ready depth
  elevated AND above its value 10m ago, 5m hold — growth, not absolute
  depth), `DeadLetterRateSpike` (> 0.01/s over 5m, 2m hold),
  `ReclaimRateSpike` (same shape), `OutboxDispatchLag` (oldest pending
  row > 30s, 2m hold, severity critical). Thresholds are dev-scale on
  purpose so the smoke can test-fire them; the rules file documents the
  max()-gauge staleness caveat (a dead fleet silences the sampler-fed
  alerts instead of firing them — pair with absent()/up in production).
- **`make obs-lint`** — promtool `check rules` + `test rules` running
  from the exact Prometheus image tag compose pins (docker, not a
  go-run pin: same binary as production, no module build). The unit
  tests (`prometheus-rules.test.yml`) fire each alert on a synthetic
  series shaped like its failure mode plus a negative case (a deep but
  *draining* queue must stay quiet). Wired into the CI lint job.
  promtool compares annotations strictly, so the test file carries the
  full expected annotation text.
- **Anti-drift audit** — `TestDashboardsAndRulesReferenceRegisteredMetrics`
  (`internal/obs/metrics/dashboards_test.go`): regex-extracts every
  `engine_*` name from both dashboards and both rules files (PromQL,
  synthetic series, and prose alike), normalizes `_bucket`/`_count`/
  `_sum` to family names, and fails unless each is registered on the
  real instrument sets. Renaming a metric now breaks the unit-test job,
  not a panel.
- **`make smoke-dashboards`** (`scripts/dashboard-smoke.sh`) — the
  acceptance script: boots app+obs (5s lease TTL), SIGKILLs the worker
  holding a lease mid-`sleep` (reclaim + takeover on the survivor),
  restores it, then drives a 12-run retries-exhausted burst (paced one
  submission per 2s — see below), three
  fan-outs, a transient retry, and a 429 storm against the admin class;
  asserts every panel query in both dashboards returns a non-empty
  instant vector (three deliberately-quiet-when-healthy queries
  allowlisted: reconciler heals, rate-limit fail-open, 5xx rate), all
  four rules loaded via `/api/v1/rules`, and `DeadLetterRateSpike`
  observed in state `firing` — the documented test-fire, with the alert
  JSON printed from `/api/v1/alerts`.
- **Docs** — `docs/observability.md` (dashboard tour, key signals and
  what each failure smells like, alert table with rationale, the
  test-fire, metrics→traces→logs correlation loop) with screenshots in
  `docs/img/`; ADR-008 gained the as-built "Dashboards & alert rules"
  section and the in-flight gauge inventory row; `metrics-smoke.sh`
  picked up the deferred `engine_steplog_*` EXIST checks plus the new
  in-flight gauge.

**Non-obvious decisions.**

- **Kill before the burst, not after.** The first smoke ordering (burst
  first, SIGKILL later) failed nondeterministically: label-vec counters
  have no series until first increment, and when the SIGKILL victim
  happened to be the only worker that had recorded the 10 dead letters,
  its restart wiped the in-process registry — the series went stale
  fleet-wide, `rate()` went empty, and the alert lost its hold mid-way.
  Running the kill first (also satisfying the holder lookup's
  idle-stack assumption) means the burst lands on a fleet that stays
  alive through the checks.
- **The burst is paced, not fired at once.** Second lesson from the
  same alert: `rate()` measures increases between samples of a visible
  series, and a vec counter has no series until its first increment —
  when one worker (with batch 16, whoever reads first takes all)
  absorbed the whole burst between two scrapes, its counter was *born*
  at the final value and rated as zero forever. One submission per 2s
  spreads ~24s of increments across the 5s scrape interval, so
  Prometheus provably observes rises; 12 rows keep the margin even if
  the first increments predate the series' first sample.
- **Holder→container mapping by hostname prefix, not log grep.**
  demo-crash's `docker logs | grep -q` pattern is pipefail-fragile
  under load: `grep -q`'s early exit SIGPIPEs a still-streaming
  `docker logs` into exit 141, which reads as a miss. Consumer names
  start with the container hostname (= 12-char short container ID in
  compose), so a prefix match needs no pipe at all. demo-crash itself
  is unaffected in practice (idle stack, short logs) and was left
  untouched.
- **"Under the chaos suite" is honored in spirit, not literally**: the
  sustained chaos suite spawns host subprocesses on isolated queue keys
  with no metrics listeners — compose Prometheus structurally cannot
  scrape them. The smoke recreates the suite's signal shape (crash /
  reclaim / takeover, retries, dead letters) against the scrapable
  fleet; the script header records the reasoning.
- **promtool via docker, not `go run`**: the prometheus module is a
  heavy build and the docker route pins the *identical* image tag the
  compose stack runs (Makefile comment keeps them in sync), so CI
  validates exactly what production loads.
- Panels that are empty-when-healthy (reconciler heals, fail-open, 5xx)
  say so in their descriptions and live on the smoke's allowlist —
  "empty is the good state" is a property worth rendering, not padding
  away with `or vector(0)`.

**Deferred / quirks.**

- Grafana's `grafana-data` volume can hold a pre-7.5 datasource without
  the uid; provisioning updates it on boot, but a stack that misbehaves
  wants `make down` + volume removal (noted in docs).
- The alert thresholds are examples, not tuned defaults — re-tuning
  them is part of any real deployment (M20's Helm values are the likely
  home).
- API-side cancel finalizations still don't record `engine_run_duration_
  seconds` (the known 7.2 gap); the run-completions panel inherits it.

## Milestone 8 — Plugin SPI & LLM/tool execution

### 8.1 — ADR-009 & plugin registry refactor ✅

**Delivered.** The plugin SPI as architecture (ADR-009), M4's executor
registry refactored onto it, and the catalog surfaced through the API
and ctl.

- **ADR-009** — the five plugin kinds (executor, tool, retriever,
  model_provider, validator) with identity = (kind, name), names in
  step-type spelling (`^[a-z][a-z0-9_]*$`, ≤ 64); the in-process
  compilation model (plugins are Go packages compiled into the binaries,
  boot-time registration into instance registries, invalid/duplicate
  registration fails startup; out-of-process/WASM explicitly backlog);
  capability-flag semantics — `side_effectful` (must journal, never
  cache), `cacheable` (deliberately stronger than "deterministic": sleep
  is pure but a cache hit would skip the wait, while non-deterministic
  LLM calls ARE cacheable — that reuse is M9's whole point),
  `cost_bearing` (M10 metering) — with the builtin flag table; semver
  version strings whose bump rule is behavioral (bump when cached
  outputs should invalidate — they feed M9 cache keys), dev stubs on
  `0.1.0-stub` so a stubbed plugin is unmistakable; config schemas
  generated from the same Go structs the decoders use, never
  hand-written.
- **`internal/plugin`** — a new leaf package (deliberate layout addendum,
  recorded in the ADR: kind-owning packages import it, it imports none
  of them) holding `Kind`, `Capabilities`, `Manifest` (+`Validate`), and
  the generic `Registry` keyed (kind, name) with typed
  `*InvalidManifestError`/`*DuplicateError` and sorted
  `Manifests()`/`ManifestsByKind()` listings.
- **exec refactor** — `exec.Registry` is now a typed facade over
  `plugin.Registry` with the 4.1 surface unchanged (`NewRegistry`,
  `Register`, `Get` → `*UnknownTypeError`, `Types`), so no engine code
  and none of the ~15 existing test call sites changed. Production
  executors self-describe via the optional `SelfDescribing`
  (`PluginManifest()`), identity-checked (a manifest may not claim
  another name/kind); bare executors get a synthesized conservative
  manifest (version `0.0.0`, `side_effectful: true`, `cacheable:
  false`) — the test-double escape hatch, with
  `TestBuiltinManifestConformance` pinning that everything in the
  shipped sets self-describes and that the flags match the ADR table
  executable-style. All 11 builtins carry manifests.
- **`dag.StepConfigSchema`** — standalone per-step-type config schema
  (invopop reflector, `ExpandedStruct` so fields inline at the root,
  `additionalProperties: false` mirroring `DisallowUnknownFields`);
  the exec registry attaches it automatically at registration since
  executor name = step type, so manifests never hand-carry schemas.
- **`GET /v1/plugins`** — read scope/class, the catalog compiled into
  the API binary (ADR-009's single-build assumption), precomputed at
  construction via the new `api.WithPlugins` option, schemas embedded
  verbatim; empty catalog serves `{"plugins":[]}`. Wired through the
  route→scope, route→class, auth-matrix, and OpenAPI coverage tables;
  spec still lints 100/100. `cmd/api` builds the catalog gated by new
  `AGENTLOOM_API_TEST_EXECUTORS` (mirror of the worker knob; compose
  ties them to the same value so the listing matches the fleet), and
  both deployables log the plugin count at boot.
- **`ctl plugins list`** — kind/name/version/capability-flags table
  (schemas deliberately not rendered — they're machine food).

**Non-obvious decisions.**

- The dev stubs' capability flags describe the REAL executors that
  replace them in place (llm: cacheable + cost_bearing; tool:
  side_effectful; retrieve: cacheable), so M9/M10 middleware written
  against the flags survives the swap; the swap itself is the mandatory
  version bump to 1.0.0.
- No worker→Postgres plugin registration: under the in-process model the
  catalog is a build-time property, so the API serves its own compiled
  set. The mixed-fleet inaccuracy is accepted and recorded in ADR-009
  with dynamic introspection as the deferred trigger.
- Synthesized manifests default `side_effectful: true` / `cacheable:
  false` — the safe assumptions when nothing is known (never skip
  journaling, never serve from cache).
- Version format is a regex, not a semver parser — nothing orders
  versions yet (M9 needs equality only).

**Deferred / quirks.**

- `join`/`branch`/`sleep`/`fail_n_times` are flagless by design
  (control flow / attempt-dependent); flag changes are ADR amendments,
  enforced by the executable table.
- The catalog serves whole-and-unfiltered (a few tens of KB); `?kind=`
  filtering waits until a second kind exists to filter by.

### 8.2 — Step input templating & data flow ✅

**Delivered.** The data-flow layer: `${{ ... }}` template expressions in
step config strings, rendered against upstream outputs and run params in
the worker just before execution, with a static lint at
definition-validation time and strict typed errors at runtime.

- **`internal/dag/template.go`** — templating lives in dag beside CEL
  (same reasoning: it is part of the definition contract and the lint
  needs `Graph`). `ParseConfigTemplates(config)` decodes the config with
  `UseNumber` (number fidelity), walks string values (map keys sorted so
  reported problems and extracted refs are deterministic), and compiles
  each templated string into a `text/template` with `${{`/`}}`
  delimiters — plain `{{ }}` is inert literal text. Each action's
  expression is *rewritten* before parsing: bare references
  (`steps.<id>.output[.<path>]`, `run.params[.<key>[.<path>]]`) become
  strict `__ref` lookups; any other dotted identifier is a typed
  unknown-root error; and bare identifiers outside the allowlist
  {get, default, toJson, truncate, true, false, nil} are rejected —
  the mechanism that makes the FuncMap genuinely restricted, since
  text/template's builtins (`printf`, `index`, `call`, ...) cannot be
  removed from the engine itself. Control structures, variables, and
  `:=` are rejected: an expression is a single pipeline. String literals
  may be single-quoted (`get 'steps.a.output.x'`) and are re-emitted
  double-quoted, sparing JSON documents `\"` noise; ref-shaped quoted
  literals are recorded as *lenient* refs — exempt from lint (nil on
  miss is `get`'s contract) but included in `StepIDs()`, the prefetch
  set.
- **Render semantics.** `Render(RenderData{Outputs, Params})` resolves
  against a `{steps: {<id>: {output: ...}}, run: {params: ...}}` root.
  Bare refs are strict: any miss is a typed `*MissingRefError` (never an
  empty string); `get | default` is the sanctioned opt-out. A string
  that is exactly one expression is **type-preserving** — the resolved
  value splices in as JSON (via a `__cap` capture func; whole objects
  and arrays flow between steps), while mixed strings interpolate via
  `__fmt` (strings verbatim, numbers/bools in JSON spelling, nil → "",
  composites as compact JSON). Rendering happens exactly once on the
  authored definition, so template text arriving through outputs or
  params is inert data (pinned by the injection test). Template-free
  configs return byte-identical; rendered ones re-encode canonically
  (sorted keys, no HTML escaping — `marshalNoEscape`).
- **Static lint** (`checkTemplates`, sharing the well-formed-graph gate
  with `checkGraphSemantics`, which now takes the Graph instead of
  rebuilding it): five new codes — `template_invalid`,
  `template_ref_invalid`, `template_ref_unknown_step`,
  `template_ref_not_upstream`, `template_ref_unknown_param` — with
  path-qualified issues (`steps[2].config.input.greeting`). Upstream
  means strict normal-edge ancestry (`Graph.Ancestors`): loop-edge-only
  reachability and self-references are rejected (pinned by the
  writer⇄critic test). One carve-out: a *templated* sleep `duration`
  skips the literal parseability check (the executor re-validates the
  rendered value; pinned end-to-end).
- **Engine wiring** (`internal/engine/render.go`, called from `execute`
  after the registry/timeout checks, inside a `step.render` span):
  configs without `${{` pass through with zero reads — the fast path
  every pre-8.2 workflow takes. Otherwise the run row (params) and one
  batched `ListRunStepsByIDs` read (new sqlc query + `StepRepo.ListByIDs`,
  the only store change) supply the data; only `succeeded` steps
  contribute outputs, so referencing a skipped or unfinished step
  (possible behind a join-any) is a strict missing-ref failure. The
  `*renderError` wrapper separates deterministic failures — routed to a
  permanent failure completion (new ADR-006 taxonomy row 15), with the
  referenced steps' statuses appended to missing-ref diagnostics — from
  transport errors, which redeliver undecided. `StepContext.Config` now
  carries the rendered config (comment updated); `Input` stays nil,
  documented as reserved.
- **Fixture & proof.** `examples/definitions/echo_pipeline.json` — a
  three-hop echo chain moving params (whole objects included), nested
  paths, interpolation, a two-hops-up reference, and all four functions;
  corpus-pinned, construct-pinned (`TestExampleEchoPipelineCoversTemplateConstructs`),
  and executed end-to-end by the engine integration suite off the real
  file with per-step output assertions. Companion integration tests:
  lint-clean-but-missing-at-runtime ref → exactly one attempt, outcome
  `permanent`, one DLQ row source `permanent` carrying the diagnostic,
  run failed under fail_fast; and a templated sleep duration executing.
  kitchen_sink gained template-construct pins (its `${{ }}` strings —
  informal since M1 — now lint for real, and the whole corpus passed
  unchanged).

**Non-obvious decisions.**

- Rendering happens in place inside `Config`, not as a separate
  `StepContext.Input` payload — executors decode one thing, and the
  echo/branch `Input` fallbacks stay dormant. `Input` is kept (churn-free)
  and documented as reserved for a future merged-input payload.
- The rendered config is ephemeral — recomputed per attempt, never
  written back to `run_steps.config` (the integration test pins that the
  stored config keeps its templates). Deterministic by construction:
  outputs and params are immutable within a run.
- Strictness is positional, not global: bare refs error on miss, quoted
  `get` paths resolve to nil. This satisfies "strict missing-reference
  errors" while keeping `default` usable — a single strict mode would
  make `default` unreachable.
- Type preservation applies only to whole-expression strings — the
  boundary is syntactic and visible in the definition, not inferred
  from the resolved value's type.

**Deferred / quirks.**

- No escape sequence for a literal `${{` in config text (a prompt
  *about* templating cannot be authored verbatim); revisit if it bites.
- Templates render only in step `config` — envelope fields
  (retry/timeout/max_wall_clock) are materialized at instantiation and
  take literals only, even though params exist by then.
- `run.params` and `steps.<id>.output` resolve as whole values too
  (bare, without a key/path suffix) — deliberate, the fixture uses it.
- Rendered inputs are not persisted for observability; if the M18
  dashboard wants "what did this step actually receive", that becomes a
  new column then.

### 8.3 — Model provider interface & Anthropic provider ✅

**Delivered.** `internal/llm` — the unified model-provider interface
(ADR-009 kind `model_provider`) and its first implementation, the
Anthropic Messages API, non-streaming v1.

- **The contract** — `Provider` is one method deep:
  `Chat(ctx, ChatRequest) (ChatResponse, error)` plus the ADR-009
  `Manifest()`. `ChatRequest` carries model, optional system prompt,
  messages (role + content blocks), tool definitions, required positive
  `MaxTokens` (Anthropic requires it, and a mandatory bound keeps M10
  budgets estimable pre-call), and optional `Temperature` (nil = provider
  default; range checking is deliberately the provider API's — vendors
  disagree on ranges). Content is the Anthropic block model — `text`,
  `tool_use`, `tool_result`, discriminated and shape-validated — which
  OpenAI's message/tool_calls model maps onto in 8.4. `ChatResponse`
  returns model, verbatim stop reason, blocks, and `Usage`
  (input/output tokens) — **mandatory on every success**: a 200 without
  usage decodes as a malformed response (transient), never a lenient
  zero, because M10's ledger meters it. Convenience accessors `Text()`
  / `ToolUses()` serve the 8.6 executor.
- **Package boundaries** — a leaf package like ratelimit: imports `dag`
  for the ADR-006 class vocabulary (where `ErrorClass` lives;
  `exec.ErrorClass` is just an alias) and `plugin` for the manifest,
  never exec/engine. No SDK dependency — a hand-rolled `net/http`
  client (~100 lines of wire mapping) with injectable `BaseURL` and
  `HTTPClient` keeps fixtures hermetic and the error surface fully
  owned. Providers make **exactly one call per Chat** (retry policy is
  the M5 engine's, informed by the classes providers attach) and never
  log (the caller owns observability). No clock anywhere in the
  package.
- **Error taxonomy** — typed `*llm.Error` with provider name, ADR-006
  class, HTTP status, provider error code, `RetryAfter`, request id,
  message, and wrapped cause. Classification by status: transport
  failures and 408/429/5xx (500 `api_error`, 529 `overloaded_error`,
  unknowns) → transient; other 4xx (400/401/403/404/413) and
  client-side validation (caught before any round-trip, zero HTTP
  calls) → permanent; malformed 200s (undecodable body, missing usage,
  unrepresentable content block) → transient per ADR-006's unclassified
  default. **Context errors pass through unclassified** — a ctx-aborted
  call returns the wrapped context error, never an `*llm.Error`, so the
  engine keeps the timeout/cancelled judgment (ADR-006 rows 3/8), pinned
  by a test asserting `errors.As` fails. `RetryAfter` parses only the
  Retry-After delta-seconds form: the HTTP-date form needs a wall clock
  (injectable-time invariant) for a shape no LLM provider sends, and
  zero means "no suggestion" — advisory in M5, consumed by M9's
  backpressure. ADR-006 gained the as-built "Provider error taxonomy"
  section.
- **Secret hygiene, structurally** — the error type has no field that
  can hold request headers or the outgoing payload; constructors ingest
  only the response body's error type/message and the
  request-id/retry-after headers, so the key cannot reach an error by
  construction (and the key travels in `x-api-key`, so a `url.Error`
  can't carry it either). The assertion test walks every error path's
  full unwrap chain under `Error()`/`%+v`/`%#v` asserting the
  runtime-built key (and any fragment of it) never appears, with two
  positive controls (the 400 fixture's marker message and the request
  id do flow through) proving the assertion has teeth. Keys in tests
  are constructed at runtime per the 6.1 discipline.
- **Anthropic specifics** — `POST {base}/v1/messages`, pinned
  `anthropic-version: 2023-06-01` (bumping it is a behavior change that
  must ride a manifest version bump), `NewAnthropic` rejects a missing
  key at construction so a mis-wired deployment fails at boot, response
  bodies capped at 32 MiB, a 10-minute backstop client timeout when the
  caller injects neither client nor deadline (LLM calls are
  legitimately slow; the step timeout is M5.3's, delivered via ctx).
  Unknown response block types are an error, not silently dropped —
  dropping content this build can't represent would corrupt the
  completion. `Manifest()`: kind `model_provider`, name `anthropic`,
  `1.0.0`, cacheable + cost-bearing (the llm-stub flag rationale).
- **Config** — `config.LLMConfig` with `AGENTLOOM_ANTHROPIC_API_KEY`;
  empty = provider unconfigured, deliberately not a load error (a
  worker running no llm steps must boot keyless, and 8.4 requires
  either provider to be absent without breaking startup — presence is
  enforced where a provider is constructed, i.e. 8.6's executor
  wiring). Nothing consumes it until 8.6.
- **Tests** — recorded-fixture suite under `testdata/anthropic/`
  (success, tool-use success, 429/529/500/400/401/404 error envelopes)
  driven through httptest: the taxonomy matrix pins class, code, status,
  retry-after (including unparseable-header → 0), and request id per
  shape; **golden request fixtures pin the outgoing wire shape both
  ways** (headers + structural JSON comparison of the encoded body for
  the basic and full shapes — the contract 8.4/8.6 build on);
  malformed-200 matrix; transport-error and context-cancellation paths;
  client-side validation with a zero-calls assertion; `ChatRequest`
  validation table; manifest conformance. Optional live smoke behind
  `LIVE_LLM_TESTS=1` + a key env var (one tiny haiku call asserting
  text + positive usage), in no CI job.

**Non-obvious decisions.**

- The typed `llm.Registry` facade and the `GET /v1/plugins` listing of
  model providers are deferred to 8.4 with the routing table — 8.3
  defines `Manifest()` so 8.4 only wires, but a registry with one
  hardcoded member would just be churn.
- `*llm.Error` carries `dag.ErrorClass` directly rather than wrapping
  `exec.ClassifiedError`: llm must not import exec (the executor SPI is
  a consumer of providers, not a dependency), so the 8.6 executor does
  the one-line translation at its boundary.
- 401/403 map permanent even though a key rotation could heal them:
  within one attempt's lifetime the key is fixed, and burning a retry
  budget on a misconfigured credential hides the misconfiguration.

**Deferred / quirks.**

- Streaming, provider-side cache-token usage fields, and the HTTP-date
  Retry-After form are all explicitly out (backlog / M10 / never
  observed in the wild, respectively).
- `System` is a plain string; Anthropic's array-of-blocks system form
  (needed for provider-side prompt caching) waits for the milestone
  that wants it.
- The 32 MiB response cap and 10-minute backstop timeout are
  constants, not config — promote to `LLMConfig` knobs if a real
  deployment ever needs to tune them.

### 8.4 — OpenAI provider & model routing ✅

**Delivered.** The second provider behind the 8.3 interface, the typed
`llm.Registry` facade + routing table (both deferred here from 8.3), and
the plugin-listing wiring that surfaces configured providers on
`GET /v1/plugins`.

- **OpenAI provider** (`internal/llm/openai.go`) — mirrors the Anthropic
  provider structurally: hand-rolled `net/http` (no SDK), one call per
  `Chat`, no internal retries, no logging, no clock; injectable
  `BaseURL`/`HTTPClient`; 32 MiB body cap; the shared 10-minute backstop
  timeout; `NewOpenAI` rejects a missing key at construction. Targets the
  **Chat Completions API** (`POST /v1/chat/completions`), deliberately
  not the newer Responses API — Chat Completions is the stable shape and
  carries tools + usage in the non-streaming form this milestone needs
  (Responses API → backlog).
- **The unified↔wire mapping** handles the two structural mismatches
  between the unified (Anthropic-shaped) content model and OpenAI's
  message model: (1) the system prompt becomes a leading `system`
  message; (2) a user turn's `tool_result` blocks fan out into standalone
  `tool`-role messages (OpenAI has no per-block roles). `MaxTokens` maps
  to `max_completion_tokens` (the deprecated `max_tokens` is rejected by
  o-series reasoning models); `tool_use` → `tool_calls` with
  JSON-**string** `arguments`; auth via `Authorization: Bearer`. The
  response decodes `choices[0]` (zero choices → malformed-200), surfaces
  a non-empty `refusal` as a text block (it *is* the completion),
  decodes `tool_calls` (unknown call type → malformed, never silently
  dropped), and **normalizes `finish_reason` onto the Anthropic
  stop-reason vocabulary** the unified `ChatResponse` documents
  (`stop`→`end_turn`, `length`→`max_tokens`, `tool_calls`/`function_call`
  →`tool_use`; unknown reasons like `content_filter` pass through
  verbatim) so the 8.6 executor branches on one vocabulary. Usage is
  mandatory on success (missing → malformed-200), exactly as 8.3.
- **Error taxonomy is provider-agnostic** — OpenAI reuses 8.3's
  `classifyStatus` and `parseRetryAfter` unchanged; the taxonomy table
  is identical by design. Two OpenAI specifics: a new clock-free
  `parseRetryAfterMillis` reads the millisecond `retry-after-ms` header
  (preferred over whole-second `Retry-After`), and `Error.Code` prefers
  OpenAI's specific `code` (e.g. `context_length_exceeded`) over the
  coarse `type`, falling back to `type` when `code` is null. Request id
  from `x-request-id`; context errors still pass through unclassified.
  Secret hygiene remains structural (the error type has no field for
  headers/payload) and is pinned by the same every-error-path assertion
  with positive controls.
- **Registry & routing** (`internal/llm/registry.go`) — `llm.Registry`
  is the typed facade over `plugin.Registry` (kind model_provider), same
  pattern `exec.Registry` set: `Register` identity-checks the kind,
  duplicate/invalid → boot error; `Get`/`Manifests`/`Names`.
  `Resolve(explicitProvider, model)` returns the provider **and the
  canonical model the provider should see** via three priority rules:
  (1) explicit provider field wins; (2) `"<provider>/<model>"` namespace
  form routes by the named provider and strips the prefix (reserved now
  for 8.5's `mock/...`); (3) a longest-vendor-prefix table for bare
  models (`claude`→anthropic; `gpt-`/`chatgpt-`/`o1`/`o3`/`o4`→openai).
  Two deliberately distinct typed errors: `*UnknownModelError` (no rule
  matched — the ticket's required error) vs. `*ProviderUnavailableError`
  (routed provider not registered, i.e. key absent) — the split makes a
  misconfiguration diagnosable, and both are deterministic so 8.6 maps
  them permanent.
- **Independent configurability** — `llm.NewRegistryFromKeys(
  ProviderKeys{Anthropic, OpenAI})` is the *one* constructor both
  deployables call: each provider is built iff its key is present, an
  empty registry is valid, and construction never errors on an absent
  key. This is the third done-when, realized as a single shared function
  so the plugin catalog and the routable set can't drift between api and
  worker. `config.LLMConfig` gained `OpenAIAPIKey` /
  `AGENTLOOM_OPENAI_API_KEY` with the same "empty = unconfigured, not a
  load error" semantics as the Anthropic key.
- **Plugin listing** — `cmd/api` folds the configured providers'
  manifests into the slice handed to `api.WithPlugins` (which re-sorts
  into canonical (kind, name) order), so `GET /v1/plugins` lists exactly
  the providers a matching worker fleet could route to. `ctl plugins
  list` needed zero changes (kind-generic since 8.1). Compose and
  `.env.example` pass `AGENTLOOM_ANTHROPIC_API_KEY` /
  `AGENTLOOM_OPENAI_API_KEY` through to both services, empty by default.
- **Tests** — recorded-fixture suite under `testdata/openai/` mirroring
  8.3 one-for-one (golden wire requests both ways pinning the
  system-fold + tool-role fan-out + `max_completion_tokens`; the full
  taxonomy matrix incl. `retry-after-ms` precedence; finish-reason
  normalization incl. unknown-passthrough; malformed-200 matrix incl.
  no-choices; transport/context/validation with a zero-calls assertion;
  the secret-hygiene walk with positive controls; manifest conformance).
  `registry_test.go` covers the routing matrix, both typed errors, the
  registration discipline (duplicate/wrong-kind/nil), and the
  `NewRegistryFromKeys` neither/either/both-keys matrix. The API plugins
  integration suite gained `TestListPluginsWithProviders` (providers
  appear with kind model_provider + capability flags, catalog stays
  sorted). OpenAI live smoke added behind `LIVE_LLM_TESTS=1`, in no CI
  job.

**Non-obvious decisions.**

- **Chat Completions, not Responses API.** The stable, tool-and-usage
  complete non-streaming shape; the Responses API is a backlog item, not
  a v1 requirement.
- **`internal/llm` stays a config-free leaf.** `NewRegistryFromKeys`
  takes a plain `ProviderKeys` struct (not `config.LLMConfig`), so config
  never enters llm's import graph — preserving 8.3's "leaf like
  ratelimit" property. The two deployables translate their `cfg.LLM`
  fields at the call site.
- **The `"<provider>/<model>"` namespace form is built now though 8.5
  needs it**, because it's the addressing scheme the mock provider's
  `model: "mock/..."` requires and it belongs in the routing table, not
  bolted on later.

**Deferred / quirks.**

- `insufficient_quota` arrives as a 429 and thus classifies transient by
  status — an accepted v1 quirk (the retry budget bounds it and it
  dead-letters); refine to permanent if it ever hurts. Recorded in
  ADR-006.
- OpenAI's `tool_result.is_error` flag has no Chat-Completions
  equivalent, so it is dropped in the wire mapping — the tool message's
  content still carries the result text the model reads.
- The vendor-prefix routing table is a small static slice; it grows as
  providers land (8.5's mock registers by name and is addressed via the
  namespace form, needing no prefix entry).

### 8.5 — Mock/simulation provider ✅

**Delivered.** A third model provider behind the 8.3 interface — a
deterministic, offline `Mock` for tests and load (the M19 workhorse) —
plus its config/deployable wiring and the reused e2e fixture.

- **Mock provider** (`internal/llm/mock.go`) — implements `Provider`
  (`Chat` + `Manifest`) with no HTTP client, no network, and no clock of
  its own. Registered under name `mock` and addressed purely through the
  8.4 registry's namespace form (`model: "mock/<model>"`, prefix
  stripped by `Resolve`), so it needed **zero** routing-code changes.
  `NewMock(MockConfig)` rejects a malformed script at construction (bad
  regex, injection rates outside [0,1], `min > max` / negative latency, a
  rule with no responses) — the analogue of the HTTP providers' boot-time
  key check.
- **Scripting.** Ordered `MockRule`s match on prompt **substring**,
  **regex**, or the 1-based global **call ordinal** (`OnCall`); clauses
  are ANDed and the first matching rule wins. Each rule's `Respond`
  sequence returns its entries in order with the last entry **sticky**
  (the queuetest `Script` shape). A `MockOutcome` is either a success
  (single `Text`, or full `Blocks` for tool_use scripting, with explicit
  or estimated `Usage`) or — when `Status != 0` — a scripted `*llm.Error`
  classified through the **same** provider-agnostic `classifyStatus` the
  HTTP providers use. `MockInjection` adds global per-call 429/500 rates
  (429 evaluated before 500). A `Hang` outcome blocks on ctx until
  cancelled and returns the context error **unclassified**, exactly like
  the real providers (ADR-006 rows 3/8).
- **Default echo.** With no rules the mock deterministically echoes the
  last user text (`[mock] …`) with estimated usage. This zero-config
  behavior is what lets two chained `llm` steps pass real data through
  8.2 templating with no scripting at all — the shape the reused e2e
  fixture exploits.
- **Determinism & offline.** One seeded PCG PRNG (`math/rand/v2`) guarded
  by a mutex; the whole per-call draw sequence (call counter → injection
  lottery → latency sample → per-rule cursor) advances under the lock, so
  a given seed and a given *sequential* call order produce a
  byte-identical transcript (responses, errors, latency draws). The
  latency wait itself happens outside the lock (concurrent callers aren't
  serialized), and time is injectable through a `Sleep` seam — the only
  clock the mock touches. Offline is asserted both **structurally** (the
  struct has no field that can hold a transport or a secret) and
  **dynamically** (the full scripting/injection/latency matrix runs with
  `http.DefaultTransport` swapped for a tripwire `RoundTripper` that fails
  the test on any use).
- **Usage.** Present on every success (the mandatory-usage contract):
  explicit override when the outcome sets one, else a deterministic
  ~4-chars-per-token estimator over request and response text, so load
  tests get plausible accounting without a tokenizer.
- **Wiring.** `ProviderKeys` gains `Mock *MockConfig` (nil = absent,
  never a boot error — it carries no key, so it is scripted, not
  authenticated), constructed by the one shared `NewRegistryFromKeys`.
  `config.LLMConfig.MockEnabled` / `AGENTLOOM_LLM_MOCK_ENABLED` toggles it
  (binary default **off**; `docker-compose.yml` and `.env.example`
  default it **on** so the M8 exit-criterion workflow runs on the mock in
  CI with no key). `cmd/api` folds the mock's manifest into
  `GET /v1/plugins`; `internal/config` stays a non-importer of `llm` (the
  toggle→`MockConfig` translation is inlined at the call site, preserving
  llm's config-free-leaf property). Manifest is (`model_provider`,
  `mock`, `1.0.0`, `cacheable` only) — the first provider that is
  cacheable but **not** cost-bearing (the point is a free provider) and
  not side-effectful.
- **Reused e2e fixture.** New canonical
  `examples/definitions/mock_pipeline.json` — a converted linear M4 chain
  of two `llm` steps (`draft` → `refine`) that pass data via
  `${{ steps.draft.output.text }}`, then an echo `record` step —
  pinned in the examples corpus and golden-tested by `internal/dag`. The
  engine integration suite (`mock_llm_integration_test.go`) runs it
  end-to-end on the production claim/complete pipeline through a minimal
  in-test `llm` executor (resolve model → `Chat` → `{model, text,
  usage}`), asserting the deterministic echoed chain and positive usage
  on each llm step.
- **Tests.** `mock_test.go` covers the full scripting matrix (fixed,
  sequence-sticky, substring/regex conditionals, `OnCall` ordinal,
  scripted 429/400 with class + retry-after, latency draws via an
  injected recording sleep, hang→unclassified-cancel), determinism under
  identical seed with a different-seed positive control, injection
  reproducibility, the zero-external-calls tripwire, validation-before-
  any-draw, the `NewMock` config-error matrix, and manifest conformance.
  `registry_test.go` adds the `mock/` namespace-routing case and the
  `NewRegistryFromKeys` mock matrix; `config_test.go` the toggle
  (default-off / on / non-boolean → error); the API plugins integration
  suite gained `TestListPluginsWithMockProvider` (cacheable-not-cost-
  bearing flags on the catalog).

**Non-obvious decisions.**

- **Version `1.0.0`, not a stub version.** Unlike the dev-stub executors
  (`0.1.0-stub`, replaced in place), the mock is a permanent fixture of
  the system — it is never swapped out — so it carries a real version
  from the start.
- **In-test `llm` executor for the e2e, not the real one.** The
  production `llm` step executor is 8.6's ticket (templated messages,
  usage-on-attempt persistence, retry-class mapping). 8.5's e2e uses a
  ~30-line stand-in that does the provider call and nothing else, purely
  to prove the mock is registered-like-any-provider and deterministic
  end-to-end; 8.6 deletes it and supersedes this test with its compose
  e2e. This is the only honest way to satisfy "reused to convert one M4
  e2e fixture" this ticket without front-running 8.6.
- **Mock defaults on in compose but off in the binaries.** Mirrors the
  `AGENTLOOM_WORKER_TEST_EXECUTORS` precedent: the dev/CI stack wants the
  free offline provider available so llm steps run without a key, while a
  real deployment opts in explicitly.
- **Determinism is defined for sequential call order.** Concurrent
  callers still draw from the same seeded stream under the lock (a
  deterministic *set* of draws), but which call gets which draw depends
  on scheduling; the byte-identical-transcript guarantee is scoped to a
  sequential order and documented as such.

**Deferred / quirks.**

- Tool_use scripting is representable (`MockOutcome.Blocks`) but has no
  dedicated fixture yet — 8.7's tool executor will exercise it.
- The latency estimator and token estimator are intentionally crude
  (uniform draw; ~4 chars/token); M19 can refine the distributions if
  load-test fidelity ever demands it.

### 8.6 — LLM step executor ✅

**Delivered.** The production `llm` step executor, replacing the dev stub
in place: it renders messages (via 8.2 templating, already applied by the
time the executor runs), routes the model to a provider, makes one `Chat`
call, persists the completion and the provider's token usage, and maps
provider failures onto the M5 retry classes. Usage gains a durable home on
the attempt row and surfaces in the run-status API.

- **The executor** (`internal/exec/llmexec.go`) — `exec.LLMExecutor`
  (constructed with a `*llm.Registry`; nil is valid — a keyless worker
  boots and any `llm` step then fails permanent at resolve time). Version
  `1.0.0`, cacheable + cost-bearing (ADR-009). `Execute` strict-decodes
  the already-rendered `*dag.LLMConfig`, builds a unified `ChatRequest`
  (prompt → one `UserText` message; `messages[]` mapped onto the two
  conversational roles; `max_tokens` default 1024 when absent — the
  provider interface requires a positive bound; `temperature` passed
  through by pointer), routes via `Registry.Resolve("", model)`, calls
  `Chat` exactly once (no retries — the M5 engine owns retry), and
  persists `{model, stop_reason, text, tool_calls?, usage}`. `text` is
  always present (empty when the completion had no text blocks) so a
  downstream `${{ steps.x.output.text }}` never misses; `tool_calls`
  appears only when the model called tools.
- **Error mapping** — `classifyProviderError`: a `*llm.Error` wraps into
  `exec.ClassifiedError` carrying the provider's ADR-006 class (429 →
  transient, 4xx → permanent), the `*llm.Error` still reachable via
  `errors.As`; the engine's `classifyFailure` then honors it as declared.
  Routing failures (`*UnknownModelError`, `*ProviderUnavailableError`) and
  a nil registry are deterministic ⇒ force-classified permanent. Context
  cancellation/deadline pass through **unwrapped** so the engine keeps the
  timeout/cancelled judgment (ADR-006 rows 3/8).
- **Usage persistence** — `exec.Output` gained `Usage *exec.Usage`;
  migration 0012 adds nullable `step_attempts.usage` JSONB. The store's
  `finishAttempt`/`SucceedStepArgs` thread it through, and the engine's
  `completeSuccess` marshals `out.Usage` into the success completion tx (a
  marshal failure logs and stores NULL rather than failing a succeeded
  step). `AttemptView.Usage` surfaces it in `GET /v1/runs/{id}` (+
  openapi.yaml, still lint 100/100). NULL for every non-`llm` step, every
  failed provider call, and all pre-0012 rows.
- **Wiring** — `Builtins`/`CoreBuiltins` now take a `*llm.Registry`;
  `cmd/worker` builds it from `cfg.LLM` (Anthropic/OpenAI keys + mock
  toggle) the way `cmd/api` already did, and `cmd/api` passes nil (it
  never executes steps — the manifest is identical either way). The dev
  `StubLLMExecutor` is deleted; `manifest_test`'s stub set shrinks to
  `{tool, retrieve}`.
- **Validation** — `checkLLMConfig` now also validates each `messages[]`
  entry's role ∈ {user, assistant} and non-empty content at submit time,
  so a bad role fails validation rather than mid-run (all corpus fixtures
  use `user`, unaffected).

**Tests.** `llmexec_test.go` (offline, a recording provider + the mock):
request mapping (prompt/messages/roles), max_tokens default, temperature
passthrough, tool-call output, always-present text, the provider-error
class matrix (429→transient / 4xx→permanent, `*llm.Error` still
reachable), context-error passthrough, routing failures permanent (and
never reaching the provider), nil-registry permanent, missing-model
invalid-config. `mock_llm_integration_test.go` rewritten to drive the
**production** executor over `mock_pipeline.json` (8.5's shim deleted),
asserting the templated data flow **and** usage on each `llm` attempt row
(with a non-`llm` step recording none). `llm_retry_integration_test.go` —
the injected-429 acceptance on the 5.2 fake clock: a scripted
`[429, success]` mock, attempt history exactly `[transient, succeeded]`,
one retry event, backoff honored, usage only on the successful attempt.
`api_integration_test.go`'s fanout-to-completion test now asserts
`brainstorm`/`synthesize` output text + `attempts[].usage` through the API
(criterion 2). Migration round-trip test bumped to version 12.

**Non-obvious decisions.**

- **`fanout.json`'s llm models moved to `mock/sim-1`.** Post-8.6 the `llm`
  steps actually call a provider, and the only offline provider on
  compose/CI/smoke is the mock (no API key). fanout.json is executed to
  `succeeded` by the API completion test, the metrics/dashboard smoke
  scripts, and the M4.6 compose acceptance — all keyless — so its llm
  models had to point at the mock or every one of those would break. Its
  tool/retrieve steps are already stubs, so it was already a demo-shape
  fixture; `linear.json` (validation/instantiation-only, never executed)
  stays realistic (`anthropic/…`).
- **`cmd/api` passes a nil provider registry to `Builtins`.** The API
  never executes steps (ADR-002), and the llm executor's *manifest* (what
  the API serves at `/v1/plugins`) is self-described independent of the
  registry — so nil is correct and avoids reordering the api's provider
  block. Only `cmd/worker` builds the real routable registry.
- **Usage in JSONB, not two columns.** Mirrors the existing `error` JSONB
  and stays extensible for M10's provider-cache token counts (a 8.3
  omission) without another migration.
- **No side-effect journal for llm calls.** An LLM call is exactly what
  the idempotency-key / retry model treats as safely re-executable, and
  M9's response cache is the dedup layer; journaling it would add a
  durable write per call for no correctness gain.

**Deferred / quirks.**

- `provider` (explicit) and `system` config fields are not added to
  `LLMConfig` — no 8.6 criterion needs them, and each is a versioned-
  contract change (schema regen, canonical encoding, corpora pins) better
  made when a consumer requires it. Routing today is by model id alone.
- Tool-use *output* is persisted (`tool_calls`) but no fixture chains a
  tool call back into the model yet — the multi-turn tool loop is 8.7+.

### 8.7 — Tool SPI & built-in tools ✅

**Delivered.** The tool SPI (`internal/tools`), a real `tool` step
executor replacing the dev stub in place, and two built-in tools —
`http_request` (allowlist/SSRF guard, timeout, automatic `Idempotency-Key`
from 5.5 on non-GET) and `json_transform` (gojq). No migration — the
ticket touches no schema.

- **The SPI** (`internal/tools`, the fourth kind-owning leaf package —
  imports `plugin`+`dag`+stdlib+gojq+jsonschema/v6, never exec/engine).
  `Tool` = `Manifest() plugin.Manifest` + `Invoke(ctx, Invocation)
  (json.RawMessage, error)`. The ticket's literal `Invoke(ctx, args)` grew
  to an `Invocation` struct so `http_request` can read the step's stable
  idempotency key (5.5) and a logger without a later signature churn (the
  `StepContext` precedent). Every tool **must** declare its args JSON
  Schema in `Manifest().ConfigSchema` — generated from the tool's Go arg
  struct with the same invopop reflector settings `dag.StepConfigSchema`
  uses (`argsSchema`), so a served schema can never drift from what the
  tool decodes. A `json.RawMessage` arg field reflects to the permissive
  `true` schema, which is how `http_request`'s `body` accepts arbitrary
  JSON.

- **The registry validates args, generically** (`tools.Registry`, the
  typed facade over `plugin.Registry` kind tool). Unlike executor config
  (validated only by strict struct decode), tool args are validated
  against the declared schema at dispatch by a real 2020-12 validator
  (`santhosh-tekuri/jsonschema/v6`, compiled once per tool at registration
  — an uncompilable schema, a missing schema, wrong kind, nil, or a
  duplicate name all fail boot). `ValidateArgs` is the framework gate the
  executor calls **before** `Invoke`, so "bad args → permanent failure, no
  call" is a framework guarantee every tool inherits, not a per-tool
  discipline. A violation is a typed `*ArgsValidationError` (permanent);
  an unknown tool is `*UnknownToolError`. Validation detail is collapsed
  to a single structure-only line (field names, not values).

- **`exec.ToolExecutor`** (`internal/exec/toolexec.go`) replaces
  `StubToolExecutor` in place — version `0.1.0-stub` → `1.0.0` (the M9
  cache-bust trigger), flags unchanged (`side_effectful` — an unknown tool
  must be assumed to act on the world; individual tools carry their own
  per-tool flags under kind tool). It decodes the already-8.2-rendered
  `ToolConfig`, validates args, looks the tool up, invokes once (no retry —
  the M5 engine owns retry), and persists the tool result **verbatim** (no
  `{tool, result}` envelope — the tool name already lives in the step
  config, and downstream templating reads `${{ steps.x.output.<field> }}`
  directly). A `*tools.Error`'s class is honored via `ClassifiedError`, a
  `*HostNotAllowedError` maps permanent, and context errors pass through
  unwrapped (engine judges timeout/cancelled). A nil registry is valid — a
  tool step then fails permanent at lookup (the 8.6 keyless-worker
  pattern).

- **`http_request`** (side_effectful): one outbound call guarded by a host
  **allowlist** (`hostSet`; each entry a hostname — any port — or
  host:port; case-insensitive; **empty allowlist denies every host**, the
  safe default so the tool is inert until a deployment names hosts). A
  blocked host is the typed `*HostNotAllowedError` (permanent) surfaced
  *before any connection*, and `CheckRedirect` re-validates every redirect
  hop against the same allowlist on a cloned client (closing the
  redirect-to-forbidden-host bypass). Bounded by a timeout (per-request
  `timeout` arg overrides the configured default) and a response-size cap
  (oversize → permanent). Automatic `Idempotency-Key` header on non-GET
  from `Invocation.IdempotencyKey` — it wins over any user-supplied header
  (stability is the guarantee), and GET carries none. Outcome: 429/5xx →
  transient, other non-2xx → permanent, transport → transient; a
  self-imposed timeout tripping while the engine ctx is alive → transient,
  distinguished from engine cancellation (passthrough). Output is
  `{status, body}` with the body embedded as JSON when it parses, else a
  JSON string. Secret hygiene is structural (the error type holds no
  header/body field) and pinned by a positive-control test.

- **`json_transform`** (cacheable — the first pure built-in tool):
  evaluates a gojq (jq-syntax) program over a JSON input under `ctx`
  (`RunWithContext`, so a pathological program is bounded by the step
  timeout). One emitted value returns that value; zero-or-many returns a
  JSON array (empty → `[]`). Compile and runtime (jq) errors are
  deterministic → permanent; a ctx error passes through unwrapped.

- **Wiring.** `tools.NewBuiltins(HTTPOptions)` is the one constructor both
  deployables call. `config.ToolsConfig` reads
  `AGENTLOOM_TOOLS_HTTP_ALLOWLIST` (comma-separated, empty-default
  deny-all), `_TIMEOUT` (30s), `_MAX_RESPONSE_BYTES` (1 MiB). `cmd/worker`
  builds the registry from config and hands it to the exec registry;
  `cmd/api` folds the tools' config-independent manifests into
  `GET /v1/plugins` (the OpenAPI `kind` enum already carried `tool` from
  8.1). `exec.Builtins`/`CoreBuiltins` gained a `*tools.Registry`
  parameter (the 8.6 `*llm.Registry` precedent; ~20 call sites, most
  passing nil). Compose + `.env.example` pass the three tool env vars to
  the worker with an empty allowlist by default.

- **Fixture.** `fanout.json` (the one *executed* canonical fixture) moved
  its `fetch_news` tool step from `http_request` (which the empty compose
  allowlist would block) to an offline `json_transform` over the templated
  topic — the same offline-conversion move 8.6 made for the llm models —
  so the M8 exit-criterion workflow stays fully offline on compose/CI. The
  non-executed corpus fixtures (`linear`, `conditional_branch`,
  `kitchen_sink`) keep realistic `http_request` steps.

**Non-obvious decisions.**

- **A real JSON Schema validator, not strict struct decode.** The ticket
  frames validation as "against the tool schema," and the SPI's point
  (ADR-009) is that one generated schema drives validation *and* the UI.
  Making `ValidateArgs` a framework service on the compiled schema means
  the acceptance guarantee holds for every future third-party tool without
  it re-implementing shape checks. `santhosh-tekuri/jsonschema/v6` was
  already in the module cache; `gojq` was the only network fetch.
- **`Invocation` struct over the literal `Invoke(ctx, args)`.**
  `http_request` needs the idempotency key; a struct also leaves room for
  future context (the exact reasoning that grew `StepContext`).
- **Verbatim tool output, no envelope.** Downstream steps template
  `output.status`/`output.body` directly; wrapping in `{tool, result}`
  would force `output.result.status` for no gain.
- **Empty allowlist = deny all.** A permissive default would make
  `http_request` an open SSRF vector the moment the tool ships; a
  deployment opts hosts in.

**Deferred / quirks.**

- IPv6-literal allowlist entries are out of scope (`splitHostPort` splits
  on the last colon); no fixture or deployment needs them yet.
- DNS-rebinding-grade SSRF defenses (re-resolving and pinning the IP
  between check and connect) are out of scope — the allowlist guards the
  hostname and every redirect hop, which is the ticket's SSRF bar.
- The multi-turn tool loop (model → tool_call → tool → model) is still
  unbuilt; 8.7 ships the `tool` step, not the agentic tool-use loop.

### 8.8 — Retrieval SPI & reference backend ✅

**Delivered.** The retrieval SPI (`internal/retrieval`), a Postgres
full-text reference backend (`internal/retrieval/pgfts`), a real `retrieve`
step executor replacing the **last** dev stub in place, a RAG-lite fixture,
and the plugin-SPI docs page. Migration 0013 adds the corpus table. With
this ticket every builtin ships at a real `1.0.0` — no dev stub remains,
and M8 is complete.

- **The SPI** (`internal/retrieval`, the fifth kind-owning leaf package —
  imports `plugin`+`dag`+stdlib, never exec/engine/**store**). `Retriever`
  = `Manifest()` + `Ingest(ctx, []Doc) error` + `Query(ctx, q, k)
  ([]ScoredDoc, error)`. `Doc` is `{id, content, metadata}` (id the upsert
  key, so re-ingesting a corpus is idempotent); `ScoredDoc` embeds `Doc`
  and adds `score` — the shape written into step output. `Ingest` is
  deliberately **off** the step path (steps only `Query`): it is
  corpus-loading code (tests, seeding, a future ingest API — not an
  endpoint in v1). `retrieval.Registry` is the plain typed facade over
  `plugin.Registry` (kind retriever) — register/get/manifests, **no**
  per-retriever schema compilation, because retrievers declare a nil
  `ConfigSchema` by decision: the `retrieve` step's config shape
  (`retriever`, `query`, `top_k`) is uniform and its schema lives on the
  executor. `*retrieval.Error{Class}` + `*UnknownRetrieverError` mirror
  `internal/tools`; secret hygiene is structural (no field can hold a query
  or content), and ctx errors are never wrapped.

- **The reference backend is a subpackage** (`internal/retrieval/pgfts`) —
  deliberately split from the SPI so `internal/retrieval` stays a leaf and
  only `pgfts` imports `store`. That split is also the point: `pgfts` is
  the worked example `docs/plugins.md` points at. It runs over Postgres
  full-text search — **zero new infrastructure**, the reason it is the
  reference: the corpus is one table in the same Postgres the engine
  already depends on. Migration 0013's `retrieval_docs` is
  `id`/`content`/`metadata`/timestamps plus a **functional GIN index** on
  `to_tsvector('english', content)` — no materialized tsvector column, so
  the sqlc row type stays scalar columns. `Query` ranks by `ts_rank`
  descending (id ascending as a deterministic tiebreak) via
  `websearch_to_tsquery`, which ANDs query terms and never errors on
  arbitrary input, so an empty/no-match query returns an empty slice, not a
  failure; a datastore error is transient, a ctx error passes through. The
  manifest is `(retriever, pg_fulltext, 1.0.0, cacheable-only)`. Store grew
  `RetrievalDocRepo` (Upsert/Query/Count) on `Querier`/`repos` +
  `queries/retrieval.sql`.

- **`exec.RetrieveExecutor`** replaces `StubRetrieveExecutor` in place
  (version `0.1.0-stub` → `1.0.0`, the M9 cache-bust trigger; flag
  unchanged — cacheable). It decodes the already-8.2-rendered
  `RetrieveConfig`, defaults `top_k` to 5 when absent and caps it at 100,
  resolves the retriever (unknown → permanent), runs one `Query`, and
  writes `{retriever, query, top_k, results}` where `results` is an array
  of `{id, content, score, metadata}` — **always present** (empty array on
  no match, never null) so `${{ steps.<id>.output.results }}` never misses.
  A `*retrieval.Error`'s class is honored via `ClassifiedError`; context
  errors pass through unwrapped; an empty rendered query and a negative
  `top_k` are deterministic ⇒ permanent. dag validation gained a
  `top_k >= 0` check (`config_field_invalid`; top_k is always an int
  literal — templating rewrites string values only — so it is a static
  check with no runtime carve-out).

- **Wiring.** `exec.Builtins`/`CoreBuiltins` grew a third
  `*retrieval.Registry` parameter (the 8.6/8.7 precedent; nil valid — a
  retrieve step then fails permanent at lookup). `cmd/worker` **always**
  builds `retrieval.NewRegistry(pgfts.New(st))` (no key, no toggle: it
  needs only the shared Postgres) and hands it to the exec registry;
  `cmd/api` folds the manifest into `GET /v1/plugins`. The OpenAPI spec
  already enumerated the `retriever` kind and made `config_schema` optional
  (8.1's forethought), so no spec change was needed.

- **RAG-lite fixture.** New canonical `examples/definitions/rag_lite.json`
  (corpus-pinned in `examples_test.go`): a `pg_fulltext` retrieve step
  feeds its ranked results into a mock-backed `llm` step via
  `${{ steps.search.output.results }}`, executed end-to-end against a
  seeded corpus in the engine integration suite — the answer text carries
  the seeded document ids (the citation proof). `fanout.json`'s retrieve
  step is now the real executor against an empty corpus, keeping the M8
  exit-criterion workflow offline-green on compose/CI.

- **Tests.** retrieval registry matrix (nil/wrong-kind/duplicate/unknown);
  `exec` retrieveexec suite (top_k default/cap/negative, empty-query
  permanent, nil-registry & unknown-retriever permanent, class + ctx
  passthrough, output shape incl. empty-array-not-null); pgfts integration
  (ingest+rank with more-matches-ranks-higher, top-k bound, no-match/empty
  query, re-ingest upsert with count unchanged, metadata round-trip,
  empty-field rejection); engine RAG-lite e2e + unknown-retriever
  dead-letter (source permanent); api plugins-listing for kind retriever;
  the migrate up/down round-trip bumped to latest version 13 (one Down
  drops `retrieval_docs`, 0012's `usage` column survives). `manifest_test`
  flag table updated (retrieve → 1.0.0), `stubTypes` now empty.

- **Docs.** New `docs/plugins.md` — the plugin-SPI guide (five kinds,
  capability flags, and a worked "writing a retriever plugin" walkthrough
  with `pgfts` as the exemplar), linked from `docs/README.md`. ADR-009
  gained the retrieval as-built section and updated flag table (retrieve
  1.0.0, `pg_fulltext` row, the last "(stub)" gone). ADR-006 gained the
  retrieval error taxonomy. The examples README describes `rag_lite.json`
  and `mock_pipeline.json`.

**Deferred / quirks.**

- **No ingest API endpoint** — `Ingest` is Go-level only (tests, seeding);
  an HTTP route to load a corpus is backlog. The ticket's acceptance only
  needs the integration test.
- **Single flat corpus, fixed `'english'`** — no namespace/collection
  column and no per-corpus language in v1. Both, plus the alternate
  backends (**pgvector**, external vector stores), are documented backlog
  plugins on this same SPI.
- **`builtinManifest`'s `version` parameter** now always receives `1.0.0`
  (the last non-1.0.0 caller, the retrieve stub, is gone), so `unparam`
  flags it; kept with a `//nolint:unparam` and a reason because the
  parameter is part of the manifest contract and a future dev stub carries
  a pre-release version there.
- **Pre-existing lint debt untouched:** `make lint` reports issues in
  `internal/tools/*` and `internal/engine/tool_integration_test.go` (from
  ticket 8.7 — errcheck on `w.Write`, revive unused-parameter, unparam on
  `transientf`); those files have zero diff in this ticket, so they are out
  of scope for 8.8. Every package 8.8 touched is lint-clean.

## Milestone 9 — Distributed rate limiting & response caching

### 9.1 — ADR-010 & resource limit configuration ✅

**Delivered.** The design for fleet-wide rate limiting and backpressure
(ADR-010) plus the config half it needs: the resource-limit
configuration is parsed and validated at worker boot. No migration, no
middleware, and no runtime behavior change — those land in 9.2/9.3. This
ticket decides the semantics the rest of M9 implements against and ships
the leaf package that loads the limits.

**ADR-010 (Accepted).** The decisions:

- **Named resources, keyed by the resolved provider.** A resource is a
  shared external-capacity pool named `<provider>:<identifier>`:
  `anthropic:<model>`, `openai:<model>`, `mock:<model>` — named by the
  provider 8.4's routing *resolves* the model to, so `model: "mock/sim-1"`
  binds to resource `mock:sim-1` and `model: "claude-sonnet-5"` to
  `anthropic:claude-sonnet-5` — plus custom `tool:<name>` resources. The
  limiter keys off what the provider meters, not how the author spelled
  the model.
- **Resolution: exact → `<provider>:*` wildcard → not-found = unlimited.**
  The unknown-resource policy is the deliberate mirror of 6.4's fail-open:
  limits are protective opt-in, and fail-closed (unknown = zero capacity)
  would brick the fleet the first time a workflow used a new model name
  before an operator added its limit. `*` is admissible in config only as
  a trailing `:*` segment.
- **Dual buckets per resource, acquired atomically.** Each resource
  carries up to two 6.3 token buckets — requests (cost 1) and tokens (cost
  = the pre-call estimate) — acquired **all-or-nothing in one two-key Lua
  script**. This is exactly the two-key script 6.3 deferred; M9 is where
  it stops being deferrable, because M9 denials are *steady-state under
  load* (not 6.4's rare abuse), so a sequential acquire's no-refund skew
  would systematically leak a request token on every token-bucket denial
  and drift the two ledgers. The script itself lands in 9.2 (the
  `ratelimit` library stays tenant-agnostic — the caller supplies both
  buckets and costs).
- **The `throttled` backpressure contract.** A denial is not a failure: it
  records a **second administrative attempt outcome, `throttled`**,
  alongside `lost` and likewise outside the ADR-006 error-class taxonomy.
  It is never counted against the retry budget (`CountCountedFailures`
  already counts `transient`/`timeout` only — *zero query change*), never
  in `retry_on`, and decided structurally by the middleware before the
  executor runs. It reuses 5.2's retry machinery **wholesale** — the
  claim-fenced `running → retrying` CAS (with `steps_failed` un-bumped),
  the `next_attempt_at ≤ now` claim guard, and the overdue-retrying
  reconciler scan (status-keyed, so it heals a throttle's commit-then-
  schedule gap with **no new reconciler duty**) — re-dispatched through the
  delayed ZSET under a new reason `throttle` (added by 9.2, as `retry`/
  `unpark`/`dlq_requeue` were added by their tickets). The worker slot is
  released immediately; nothing anywhere waits in-process for tokens.
- **Requeue math.** `delay = clamp(retry_after, floor, cap) + U[0,
  jitter_frac × clamped]`, defaults floor 500ms / cap 5m / jitter 20%.
  Deliberately *additive-partial* jitter, not ADR-006's AWS full jitter:
  `retry_after` is a real refill deadline, so `U[0, computed]` would wake a
  fan-out of siblings *before* the tokens exist, guaranteeing a second
  denial. The floor guards a hot requeue loop on a near-zero `retry_after`.
- **Wait-vs-never.** `ratelimit.ErrCostExceedsCapacity` (estimate > token
  bucket capacity) and `RetryAfterNever` (rate-zero bucket) are denials no
  wait can lift → force-classified `permanent`, dead-lettered — the 6.3
  contract edges consumed exactly as 6.3 anticipated ("M9 must perm-fail
  those instead of scheduling a delayed requeue").
- **The 9.2 executor hook.** A step binds to a resource + estimate through
  a new optional `ResourceClaimer` interface (designed in-ADR, implemented
  9.2); executors that don't implement it bypass the limiter. The estimator
  (chars/4 + declared `max_tokens`) and 9.3's post-call reconciliation are
  forward-pointed so those tickets build against decided semantics.
- **Fairness stance (documented, deferred to M19).** Global buckets +
  fire-time-ordered requeue let one huge fan-out starve a small
  concurrent run of the same resource. The remedy — a per-run throttle cap
  that parks the run under a new `resource_starved` reason (5.6's park
  primitive, which holds no lease or slot) — is deferred until load tests
  demand it, because the *hard* limit (the provider's budget) is always
  respected by the global bucket; only the throughput *distribution* is
  unfair, and only under contention.

**ADR-006 cross-update.** `throttled` added to the outcome-vocabulary
note (a second administrative outcome after `lost`); taxonomy **rows 16**
(limiter denial → `throttled`, delayed requeue, no budget, no DLQ) and
**17** (cost-exceeds-capacity / never-refilling → `permanent` → DLQ); the
retry-budget section notes the exclusion; the Enforcement-points section
gained an M9 line. The `throttled` CHECK-constraint migration is 9.2's
(the 5.1→5.2 design-then-migration split precedent).

**Config half (shipped in 9.1).**

- **`internal/limits`** — a leaf package (stdlib only; imports neither
  `ratelimit` nor any engine package, so the SPI/enforcement split holds).
  `Rate{PerMinute, Burst}` with `RefillPerSec()` (= per_minute/60) and
  `Capacity()` (= burst, or `ceil(per_minute)` when unset) mapping helpers
  the 9.2 middleware hands to `ratelimit.Bucket`; `Resource{Name, Requests,
  Tokens}`; `Set` with `Lookup` (exact → wildcard → not-found), `Names`
  (sorted copy), `Len`, all nil-safe. `Parse` does strict
  `DisallowUnknownFields` decode + a trailing-content check, then validates
  with **every error joined at once** (the config package's house style):
  name required / no whitespace / `*` only trailing `:*` / unique; at least
  one of requests|tokens; `per_minute` strictly positive and finite (no
  rate-zero fixed-quota buckets in v1 — a `RetryAfterNever` brick, exactly
  as 6.4 forbids rate-zero API buckets); `burst` non-negative. `Load(inline,
  file)` is inline **XOR** file **XOR** empty (= unlimited); the file read
  lives here so the config package stays env-pure.
- **`config.ResourcesConfig`** — `AGENTLOOM_RESOURCES` (inline JSON) /
  `AGENTLOOM_RESOURCES_FILE` (path), mutual exclusion rejected at
  `config.Load` (so a both-set mistake fails before any backend opens;
  `limits.Load` also rejects it defensively).
- **`cmd/worker`** loads the set via `limits.Load` at boot (a bad config
  fails boot, not the first throttled step) and logs the resource count +
  names, holding the set for 9.2's middleware. `cmd/api` is untouched — it
  never executes steps.
- **`.env.example`** documents both sources with a worked inline example.

**Non-obvious decisions.**

- **The two-key atomic script is promoted from "deferred" to
  "mandatory".** 6.3 and 6.4 both punted it as a micro-optimization; the
  real reason it matters is correctness under *sustained* denial, which is
  M9's normal operating regime, not a latency concern. Recorded in ADR-010
  and its alternatives.
- **`throttled` needs no store/query changes to be budget-exempt** — the
  budget counter was already scoped to `transient`/`timeout` since 5.2, so
  a third excluded outcome is free. This is why the whole backpressure path
  reuses 5.2/5.6 machinery with no new primitive.
- **No dead code in 9.1.** The `ResourceClaimer` hook and the two-key
  script are *described* in the ADR, not stubbed into `exec`/`ratelimit` —
  9.1 stays free of unused interfaces; `limits`'s `RefillPerSec`/`Capacity`
  helpers are the only forward-looking code, and they are pure, tested, and
  the natural home for the per-minute→per-second mapping the ADR pins.

**Tests.** `internal/limits` unit matrix: valid golden config,
exact/wildcard/unlimited resolution (incl. exact-wins, bare-name-never-
wildcards), `Rate` helpers (explicit burst vs. ceil default), every
validation-error path individually, all-errors-at-once, JSON overflow
rejection, `Load` inline/file/both/missing-file/empty/whitespace, invalid
inline, and nil-`Set` safety. `config`: resources overrides (default
zero-value, inline-only, file-only) and the mutual-exclusion load error.

**Verified.** `go build ./...`, `go test -race
./internal/limits/ ./internal/config/`, and the full `go test -race
./...` green; no integration tests — 9.1 crosses no process or datastore
boundary (both new surfaces are pure).

### 9.2 — Limiter middleware for executors ✅

Fleet-wide rate-limit enforcement as executor middleware: before a
cost-bearing executor's provider call the engine acquires the resource's
dual buckets; a denial defers the step (throttle → delayed requeue, slot
released, no counted attempt), an impossible request perm-fails, a Redis
error fails open. Everything conforms to ADR-010 as written in 9.1 — this
is the build.

**The two-key atomic acquire (`internal/ratelimit`).** `AcquireDual` +
`acquireDualScript` — 6.3's explicitly-deferred second Lua script, now
mandatory because M9 denials are steady-state. It refills both buckets
against one Redis `TIME`, grants only if **both** hold their cost, debits
both or neither, and on denial reports `retry_after = max` of the denying
buckets and a denied-dimension code (`DeniedRequests`/`Tokens`/`Both`).
Each bucket keeps the same `{tokens, ts}` hash as the single-key script
(absent = full, `%.17g` exact balance, TTL re-armed to time-to-full,
`PERSIST` for rate-zero, backwards-clock clamp); refill bookkeeping is
written for both even on a denial (advancing the clock mints nothing). The
drift property the script exists for — a token-dimension denial must **not**
debit the request bucket — is proven by an all-or-nothing concurrency stress
(the request ledger ends debited by exactly the grant count, no leaked
tokens). Test seam `AcquireDualAt` (injected clock) through
`export_test.go`, mirroring `AcquireAt`.

**The resource adapter (`internal/ratelimit/resource`).** A new subpackage
— the one place importing both `limits` and `ratelimit`, keeping each a leaf
(the `retrieval/pgfts` precedent). `resource.Limiter.Acquire(ctx,
resourceName, estTokens)` resolves the name against the configured `Set`
(exact → wildcard → **unlimited-skips-Redis**, ADR-010's opt-in policy) and
maps the resolved entry onto buckets via 9.1's `Rate.Capacity()` /
`RefillPerSec()`: both dimensions + `estTokens > 0` → `AcquireDual`; one
effective dimension (requests-only, tokens-nil, or a tool claim's
`estTokens == 0`) → single `Acquire`; a tokens-only resource with a zero
estimate meters nothing (unlimited). `ErrCostExceedsCapacity` passes through
typed; `RetryAfterNever` is re-exported for the engine. Keys
`<prefix>:<resource>:{requests,tokens}`.

**The `ResourceClaimer` hook (`internal/exec`).** ADR-010's interface
verbatim in `exec.go`. `LLMExecutor.ResourceClaim` resolves the model
through the `llm.Registry` and forms `<provider>:<model>` by the **resolved**
provider (so `mock/sim-1` → `mock:sim-1`), estimating chars/4 over the
rendered prompt/messages plus `max_tokens` (defaulted); a resolution failure
returns an error so the middleware skips limiting and lets `Execute` land the
one permanent classification. `ToolExecutor.ResourceClaim` returns
`tool:<name>` with `estTokens 0` (requests-only). Every other executor
doesn't implement the interface → structurally bypassed.

**The engine middleware.** `rateLimit` in `execute()` (after `renderConfig`,
before `runExecutor`; the `StepContext` is hoisted so the claim hook and the
executor share it): if the executor implements `ResourceClaimer` and a
limiter is wired, bind → `Acquire` → route. Granted / unlimited /
claim-binding-error / Redis-error(**fail-open**, `RateLimitFailOpen`
metric) all proceed to the executor; `ErrCostExceedsCapacity` or
`RetryAfterNever` → `completeFailure(..., ClassPermanent, ...)` → DLQ source
`permanent` (wait-vs-never); a genuine denial → `completeThrottle`.
`completeThrottle` (sibling of `completeFailure` in `complete.go`) computes
the delay via the pure `throttleDelay` (clamp(retry_after, floor, cap) +
**additive** jitter `U[0, frac×clamped]` — deliberately not `retryDelay`'s
full jitter, since retry_after is a real refill deadline and full jitter
would wake fanned-out siblings before tokens exist), then one transaction:
`LockRunStatus` (cancelling run → settle via `CancelRunningStep`) else
`ThrottleStep`; fence conflicts → `abandonFenced`; post-commit schedules the
delayed envelope (reason `throttle`, no `EnqueuedAt`) through the existing
`scheduler` seam, failures log-and-ACK (reconciler backstop). Seam:
`engine.ResourceLimiter` + `WithResourceLimiter`, `WithThrottleBackoff`;
nil limiter = every existing test layer untouched.

**Store (`ThrottleStep`, migration 0014).** `store.ThrottleStep` reuses
5.2's `RetryRunStep` query **verbatim** (the claim-fenced `running →
retrying` CAS, claim cleared, `next_attempt_at` stamped — ADR-010's "no new
store primitive") but records the attempt with the administrative outcome
`throttled` and appends `step_throttled` (payload: resource, bucket,
retry_after, next_attempt_at); like `RetryStep` it never bumps
`steps_failed`. `throttled` is not a counted class, so
`CountCountedFailures` ignores it — the retry budget is untouched (a step may
throttle any number of times; the physical `attempt_count` grows like it does
for `lost`, but the budget does not). Migration 0014 adds `throttled` to the
`step_attempts.outcome` CHECK (`run_events.type` is free-form TEXT — no DDL
for `step_throttled`). No claim-path or reconciler change: the claim CAS
already admits due retrying steps and the overdue-retrying scan already heals
a lost `throttle` schedule.

**Config / worker / obs.** `queue.ReasonThrottle`. `ResourcesConfig` grew
`KeyPrefix` (`AGENTLOOM_RESOURCES_KEY_PREFIX`) + `ThrottleFloor/Cap/JitterFrac`
(`AGENTLOOM_RESOURCES_THROTTLE_{FLOOR,CAP,JITTER_FRAC}`, validated
`0 < floor ≤ cap`, `jitter ∈ [0,1)`). `cmd/worker` builds `resource.New` over
the queue's Redis client only when `resourceLimits.Len() > 0` and wires the
two engine options. New obs subsystem `ratelimit`:
`engine_ratelimit_throttled_total{resource, bucket}`,
`engine_ratelimit_throttle_wait_seconds{resource}`,
`engine_ratelimit_fail_opens_total`; `resource` added to ADR-008's label
allowlist and `TestInstrumentConformance` exercises them. OpenAPI outcome
enum gained `throttled` (spec still lints 100/100).

**Non-obvious decisions.**
- **The step IS claimed when throttled.** The middleware runs after the
  claim (attempt row created), so a throttle closes that attempt as
  `throttled` and the physical `attempt_count` increments per throttle —
  exactly as `lost` does. "Attempt counter unchanged by throttles" (the
  acceptance criterion) means the **retry-budget** counter
  (`CountCountedFailures`), which `throttled` is excluded from — not the
  physical column. Each throttle re-dispatches a fresh delayed entry
  (delivery count resets), so no poison escalation.
- **Fail-open on a claim-binding error, not just a Redis error.** If
  `ResourceClaim` can't resolve the model, the middleware proceeds and lets
  `Execute` land the permanent failure — the routing judgment lives in one
  place, never duplicated.
- **The fleet test asserts the token-bucket cumulative bound, not a
  wall-clock rate.** `TestFleetRespectsResourceLimit` records every provider
  call's elapsed time and asserts `n ≤ burst + refill×elapsed + ε` for the
  n-th call — the exact invariant the shared bucket guarantees, deterministic
  regardless of scheduling, and the honest form of "N workers ≤ R calls/min".

**Tests.** `ratelimit`: dual-acquire integration matrix (both-grant exact
accounting, token-denial-leaves-request-ledger-untouched, `retry_after` max,
never-refill sentinel, cost-exceeds typed, all-or-nothing concurrency
stress). `resource`: unit (nil-client reject, unlimited-skips-Redis via an
unroutable client, nil-`Set`) + integration (exact/wildcard, dual dimension
with a token denial, tokens-only-zero-estimate, tool requests-only,
cost-exceeds). `exec`: `ResourceClaim` unit (llm resource+estimate+default,
routing failure, tool name, missing-tool). `engine`: `throttleDelay` unit
(clamp + additive jitter, table) + integration — headline throttle-defer-
and-complete on the fake clock (history `[throttled, succeeded]`, budget 0,
one `step_throttled` event, executor invoked once, next_attempt_at cleared),
the two perm-fail paths → DLQ permanent, fail-open, and the 4-worker fleet
bound. `config`: throttle-knob overrides + validation matrix. `store`:
migration round-trip bumped to 14 (the single-Down now reverts 0014's
CHECK — a constraint-only change — so 0013's `retrieval_docs` survives).

**Verified.** `go build ./...`, `go vet ./...`, `golangci-lint` clean,
`make openapi-lint` 100/100, full `go test ./...` green, and the full
`-tags integration` suite green against the compose stack
(`ratelimit`/`resource`/`store`/`exec`/`config`/`obs`/`engine`/`api`/
`cmd/worker`/`queue`). Requires migration 0014 on any long-lived database
(`make migrate-up`); the integration harness migrates each test DB itself.

### 9.3 — Token-cost reconciliation ✅

The M9 limiter debits a pre-call token *estimate* (chars/4 + declared
`max_tokens`); the real usage is known only after the provider responds. A
biased-low estimator would admit more real tokens than the provider's budget
on every call, and the error compounds under sustained load. 9.3 closes the
gap: after a granted, token-metered call returns, the middleware corrects the
token bucket by `delta = actual − estimate`. Everything conforms to ADR-010's
9.3 forward-pointers; this is the build. No migration, config, or store
change — reconciliation lives entirely in the `ratelimit` library, the
`resource` adapter, and the engine middleware.

**The third Lua script (`internal/ratelimit`).** `Adjust(ctx, Bucket, delta
int64) (remaining int64, err error)` + `adjustScript`, beside the single-key
`Acquire` and 9.2's two-key `AcquireDual`. It refills the bucket to the Redis
clock exactly as the acquire scripts do (same `{tokens, ts}` hash,
absent-key-=-full, backwards-clock clamp, `%.17g` exact state, TTL re-armed to
time-to-full / `PERSIST` for rate-zero), then applies `tokens -= delta`. The
**asymmetry is the whole design**:
- A **positive** delta (under-estimate — actual exceeded the estimate) is an
  extra debit, deliberately **unclamped**: it can drive the balance negative.
  A negative balance is the enforcement mechanism — the *unchanged* acquire
  scripts already deny while `cost > tokens` and grow `retry_after` as
  `(cost − tokens)/rate`, so subsequent acquires throttle until refill pays the
  debt back. No acquire-script change was needed.
- A **negative** delta (over-estimate) is a refund and **clamps at capacity** —
  a bucket is never fuller than full, mirroring the refill clamp.

The TTL rule composes with a negative balance for free: time-to-full
`(capacity − tokens)/rate` grows past a minute of refill when `tokens < 0`, so
the debt survives in Redis until it is actually paid off (an early expiry —
absent key = full — would erase it). `validate(cost)` split into
`validateFields()` (key/capacity/rate) so `Adjust` validates the bucket without
a positive-cost constraint (a reconciliation delta is signed). Test seam
`AdjustAt` in `export_test.go`.

**The adapter (`internal/ratelimit/resource`).** `Reconcile(ctx, name, est,
actual)` re-resolves the name (exact → wildcard → unlimited, as `Acquire` does)
and `Adjust`s **only the token bucket** — the requests cost of 1 is exact,
never reconciled. An unlimited resource, a resource with no token dimension, or
a zero delta reconciles nothing and touches no Redis. `Decision` grew
`TokensMetered` (true only on the dual path and the tokens-only single path)
so the engine reconciles exactly what it debited — never a requests-only or
unlimited grant.

**The engine middleware (`internal/engine`).** `rateLimit` now returns a
`*reconcileBinding` (the resource name to re-resolve, the resolved config-entry
label for the metric, and the estimate) on a **granted, token-metered**
acquire; nil otherwise. In `execute`, after the executor returns,
`reconcileTokens` runs iff `execErr == nil && out.Usage != nil && rc != nil`:
it observes `EstimateError(rc.resource, actual − est)` and calls
`limiter.Reconcile`, `actual = Usage.InputTokens + Usage.OutputTokens`. Placed
right after the `StepDuration` metric and before the completion transaction, so
it catches every success carrying usage — including a success that raced its
timeout (honored) or a cancellation (still spent the tokens) — while the
shutdown-abandon path (canceled handler ctx, returns before here) skips it,
matching its no-completion contract. An **errored call carries no usage**
(`out.Usage` nil on the error return), so its estimate stays debited —
deliberately conservative: a failed call's true spend is unknowable, and
leaving the estimate on the ledger protects the provider budget. Reconciliation
is **fail-open** (a `Reconcile` error logs + increments
`ReconcileFailure`, never touches the step outcome). The `ResourceLimiter` seam
grew `Reconcile`; the `Metrics` seam grew `EstimateError`/`ReconcileFailure`.

**Metrics (`internal/obs/metrics`).** `engine_ratelimit_estimate_error_tokens
{resource}` — a histogram of the signed error `actual − estimate` with
roughly-log-scaled buckets symmetric about zero (`±64…±65536`), so a
systematically biased estimator shows as a skewed distribution. This is the
**first `_tokens`-unit histogram**: `TestInstrumentConformance`'s histogram
rule widened from "`_seconds` only" to `{_seconds, _tokens}`, and ADR-008's
unit table was amended to match. `engine_ratelimit_reconcile_failures_total`
counts fail-open reconciliations. Both label by the resolved config-entry name
(the bounded `resource` label), never the raw `<provider>:<model>`.

**Non-obvious decisions.**
- **The debit is unclamped; the refund is not.** The negative balance IS the
  enforcement — it makes the *existing* acquire scripts throttle without any
  change to them. Clamping the debit at zero would silently discard the
  correction and re-open the drift the ticket exists to close.
- **Reconcile only real usage, and only what was metered.** No usage (errored
  call) → estimate stays debited (conservative). Not token-metered
  (requests-only / unlimited / tool claim) → nothing on the token ledger to
  correct. `TokensMetered` on the `Decision` carries the second fact from the
  adapter (which knows the config shape) to the engine (which does not).
- **Reconcile against the acquire, not the completion.** The provider call
  happened; a later fenced completion does not un-spend the tokens, and a
  zombie + its takeover each reconcile against their own acquire, so there is
  no double-count.
- **The fleet test asserts cumulative ACTUAL tokens, not est.**
  `TestFleetActualTokensRespectResourceLimit` uses a biased-3×-low estimator
  (est 10, actual 30) and asserts `Σactual ≤ burst + refill×elapsed +
  in-flight-slack` at every call — a bound that would be violated 3× over
  without reconciliation (the acquire gates on est=10, so the raw admission
  rate is 3× the token budget; the reconcile debit is what claws it back). The
  slack `workers×(actual−est) + actual` bounds the window between a call's
  estimate-debit (at acquire) and its shortfall-debit (at reconcile).

**Tests.** `ratelimit`: `Adjust` integration matrix (refund adds / clamps at
capacity; debit goes negative and the debt throttles a later acquire with a
correctly-grown `retry_after`, recovering after refill; refill-before-apply;
zero-delta no-op; rate-zero PERSIST; debt-TTL outlives time-to-full;
bucket validation) + the **conservation property test**
(`TestReconcileConservationProp`, `pgregory.net/rapid`) — random
`AcquireAt`/`AdjustAt` interleavings at non-decreasing injected times matched
against a pure-Go model's exact float64 balance at every step. `resource`:
no-op unit paths (unlimited / requests-only / zero delta skip Redis via the
unroutable-client trick) + integration debit/refund/wildcard with the requests
ledger proven untouched. `engine`: `TestReconcileAppliedAfterMeteredCall`
(exact est/actual passed, histogram observed the +50 delta),
`TestReconcileSkippedWhenTokensNotMetered`, `TestReconcileFailOpen`, and the
headline fleet test. `obs/metrics`: conformance sweep exercises the two new
instruments.

**Verified.** `go build ./...`, `go vet ./...` (incl. `-tags integration`),
`golangci-lint` clean on the changed packages, full `go test ./...` green, and
`-tags integration` green against the compose stack under `-race`
(`ratelimit`/`resource`/`engine`/`obs`). No migration — 9.3 adds no schema, so
no `make migrate-up` needed.

### 9.4 — ADR-011 & cache key builder ✅

M9's second mandated feature is a response cache, implemented (like the rate
limiter) as executor middleware. 9.4 is the **contract half** — the ADR, the
`cache` step-envelope field, and the key builder + policy — mirroring how 5.1
shipped `Step.Retry` before 5.2 built the retry engine, and 9.1 shipped
`internal/limits` before 9.2 built the limiter. **No migration, config, store,
engine, or exec production change lands in 9.4**; the Redis store, the
middleware, `config.CacheConfig`, any materialization onto `run_steps`, the
metrics, and the bust/stats ops surface are deferred to 9.5/9.6, called out as
forward slots in the ADR.

**ADR-011 (`docs/adr/011-response-cache.md`).** Records the whole design: the
key over length-prefix-framed components, the two-layer eligibility/default
policy (with the full step-type × determinism × capability-flags matrix
table), the three invalidation mechanisms (TTL, version-bump, admin
bust-by-prefix), and the storage rationale (write-through Redis, not Postgres,
with an oversized-value skip). The two required "why" items are explicit
sections: Redis because a cache is disposable derived data whose loss costs a
re-computation never correctness (so it must not live in the source of truth,
and the hot claim-path read wants Redis latency), and the size cap skips rather
than chunks or spills. Index row added to `docs/adr/README.md`; ADR-003's
templating section gained `cache` in the literals-only envelope-field list
(the field name was already reserved there for M9).

**The `cache` field (`internal/dag`).** A new `cachepolicy.go` beside
`retry.go`: `CacheMode` (`off`/`read_write`/`read_only`), `CacheScope`
(`global`/`run`), `CachePolicy{Mode,TTL,Scope}`, and `MaxCacheTTL` (30d, the
`MaxRunWallClock` ceiling — a cached AI output kept past a month is a staleness
hazard). `Step.Cache *CachePolicy` (nil = absent, engine default) after
`Timeout`, so canonical encoding gets the field for free (struct field order +
`omitempty`). `decodeCache` mirrors `decodeRetry` — `strictUnmarshal` plus
closed-enum checks for mode/scope at the codec level, the ttl bound and the
mode-required rule deferred to Validate. Two new validation codes,
`cache_field_required` (an empty `cache: {}` or one setting only ttl/scope is
ambiguous — mode is the field's whole point) and `cache_field_invalid` (ttl
parseable/positive/≤ MaxCacheTTL); `checkCache` follows `checkTimeout`. Schema:
`CacheMode`/`CacheScope` `JSONSchema()` enum methods, `CachePolicy`
auto-reflected into `$defs`, regenerated `docs/schema/workflow-definition.v1.json`.

**The leaf package (`internal/cache`).** Imports `internal/dag` +
`internal/plugin` + stdlib only — a true leaf (exec will import *it* in 9.5, no
cycle). `key.go`: `Key(KeyInput) (string, error)` = hex SHA-256 over ordered
components — `KeySchemaVersion` (the builder's own format version, the
fleet-wide invalidation lever), the definition `schema_version`, the scope +
run id, the **executor** plugin identity (llm/tool/retrieve), the **concrete
external** plugin identity (provider/tool/retriever), then the per-type request
body (`LLMRequest`/`ToolRequest`/`RetrieveRequest`, a closed `Request`
interface). `policy.go`: `Decide(caps, deterministic, *dag.CachePolicy,
defaultTTL) Decision` encoding the matrix — hard eligibility gate
(`Cacheable && !SideEffectful`, unopenable by any step mode), then a
determinism-driven default (deterministic → read_write, else bypass), then the
step `mode` override within eligibility; `Decision{}` (bypass) is the safe
zero value.

Non-obvious decisions:
- **Length-prefix framing, not `0x00` separators.** Each component is prefixed
  with its 8-byte big-endian length before hashing, so no component's bytes can
  be mistaken for a boundary (`("ab","")` ≠ `("a","b")`) — strictly stronger
  than the 6.5 idempotency fingerprint's delimiter scheme, chosen because a
  cache-key collision serves a *wrong output*, higher stakes than a fingerprint
  mismatch. `TestKeySeparatorUnambiguous` pins it.
- **`temperature` nil is distinct from `0`.** The `*float64` pointer (which
  exists in `dag.LLMConfig` precisely to survive canonical encoding) is keyed
  as the sentinel `"none"` when nil vs `strconv.FormatFloat` otherwise — nil is
  the provider default (non-deterministic), a genuinely different request than
  an explicit deterministic `0`. This is also the single most consequential
  policy edge: the default caches `temperature==0` but *not* nil.
- **JSON components canonicalized by round-trip through `any`.** Same technique
  as `store.canonicalJSON` (6.5): sorted keys, normalized number spelling
  (`1`/`1.0`/`1e0` → `1`), nil/empty → the `"null"` sentinel. Its documented
  limits (>2^53 int precision, duplicate-key collapse) are accepted — a
  rendered request that trips them is pathological and the cost is a miss, not
  a wrong hit.
- **Two plugin identities in the key.** The cacheable *executor* (its transform
  config→request→output can change) and the *concrete external plugin* (the
  provider/tool/retriever that actually serves the call) both bear on the
  output, so both are hashed; the concrete plugin also namespaces the Redis key
  (`<prefix>:v1:<kind>:<name>:<hash>`), the granularity 9.6 busts by.
- **Tool eligibility is the tool's, not the tool executor's.** The `tool`
  executor is `side_effectful` (worst case across tools), but a pure tool like
  `json_transform` is cacheable — so 9.5 will feed `Decide` the invoked tool's
  manifest flags, not the executor's. Documented in the ADR and in the
  cross-check test's comment.

**Tests.** `internal/cache`: golden-digest pins for llm/tool/retrieve inputs
(regression guard against format drift), the reordered-keys/renumbered-JSON
stability property, the semantic-change table (14 mutations each yielding a
distinct, mutually-non-colliding key incl. nil-vs-0 temperature), run-scope
isolation, the separator-ambiguity probe, the error matrix, and the RedisKey
layout; `policy_test.go` is the encoded matrix table. `internal/exec`'s
`manifest_test.go` gained `TestCachePolicyTracksCapabilityFlags`, feeding every
real builtin manifest through `cache.Decide` so a capability-flag drift breaks
the policy, not just the flag table. `internal/dag` corpus trail (the 5.3
`timeout` template): five `invalid/cache_*` codec fixtures (not-object,
unknown-field, bad-mode, bad-scope, ttl-wrong-type) + one
`invalid_structural/cache_bad_bounds.json` (missing mode, unparseable/negative/
over-ceiling ttl), wired into the decode/validate expectation maps (the
corpus-coverage tests force both); `TestDecodeCachePolicy` (full/partial/absent
on the fixture kitchen_sink); the examples `kitchen_sink.json` grew an opt-in
`cache` block on its retrieve step + the ticket-9.4 construct pin; schema-content
test pins the `CachePolicy`/`CacheMode`/`CacheScope` defs and a `read_write`
marker.

**Verified.** `go build ./...`, `make lint` (golangci-lint, 0 issues), full
`go test ./...` green, and `make generate` leaves the committed schema clean. No
integration-tagged tests — everything in 9.4 is offline. No migration/config
change, so no `make migrate-up` needed.

### 9.5 — Cache store & middleware ✅

9.5 is the runtime half of the response cache: the read-through/write-through
executor middleware built against 9.4's key builder and policy. It mirrors how
9.2/9.3 built the limiter runtime after 9.1's config — the cache reads a hit
**ahead of** the rate limiter (a hit skips the limiter and the provider
entirely) and writes a miss through on success.

**Materialization — migration 0015.** `run_steps.cache_policy` (nullable JSONB)
carries the step's authored `cache` block, materialized at instantiation like
`retry_policy` (0003) and `timeout` (0004): the middleware reads the effective
policy off the claimed row and never reparses the definition snapshot, and a
worker upgrade can't change an in-flight run's cache behavior. NULL means the
step authored no block (engine default decides). `queries/graph.sql`'s explicit
insert column lists grew `cache_policy`, sqlc regen carried it onto
`gen.RunStep`, `instantiate.go` marshals `step.Cache`, and the migrate
round-trip test bumped 14 → 15 with a `cache_policy`-dropped probe.

**The store — `internal/cache/redisstore`.** A thin byte-oriented Redis
key/value layer over `cache.RedisKey`: it owns the prefix and namespacing, the
value size cap, and the TTL on write. It deliberately does *not* know an
entry's shape — the engine marshals `{output, usage}` and hands opaque bytes,
which keeps `internal/cache` a true leaf (redisstore is its only go-redis
importer, the pgfts precedent) and the store free of any exec/engine
dependency. A Redis error or a corrupt value is a miss, never a failure.

**The executor hook — `exec.CacheBinder`.** Mirroring `ResourceClaimer`: the
llm, tool, and retrieve executors project the resolved request onto a
`CacheBinding` (the cacheable-executor identity, the concrete external-plugin
identity, the concrete plugin's capability flags, the determinism signal, the
request body). A binder error means "skip caching, let Execute classify" — the
routing judgment stays in one place. `exec.Usage` gained `CacheHit bool`.

**The middleware — `engine/cache.go`.** `cacheRead` runs in `execute()` ahead
of `rateLimit`; on a hit it calls `completeSuccess` with the cached output and
returns handled (no acquire, no executor). On a miss it returns a
`cacheWriteBinding` threaded to `cacheWrite` after a successful executor
return. `WithResponseCache(store, defaultTTL)` is the engine option; a nil
cache disables the middleware.

Non-obvious decisions:
- **`cache_hit` rides in the attempt's `usage` JSONB — no second migration.**
  0012 deliberately left `usage` an open object "for M10's provider-cache token
  counts without another migration." On a hit the engine completes with a usage
  marked `cache_hit=true` carrying the **counterfactual** counts the original
  miss recorded; the flag lives inside usage precisely because it marks that
  usage as not-spent. A non-llm hit records `cache_hit=true` with zero tokens.
  The API `usage` pass-through and the OpenAPI schema (open, no
  `additionalProperties: false`) needed only a doc touch-up (lint stays
  100/100).
- **The tool binding reports the *invoked tool's* flags, not the tool
  executor's.** The tool executor is `side_effectful` (worst case across
  tools), but `json_transform` the tool is cacheable — so the binder reads
  `tool.Manifest().Capabilities`, the ADR-011 rule made real. Proven in the
  binder unit test (`json_transform` cacheable/deterministic vs `http_request`
  neither) and end-to-end.
- **Write-before-commit.** The write-through happens after a successful
  executor return but before the completion transaction. Safe by construction:
  the entry is a pure function of the resolved request, so even a later-fenced
  completion wrote a *correct* entry. The shutdown-abandon path returns earlier
  and never writes.
- **`SchemaVersion` = `dag.CurrentSchemaVersion`.** Submit-time validation pins
  every stored snapshot to v1 (the only version), so a per-attempt DB read
  would provably return the constant; a comment flags that a future schema v2
  must thread the run's real version.
- **Every error path is fail-safe.** No binder, a declined policy, an
  unbuildable key, a corrupt materialized policy (unlike a corrupt timeout,
  which perm-fails — the cache is advisory), a corrupt stored entry, or a Redis
  read/write error all resolve to "execute uncached." The size-cap sentinel
  moved to `cache.ErrValueTooLarge` (the leaf) so the engine matches it without
  importing redisstore; an oversized value is a store-skip bypass.

**Metrics.** A new `cache` subsystem (ADR-008 amended: subsystem vocabulary,
the `plugin` label row, five inventory rows, the conformance test's allowlists
widened): `hits`/`misses`/`bypass`/`stores` counters labeled by `plugin`
(`<kind>:<name>`, the concrete provider/tool/retriever, bounded by the
compiled-in catalog) plus an unlabeled `fail_opens` counter.

**Config & wiring.** `config.CacheConfig`
(`AGENTLOOM_CACHE_{ENABLED,KEY_PREFIX,DEFAULT_TTL,MAX_VALUE_BYTES}`, enabled by
default — the worker already requires Redis and a default-on cache is what
makes identical deterministic steps hit with zero config; default TTL 24h
bounded by the 30-day ceiling; 1 MiB value cap). `cmd/worker` builds the
redisstore over the same coordination Redis the queue uses and wires
`engine.WithResponseCache`; `cmd/api` is untouched (the API never reads the
cache — ADR-002's Redis independence holds). `.env.example` documents the four
vars.

**Tests.** redisstore integration (round-trip, TTL applied via PTTL,
oversize-skip with the typed error, namespacing/no-collision); `exec`
binder-unit matrix (resolved model + defaulted max_tokens + nil-vs-0
temperature, tool-level flags, retrieve non-determinism, error paths mirroring
`ResourceClaim`); engine pure-helper unit tests (`decodeCachePolicy`,
`decodeCacheEntry`, `markCacheHit`) + the headline integration suite over a
temperature=0 mock-llm definition run twice — the second is a hit (exactly 1
provider Chat call, exactly 1 limiter acquire via a recording granting limiter,
`cache_hit=true` counterfactual usage on the second attempt matching the
first's counts, byte-identical served output) — plus temperature>0 default
bypass (2 calls), the `read_write` opt-in override (1 call, hit), and `mode:
off` disabling a default-cached step (2 calls).

**Verified.** `go build ./...`, `make lint` (0 issues), full `go test ./...`
green, `make openapi-lint` 100/100, `make generate` leaves derived artifacts
clean; the redisstore, engine cache, migrate round-trip, and full engine
integration suites green against the compose stack. `storetest.NewDB` applies
all embedded migrations per test, so the integration suites needed no manual
`make migrate-up`; a live `make up-app` deployment does (0015 is new). Deferred
to 9.6: bust-by-prefix + stats endpoints, `ctl cache`, the ops runbook.

### 9.6 — Cache invalidation & ops surface ✅

9.6 closes M9 with the operator-facing half of ADR-011's invalidation strategy:
the admin bust, the stats endpoint, and `ctl cache`, built entirely on 9.5's
Redis store and the `cache.RedisKey` namespacing. **No migration, no engine
change, no new config var** — the invalidation levers were designed into the
key layout in 9.4, and the store's counters ride inside the store the worker
already wires.

**The `internal/cache` leaf (`bust.go`).** `BustMatch{Kind, Name}` with three
valid shapes (all / one kind / one concrete plugin; a name without a kind is an
error) projects through `BustPattern` onto a Redis glob mirroring `RedisKey`:
`<prefix>:v*[:<kind>[:<name>]]:*`. Two deliberate properties: it matches `v*`
across every `KeySchemaVersion` (so a bust also reclaims entries stranded behind
an earlier key format, which would otherwise only TTL out), and every literal
segment (the operator-supplied prefix, the plugin name) is glob-escaped so a
metacharacter can't widen the match. The stats namespace (`<prefix>:stats:…`)
is excluded by construction — it doesn't begin with `v` — so no bust can erase
the counters. Stats helpers round out the file: `StatsRedisKey`/`StatsPattern`/
`ParseStatsKey` (kind is the first tail segment; the rest is the name, recovered
verbatim), `NewPluginStats` (computes the hit rate, 0 when no lookups), and
`ParseCounter` (empty → 0, malformed → error). A unit `globMatch` proves the
stats-exclusion property without a Redis round-trip.

**The store (`redisstore`).** Two responsibilities added to the thin KV:
- **Durable per-plugin counters.** `Get` best-effort-increments `hits` or
  `misses`, `Set` increments `stores`, in a `<prefix>:stats:<kind>:<name>` hash
  via one pipelined `HINCRBY` + `PEXPIRE` (a 30-day TTL re-armed on every
  update, so an active plugin's counters never expire and an idle plugin's
  self-clean, mirroring the 6.3 bucket TTL). The increments are swallowed on
  error — stats are pure observability and must never gate a read, a write, or
  a step. **They exist because the engine's `engine_cache_*` Prometheus counters
  are per-worker and per-process**, so the API (which never runs a step) cannot
  read them; the store is the cross-process source. They match the Prometheus
  counters one-for-one on the normal path (a store op ⇒ an engine count), which
  is what makes reconciliation exact. The one documented divergence: a corrupt
  stored value is a store-hit (a value was present) but an engine fail-open
  (couldn't decode it). `bypass`/`fail_opens` stay Prometheus-only.
- **`Bust` and `Stats`.** `Bust(BustMatch)` runs `SCAN COUNT 512` + batched
  non-blocking `UNLINK`, returning the deleted count (`UNLINK` is idempotent, so
  a SCAN duplicate never inflates it); semantics are point-in-time — an entry a
  live worker writes after the scan passed its slot survives. `Stats()` SCANs
  the stats namespace, dedups keys across passes, `HGETALL`s each, and returns
  `[]PluginStats` sorted by (kind, name).

**The API.** A `CacheOps` seam (`Bust`/`Stats`, satisfied by `*redisstore.Store`;
the api package imports only the `cache` leaf, never go-redis) + `WithCacheOps`,
nil → the routes answer **503 `cache_unavailable`** (new error code). Two
**admin-scoped, admin-rate-limit-class** routes: `POST /v1/cache/bust`
(strict-decoded `{plugin_kind?, plugin_name?}`, kind validated to the three
cacheable kinds — model_provider/tool/retriever — a name-without-kind or a
non-cacheable kind is a 400 that never reaches the store; response `{deleted}`;
a structured **audit line** `msg="cache bust"` carrying `action`, the namespace,
`deleted`, and the actor `key_id` from the 6.2 auth stamp) and
`GET /v1/cache/stats` (`{plugins:[{kind,name,hits,misses,stores,hit_rate}]}`).
Route→scope and route→class tables, the `chi.Walk` coverage tests, the auth
matrix (the cache rows use 503 as the past-the-gate success marker, the
requeue-409 precedent — `keysServer` wires no CacheOps), and OpenAPI (two paths,
four schemas, the `CacheUnavailable` response, the error-code enum, a `Cache`
tag; still lints 100/100) all extended.

**Wiring & CLI.** `cmd/api` now builds **one** Redis client shared by rate
limiting and the cache ops surface (opened when either is enabled, fail-soft,
ADR-002 intact — the API never reads a cached *result* or dispatches), passing
`WithCacheOps` when `AGENTLOOM_CACHE_ENABLED`. `ctl cache bust [--kind K]
[--name N]` (no flags = bust all) and `ctl cache stats` (tabwriter table with
hit rates) mirror the endpoints via new client methods.

**Tests.** `internal/cache` pattern/stats-key unit matrix; `redisstore`
integration for the counters (a hit/miss/store bump their own fields, per-plugin
isolation, computed hit rate), the three bust granularities (one plugin / one
kind / all, each leaving non-matching entries + **all stats** intact), and the
acceptance **under-load** test — 3000 keys per namespace, 8 concurrent Get/Set
goroutines running throughout, bust one namespace, assert zero errors on the
live ops, the exact deleted count, and the other namespace fully intact; api
behavioral suite (namespace→BustMatch mapping, audit-log `key_id`, stats
projection, the 400 matrix never reaching the store, 503-when-unwired); the
headline engine-layer e2e **`TestCacheStatsReconcileAndBust`** — a real API
handler over the same store + cache store as a metered engine: two temperature=0
runs land `engine_cache_{hits,misses,stores}` at 1/1/1, the stats endpoint's
numbers **equal the gathered Prometheus counters exactly**, then an admin bust
forces the third identical run to miss and re-call the provider (calls 1→2), the
counters surviving the bust (misses/stores 2/2); `ctl` unit tests over httptest.

**Deferred / quirks.**
- **No per-run bust.** A run-scoped entry mixes the run id into the entry
  *hash*, not the Redis key, so bust granularity stops at the concrete plugin —
  a run-scoped entry's only bound is its TTL. Documented in the ADR and runbook,
  not a gap (a `definition`-wide scope was already deferred in 9.4).
- **Audit is a log line, not a table.** The events table is run-scoped and no
  global audit table exists; the acceptance is "audit-logged with actor key ID",
  and 6.1/6.2 established the captured-slog assertion pattern. A durable audit
  table is future work if an ops requirement demands it.
- **Stats counter divergence under tampering.** A corrupt stored value diverges
  store-hits from engine-hits by 1 (see above) — acceptable, since it only
  arises from operator tampering or a version skew that the reconciliation test
  never exercises.
- **The API's Redis use widened again.** ADR-002's "no Redis for the API" became
  "no Redis for *dispatch*" in 6.4 (rate-limit buckets); 9.6 reuses that same
  opt-in fail-soft client for the cache ops surface. The API still never reads a
  cached result. Recorded in the ADR-011 amendment and the `CacheConfig` doc.

**Verified.** `go build ./...`, `make lint` (0 issues), full `go test ./...`
green, `make openapi-lint` 100/100; the `internal/cache`, `redisstore`, api, and
engine cache integration suites green against the compose stack (the shared-Redis
throttle test `TestFleetRespectsResourceLimit` is an unrelated pre-existing
parallel-contention flake — passes on isolation and rerun). **M9 complete.** The
new `docs/ops-runbook.md` is the invalidation decision guide (TTL / version bump
/ admin bust), linked from `docs/README.md`; ADR-011 gained the "Invalidation &
ops surface (as built, 9.6)" section, ADR-007 the two route rows, `docs/api.md`
the walkthroughs.

## Milestone 10 — Cost tracking & budget enforcement

### 10.1 — ADR-012 & pricing catalog ✅

M10's differentiator is a real-time cost ledger that sits *inside* the scheduler
path (checked at claim time), not a reporting afterthought. 10.1 is the
**contract half** — the ADR plus the `internal/cost` leaf and `config.CostConfig`
— exactly as 9.1 shipped `internal/limits` before 9.2's limiter and 9.4 shipped
the cache key builder before 9.5's middleware. **No migration, no store change,
no engine/executor runtime change**; the `cost_ledger`, the attempt-completion
pricing, the claim-time budget check, downgrade chains, the physical event
append, and the cost metrics are all 10.2–10.5.

**ADR-012 (`docs/adr/012-cost-model.md`).** Records the whole cost model: the
representation decision (integer **nano-USD**), the pricing-catalog format and
resolution, the attribution rules with worked examples, the estimation approach,
the unknown-model policy, and the budget-semantics vocabulary. Index row added
to `docs/adr/README.md`.

**Money is integer nano-USD.** Every computed/stored cost is an `int64` count of
nano-USD (1 USD = 1e9, `cost.NanoPerUSD`), rounded half-away-from-zero per
component. The reason is 10.2's requirement that a run aggregate equal the exact
sum of its ledger rows under concurrent completions: integer addition is
associative and exact, floating dollars are neither. Catalog rates are *authored*
as decimal $/1M-token numbers for legibility and converted to nano at pricing
time; display USD is derived at the API edge. `int64` nano-USD saturates near
$9.2 × 10⁹, far beyond any real run.

**The pricing catalog keys off the ADR-010 resource name.** A model entry is
keyed `<resolved-provider>:<model>` — the *same string the limiter uses*, so a
step's model `mock/sim-1` binds to resource `mock:sim-1` for both rate limiting
and pricing, and cost attribution needs no new identity plumbing. `internal/cost`
(stdlib only, imports no other agentloom package — the cache/limits precedent, so
engine/exec/store can later depend on it without a cycle):

- `catalog.go` — `Catalog`, `Parse` (strict decode + all-errors-joined
  validation: `schema_version == 1`, resource-name rules with the model/tool
  namespace split — model names may not carry `tool:`, tool names must — non-
  negative finite rates, required `effective_from`, duplicate `(name, date)`
  rejected), `Load(inline, file)` (mutual-exclusion, merge onto embedded
  defaults), `Merge` (whole-list-per-name replacement + fallback override, copies
  before overlaying so the shared `Default()` is never mutated), and the `Date`
  type (day-granularity UTC, `YYYY-MM-DD`).
- `defaults.json` + `defaults.go` — the embedded default catalog (`//go:embed`,
  parsed once and cached), illustrative Anthropic/OpenAI list prices plus a
  `mock:*` wildcard at synthetic rates so the offline M10 exit workflow shows
  reproducible cost with zero configuration.
- `lookup.go` — `Lookup`/`PriceModel`/`ToolPrice` with **effective-date
  selection** (newest entry whose `effective_from ≤ at`; a scheduled reprice is a
  *second* entry, so old runs keep their rate), exact → `<provider>:*` wildcard →
  not-found resolution (mirroring `limits.Lookup`), `Source` (exact/wildcard/
  fallback) recording rate provenance for the 10.2 ledger, and the
  `UnknownModelPolicy` enum + `*UnknownModelError`.
- `price.go` — the pure arithmetic: `Cost` (input×rate + output×rate), `Estimate`
  (the pre-flight upper bound: estimated input + `max_tokens` priced as output),
  `ToolCost`, all integer nano-USD with half-up rounding and an overflow-bound
  note.
- `event.go` — `EventTypeUnknownModel = "cost_unknown_model"` + the
  `UnknownModelWarning` payload (model + fallback rate); the *physical*
  `Events().Append` lands in 10.2's attempt-completion tx (no ledger tx to hang a
  per-run event on yet; `events.type` is free-form TEXT so no migration then
  either).

**Attribution rules** (enumerated in the ADR with worked examples): LLM attempt
= `input×input_rate + output×output_rate` at the *actually-served* model; cache
hit = $0 actual + a counterfactual "saved" figure priced from the
`Usage.CacheHit` snapshot; priced tool = flat `per_call_usd` (absent entry =
free, tools not being unknown-model-policy subjects); judge/summarization
overhead attributed to the serving step, flagged `overhead`; no-usage attempts
(throttled/`lost`/errored) ledger nothing. Provider-side prompt-cache token
pricing is explicitly deferred (`llm.Usage` untouched).

**Unknown-model policy** is configurable (`AGENTLOOM_COST_UNKNOWN_MODEL_POLICY`)
and split by design: `estimate` (default) prices at the catalog `fallback` +
emits the warning event; `fail` refuses. The subtlety recorded in the ADR and
enforced by the `PriceModel(name, at, policy)` signature: **fail-closed governs
pre-flight only** (10.3 blocks an unpriced model *before* spend, permanent to the
DLQ), while **post-call ledger pricing always succeeds** (10.2 always passes
`PolicyEstimate` — the money is spent, and refusing to price it would understate
real spend).

**`config.CostConfig`** — `Inline`/`File` (`AGENTLOOM_PRICING[_FILE]`, mutually
exclusive, checked at config load *and* in `cost.Load`) + `UnknownModelPolicy`
(`estimate`|`fail`, default estimate, validated at load). `config` stays a leaf:
it carries the policy *string* and duplicates the two-value check rather than
importing `internal/cost`, exactly as it does for `internal/limits`; cmd/worker
maps the string onto cost's typed enum at the 10.3 claim-time check.

**`cmd/worker`** boot-loads-and-logs the catalog (`cost.Load`, a malformed
override fails boot) — model/tool counts, override source, policy — with no
runtime consumer yet (10.2 hands it to the engine). `cmd/api` untouched (it reads
ledger rows from Postgres in 10.2; it never prices).

**Non-obvious decisions.** (1) Nano-USD integers over Postgres `NUMERIC` — exact
and native, no decimal dependency, USD is a trivial edge conversion. (2)
Merge-by-name (whole-list-per-name replacement) over whole-catalog-replace — an
operator adds one private model without copying the default list, and can't
accidentally un-price a model by omission (which would let it run at $0). (3)
Resource-name keying reuses the limiter's string — one convention documents both
scheduler-path features. (4) The `cost_unknown_model` event's payload contract is
fixed now; the append is 10.2's, so no migration this ticket.

**Verified.** `go build ./...`, `make lint` (0 issues on the touched packages),
`go test ./internal/cost/ ./internal/config/ ./cmd/worker/` green, `internal/cost`
race-clean at 87.6% coverage. Tests: the parse/validate matrix, effective-date
selection (boundary-inclusive, between-dates, all-future, wildcard's own date),
merge semantics (replace-by-name, fallback override, base-not-mutated), the
embedded-defaults sanity guard (parses, has fallback, prices `mock:*`), the
unknown-model policy matrix (estimate → fallback+warning, fail → typed error,
no-fallback → typed error), arithmetic goldens incl. a genuine half-up case and
the exact-sum property, and the `config` env matrix (defaults, overrides, bad
policy, mutual exclusion).

### 10.2 — Cost ledger & aggregation ✅

The runtime half of the cost model: a `cost_ledger` row per cost-bearing
attempt, priced against 10.1's catalog and written in the same transaction as
the success CAS, with run-level aggregates and a cost API on top. Money stays
integer nano-USD end to end, so a run's materialized spend equals the exact sum
of its ledger rows — proven under concurrent completions.

**The resource channel — `exec.Output.Resource`.** ADR-012 attributes cost by
the ADR-010 resource name, but the *resolved* provider is known only inside the
executor, so `exec.Output` gained a `Resource` field (alongside `Usage`, the
same shape). The llm executor sets `<provider>:<served-model>` — the model that
*actually served* the call (`resp.Model`, so a served-model-id variant a step
never named prices via the provider wildcard, and 10.4's downgrade will price
the real model); the tool executor sets `tool:<name>`; every other step type
leaves it empty (not cost-bearing). The engine's cache middleware carries it
across a hit via a new `cost_resource` field on the stored `cacheEntry`
(`omitempty`, so a pre-10.2 entry decodes to `""` and simply ledgers no saved
figure — the hit still serves).

**Pricing is a pure pre-transaction function.** `engine/cost.go`'s
`priceAttempt` reads no database: given the attempt's resource, usage, and the
catalog effective at completion time, it returns a `*store.AttemptCostArgs` or
nil when the attempt ledgers nothing (pricing disabled, no resource, an
unpriced/free tool, or a model attempt with no usage). It always passes
`cost.PolicyEstimate` — post-call pricing never fails a succeeded attempt — so
an unknown model is priced at the catalog fallback with the `cost_unknown_model`
warning attached, **except on a cache hit** (which spent nothing, and whose
original miss already warned). `complete.go` computes the row before the
completion transaction and calls `store.ApplyAttemptCost` *after* `SucceedStep`
landed (a fenced zombie completion returns before it, so it never ledgers),
under the run lock the CAS already holds — which is what makes the aggregate
bump serialize with every other run mutation and the exact-sum property hold.

**Migration 0016.** `cost_ledger` keyed `(run_id, step_id, attempt, entry)`:
`entry` discriminates the charge kind (`attempt` in 10.2; ADR-012 rule 4's
`judge`/`compaction` overhead rows are the same-attempt slot M11/M12 fill
without a schema change — so `entry` is in the PK now). Columns: `resource`,
`usage` (JSONB token snapshot, NULL for tool rows), `rate` (JSONB rate
snapshot for auditability), `rate_source` (`exact|wildcard|fallback`),
`cache_hit`, `overhead`, `cost_nano_usd` (≥0), `saved_nano_usd` (≥0),
`created_at`. The claim-fenced success CAS lands at most one attempt-completion
per attempt, so the productive row never conflicts. Run aggregates are two
scalar `BIGINT` columns on `runs` (`spent_nano_usd`, `saved_nano_usd`) bumped
in the same transaction. **By-step and by-model breakdowns are read-time
`GROUP BY` over the ledger, not materialized** — the rows commit in the same
transaction as the aggregate, so a read-time group is exactly consistent with
zero write contention (the deliberate alternative to per-model JSONB on the run
row).

**Store surface.** `store.ApplyAttemptCost` (transition-style, `ErrNoTx`-
guarded, called inside the completion tx) inserts the row, bumps the aggregate,
and appends `cost_unknown_model` when a warning rides along; `store.CostRepo`
(`Ledger()`) reads the ledger and its two breakdowns for the API and the
property test's independent sum. `store.EventCostUnknownModel` joins the event
vocabulary (mirroring `cost.EventTypeUnknownModel`).

**API.** `GET /v1/runs/{id}` gained a `cost` summary on the run view;
`GET /v1/runs/{id}/cost` (read scope/class) returns the summary plus `by_step`,
`by_resource` (per-model / per-tool with token sums), and the full per-attempt
`entries`. Money is integer nano-USD on the wire; the `*_usd` strings are
rendered by exact integer division (`nanoUSDString`, never float). `openapi.yaml`
gained the path + `RunCostResponse`/`CostSummaryView`/`CostByStepView`/
`CostByResourceView`/`CostEntryView` schemas + `cost` on `RunView` (route-scope,
rate-class, and route-coverage tables extended; spec still lints 100/100).
`cmd/worker` wires `engine.WithPricing(pricing)` from the boot-loaded catalog;
`cmd/api` is untouched (it only reads ledger rows).

**Attribution corners realized.** Cache hit → $0 row with `cache_hit=true` and
the counterfactual `saved_nano_usd`; priced tool → flat row (rate snapshot is
the per-call nano cost); **unpriced tool → no row** (free, ADR-012 rule 3 —
keeps the ledger from a row per `json_transform`); no-usage or non-cost-bearing
attempt → nothing; `overhead` always false (the flag and PK slot are pre-wired
for M11/M12).

**Non-obvious decisions.** (1) `exec.Output.Resource` rather than a new hook
interface — the executor already computes the resource for rate limiting, and
lifting it onto the output mirrors how `Usage` already flows. (2) Scalar
aggregate + read-time breakdowns rather than materialized per-model JSONB —
exact consistency for free (same tx), no write contention, no drift. (3)
Columns named with the nano unit (`spent_nano_usd`), deviating from the
roadmap's shorthand `spent_usd`, per ADR-008's units-in-name convention; USD is
a display-only derivation at the wire edge. (4) The `entry` discriminator is in
the PK from the start so M11/M12 overhead rows never force a PK rebuild. (5)
Cache hits stay silent on unknown models — the money wasn't spent and the miss
already warned.

**Verified.** `go build ./...`, `bin/golangci-lint fmt`, `make lint`
(0 issues), `make openapi-lint` (100/100), all unit tests green, and the full
integration suites for `store`/`engine`/`api` green against the compose stack
(migration bumped to 16). New tests: the concurrent-fan-out exact-sum property
test (`TestCostLedgerExactSumUnderConcurrency` — 8 mock llm leaves, 4 workers,
`run.spent_nano_usd == SumByRun == Σ Cost(usage, mock-rate)`), the two-run
cache-hit saved test, the unknown-model fallback+event test, the `priceAttempt`
unit matrix (nil catalog / no resource / llm / cache-hit / no-usage / unknown /
priced tool / free tool), the `ApplyAttemptCost` store test (aggregate == sum,
event append, `ErrNoTx` guard), and the API `/cost` per-step/per-model contract
test (plus zero-summary and 404 cases).

### 10.3 — Budget enforcement at claim time ✅

Delivered the enforcement half of the cost model: budgets checked *inside* the
scheduler path, at the same point as the readiness/claim decision, before any
money is spent — a run parks (or a step fails) exactly at its cap, and raising
the budget + unparking resumes it.

**The contract (`internal/dag`).** Run-level `budget_usd` (a positive USD
number; `*float64`, nil = unbudgeted, encoded/decoded like the existing
`Temperature` pointer) and `on_budget_exceeded` (`park` the resumable default |
`fail`, mirroring `on_failure`); step-envelope `budget: {max_usd?, max_tokens?}`
(a `*StepBudget` like `*CachePolicy`). New validation codes
`budget_field_required` / `budget_field_invalid`: `budget_usd` positive,
`on_budget_exceeded` a no-op without a budget (rejected — a can't-fire config is
an authoring mistake), the step block needs at least one cap, `max_usd`
positive, and **`max_tokens` restricted to llm steps** (a token cap on a
non-metered step can never fire). New `decodeFloat` helper (the first top-level
number field). Kitchen-sink fixtures (both copies) + construct pins + decode/
validate unit tables + regenerated JSON Schema.

**Materialization & migration 0017.** `runs.budget_nano_usd` (nullable
`BIGINT` — NULL is unbudgeted, deliberately distinct from 0, which would park
every cost-bearing step) and `runs.on_budget_exceeded` (`NOT NULL DEFAULT
'park'`, CHECK `park|fail`); `run_steps.budget_policy` JSONB (the caps, read off
the claimed row like `cache_policy`, never reparsed); and the
`step_attempts.outcome` CHECK extended with `budget_exceeded` (the 0014
drop/re-add recipe). Instantiation converts `budget_usd` → nano with a local
`usdToNanoUSD` matching `cost.USDToNano` (the store deliberately does not import
`internal/cost` — it holds computed nano, never prices; the trivial conversion
is the accepted leaf-boundary duplication). `runRepo.Create` defaults an empty
`on_budget_exceeded` to `park` (mirroring the existing `on_failure` default), so
the many test builders that hand-build `CreateRunParams` need no change. Migrate
round-trip pinned to 17.

**The estimate hook (`internal/exec`).** New optional `exec.CostEstimator`
returning `CostEstimate{Resource, InputTokens, MaxTokens}` — the input/output
token split ADR-012's upper-bound `Estimate` needs (input priced at the model's
input rate, `max_tokens` at its output rate). The llm executor implements it via
the refactored `estimateLLMInputTokens` / `estimateLLMMaxTokens` (the old fused
`estimateLLMTokens` now sums them, so the limiter's `ResourceClaim` is
unchanged); resource is `<resolved-provider>:<routed-model>` (the ledger later
keys the *served* model — a documented pre-flight/ledger resource asymmetry).
The tool executor returns `tool:<name>` with zero tokens (a flat-priced call).
Estimator errors route like `ResourceClaim`'s: skip the check, defer to
Execute.

**The middleware (`engine/budget.go`).** `budgetCheck` sits in `execute()`
**after the cache read and before the rate limiter** — a deliberate deviation
from the nominal cache→limit→budget order (recorded in ADR-012 and the CLAUDE.md
middleware-chain line): a hit is $0 and never budget-gated, and parking after a
limiter acquire would strand debited tokens 9.3 only reconciles on success. It
is a no-op (no reads) when pricing is disabled, the executor is not a
`CostEstimator`, or the run is unbudgeted *and* the step is uncapped (the fast
path — the common case costs nothing extra). The pure `budgetDecide` (unit-
matrix-tested) prices the estimate (`estimateNanoUSD`: tool flat-or-free, model
`cost.Estimate` under the engine's `WithUnknownModelPolicy`) and routes in
order: step `max_tokens` over cap → permanent fail (oversized request never
sent); unknown model under fail-closed → permanent fail before spend; step
`max_usd` (cumulative `SumByStep` ledger read + estimate) over cap → permanent
fail; run budget (`origin.SpentNanoUSD + estimate`) over budget → park or fail
per disposition. A `$0` estimate (free tool) never triggers a run park (avoids
an unpark→park bounce). The decision uses the **claim-time spend on the
`ClaimOrigin`** (read under the run lock the claim already took — extended
`LockRun` to return `budget_nano_usd` / `spent_nano_usd` / `on_budget_exceeded`
alongside the trace context): exact for a sequential chain, conservative-low for
concurrent fan-out (spend only rises after the read, so it never over-parks) —
the accepted bounded-overshoot tolerance.

**Park vs fail.** Park is atomic in `store.BudgetParkStep`: the
`BudgetParkRunStep` CAS releases the step `running → ready` (claim cleared,
fenced on the caller's claim), the attempt closes with the administrative
outcome `budget_exceeded` (uncounted — the step never ran, like `lost` /
`throttled`), the `budget_exceeded` event lands with the full projection detail,
and the run parks with reason `budget_exceeded` — but **only when the run is
still `running` under the lock**, so concurrent fan-out siblings each release
without a duplicate `run_parked`. The released step is `ready` (not `retrying`),
so `Unpark` re-dispatches it with **zero reconciler dependency** (unlike
throttle). The completion follows the throttle route's cancelling-run
settlement. Fail reuses `completeFailure(permanent, "budget_exceeded: …")` — the
existing DLQ + `on_failure` disposition — so the descriptive dead-letter is the
record and the fail path needs no new event or write-off logic.

**Resume & API.** `engine.Control.SetBudget` (registry-free, `store.SetRunBudget`
under the run lock, guarded to non-terminal runs, `run_budget_updated` event, no
dispatch nudge — ADR-002); `PATCH /v1/runs/{id}/budget` (submit scope/class,
`DisallowUnknownFields`, positive-required → 400, terminal → 409) wired through
the four route tables + OpenAPI (path + `SetBudgetRequest`/`SetBudgetResponse` +
`CostSummaryView` budget fields; still lint 100/100) + auth/class matrices;
`CostSummaryView` (run view + `/cost`) grew `budget_nano_usd` / `budget_usd`
(nullable) + `on_budget_exceeded`; `ctl budget <run-id> <usd>` mirrors it.
`cmd/worker` maps `cfg.Cost.UnknownModelPolicy` onto `cost.PolicyEstimate|Fail`
via the new `cost.ParseUnknownModelPolicy` and wires `WithUnknownModelPolicy`
(the config value that was loaded-but-unconsumed since 10.1).

**Tests.** Headline engine integration: a sequential mock-llm chain (fixed-usage
provider so actual == estimate) under a $0.005 budget parks at exactly the step
whose projection first exceeds the cap (asserted `spent ≤ budget < spent +
one-step estimate` — the defined tolerance), history `[budget_exceeded]`,
`budget_exceeded` event present; then `SetBudget($1)` + `Unpark` → succeeded with
history `[budget_exceeded, succeeded]` and all five steps ledgered. Plus the
fail-policy DLQ (run failed, gen dead-lettered `permanent`), the
oversized-`max_tokens` guard (a call-counting provider asserts **zero** provider
calls), the `budgetDecide` unit matrix (run park/fail/proceed, step max_tokens,
unknown-model fail-closed, free-tool proceed), the `CostEstimate` exec unit
test, the store migrate round-trip, the api PATCH contract (200/400/404 + run-
view reflection), and the ctl unit test. Full store/engine/api integration
suites green; golangci-lint + `openapi-lint` clean.

**Deferred:** model downgrade chains (10.4 — slots before park/fail in
`budgetDecide`), budget metrics + `cost_updated` events (10.5), a submit-time
`budget_usd` override on `POST /v1/runs`, and reservation-based exact accounting
for parallel fan-out (backlog).

### 10.4 — Model downgrade chains ✅

Delivered the last budget-action of the cost model: as a budgeted run approaches
its cap, a claim of an llm step carrying a fallback chain is routed to a cheaper
model — priced, cached, and rate-limited correctly at the fallback — instead of
parking at the primary's price. The decision sits at the same claim-time point
as 10.3's budget check, so downgrade is a scheduling feature, not a wrapper.

**The contract (`internal/dag`).** `model_fallbacks: [{model, at_budget_fraction?}]`
on the **llm step config** — an ordered, cheapening chain (`ModelFallback`, an
`at_budget_fraction *float64` mirroring the `Temperature` pointer). Validation
under `config_field_invalid`: each `model` non-empty and distinct from the
primary and from every other fallback; `at_budget_fraction` in `(0, 1)` and
requiring the definition to carry a `budget_usd` (a fraction of nothing cannot
fire); thresholds non-decreasing along the chain (cheaper tiers trigger at
higher spend); and a chain with no budget to trigger against at all (no run
`budget_usd` and no step `budget.max_usd`) rejected — the project's
can't-fire-is-an-authoring-mistake stance. `checkModelFallbacks` runs in
`checkSteps` where both the def (for `budget_usd`) and the step (for
`budget.max_usd`) are in scope. Regenerated JSON Schema; kitchen-sink coverage
in both copies (`examples/` and the differently-tuned `testdata/valid/`);
decode/validate unit tables. **No migration** — the actually-used model is
already durable on 10.2's `cost_ledger.resource` and the attempt output, so
"record the used model on the attempt" needs no new column.

**The executor hook (`internal/exec`).** New optional `exec.ModelDowngrader`:
`ModelFallbacks(sc)` reports the current model and the chain (projected onto an
exec-level `ModelFallback` so the engine needs no `dag` import), and
`WithModel(sc, model)` rewrites **only** the rendered config's `model` field via
a `map[string]json.RawMessage` re-key — every sibling value stays
byte-identical. Implemented on the llm executor (no other executor has a runtime
notion of "model"). Because the response-cache key, the resource limiter's
bucket, the cost estimate, and the provider call all derive from the config's
model, that one rewrite re-targets the whole pipeline. The
different-model-different-key property is asserted at the exec layer
(`TestDowngradeChangesCacheKey`: two bindings identical but for the model
produce different `cache.Key`s), which is exactly the ADR-011 assertion 10.4
owed.

**The decision (`engine/budget.go` + `engine/downgrade.go`).** `budgetCheck`
was restructured: the model-independent `max_tokens` guard runs first (a cheaper
model can't rescue an oversized request), the step's cumulative cost is read
**once** (`SumByStep`) and threaded into both the downgrade fits-check and the
step `max_usd` cap, and — when the executor is a `ModelDowngrader` with a
non-empty chain — `selectDowngrade` prices the primary and each tier and calls
the pure `chooseDowngrade`. Two triggers: **soft** (once run spend reaches a
fallback's `at_budget_fraction`, route to the deepest met tier — even if the
primary would still fit) and **hard** (the primary's projected `spend + estimate`
would exceed a budget → route to the least-aggressive fitting tier, avoiding a
park). A tier is chosen **only if priceable and its projection fits every
budget**, so the engine never downgrades to a model it would immediately park
on; when nothing fits the chain is *exhausted* and the check falls through to
10.3's ordinary park/fail on the primary. The decision is **single-shot** (no
budget re-entry — it picks the final fitting tier in one pass, so a multi-tier
threshold jump is one event, and there is no loop that could re-enter with the
primary lost from the config). `budgetDecide` (reached only when no downgrade
applied) keeps the 10.3 park/fail/proceed routing but is now IO-free and returns
no error (the `SumByStep` read moved up). An unpriceable primary (unknown model
under fail-closed) yields no downgrade — `budgetDecide` lands its permanent
failure, keeping the fail-closed judgment in one place (rescuing an unknown
primary via a known fallback was deliberately kept out). `chooseDowngrade` is
unit-matrix-tested independently: threshold picks the deepest met tier;
projection routes to the most-expensive fitting tier; a rescue drops past a
threshold tier that doesn't fit; exhaustion returns no downgrade; an unpriceable
deepest tier is skipped.

**The event & re-cache (`store` + `engine/claim.go`).** On a downgrade the
middleware records `model_downgraded` via `store.RecordModelDowngrade` — a
downgrade is **not** a state transition (no status change; the used model is
durable on the ledger), so it takes the run lock, reads the step, **fences on
the caller's live claim** (a zombie cannot record a misleading downgrade → a
`*store.TransitionError`, so `budgetCheck` abandons like any other fenced write),
and appends the event with from/to models and resources, the trigger
(`budget_threshold` | `budget_projection`), the threshold fraction, and the
spend/budget projection. `budgetCheck` returns the re-targeted config; the claim
path sets `sc.Config` and **re-reads the response cache on the fallback model**
(discarding the primary's miss write-binding, since it was keyed on the primary)
before proceeding — a fallback cache hit still short-circuits the provider call.

**Tests.** Headline engine integration (`downgrade_integration_test.go`, a
catalog pricing `mock:expensive` 50× `mock:cheap` so the swap is observable): an
expensive-model chain under a $0.25 budget with a `0.5` threshold runs gen0/gen1
expensive then downgrades gen2–gen4 to cheap — each attempt ledgered at the
model that served it (per-step `cost_ledger.resource` and the output `model`
both asserted), three `model_downgraded` threshold events with the right
from/to, and the run aggregate equal to the exact ledger sum. Plus a
projection-trigger test ($0.05 budget < the $0.10 primary estimate → immediate
downgrade, trigger `budget_projection`, limit `run`) and an exhausted-chain test
($0.001 budget < even the cheap estimate → park with **zero** downgrade events,
history `[budget_exceeded]`, then `SetBudget($1)` + `Unpark` → succeeds on the
primary). Store integration proves the fenced/`ErrNoTx` paths of
`RecordModelDowngrade`; exec units cover `ModelFallbacks`, the byte-preserving
`WithModel`, and the cache-key assertion; the `budgetDecide` unit matrix gained
step-`max_usd` cases now that it takes the threaded prior-cost. Full
store/engine/exec/dag/api integration suites green; golangci-lint clean
(`budgetDecide`'s now-always-nil error dropped to satisfy `unparam`).

**Deferred:** downgrade + budget metrics and `cost_updated` events (10.5), and
rescuing an unpriceable primary via a known fallback (kept out — an unknown
primary fails closed, crisply).

### 10.5 — Cost metrics & events ✅

Closed M10 with the reporting half of the cost model: Prometheus cost counters,
the budget-park and saved-by-cache counters, and a `cost_updated` event appended
per cost-bearing attempt carrying the run's running totals — the durable source
the M18 live meter will render (via M16's feed). No migration, no config var, no
new store primitive; the only SQL change is a `RETURNING` on the existing
aggregate bump.

**The metrics (`internal/obs/metrics` + `engine.Metrics`).** A new ADR-008
`cost` subsystem, six bounded-label counters: `engine_cost_spent_usd_total`
and `engine_cost_saved_usd_total` (labeled `resource`), the paired
`engine_cost_{input,output}_tokens_total{resource}`,
`engine_cost_budget_exceeded_total{limit,action}`, and
`engine_cost_downgrades_total{trigger}`. Money is exported in the **base USD
unit** (`float64(nano)/1e9`, a new `nanoUSDToFloat` helper) — the ledger stays
the exact integer nano-USD source of truth, and a rate counter tolerates the
float; the units-in-name rule gained `_usd` in ADR-008. `resource` is the same
pricing-catalog name the ledger and limiter use (bounded to the catalog, never
run/step), and the three tiny new label vocabularies (`limit`: run/step_usd/
step_tokens, `action`: park/fail, `trigger`: budget_threshold/budget_projection)
were added to ADR-008's allowlist and the executable conformance table. New
`engine.Metrics` methods `CostSpent/CostSaved/CostTokens/BudgetExceeded/
ModelDowngraded` (with `nopMetrics` stubs so every test layer stays recording-
off), the instruments/registration/recorders in `WorkerMetrics`, and the
`exercise()` audit helper all extended together.

**Where the counters fire.** Spend/saved/tokens are recorded **post-commit** in
`completeSuccess` via a new pure `engine.recordCost(store.AttemptCostArgs)` —
reached only after `txErr == nil`, so a fenced or rolled-back completion counts
nothing (the counters track exactly the charges that landed in the ledger). A
cache hit records only its counterfactual `saved`; a productive call records
`spent` and decodes its `Usage` for the token counters. The budget counter
increments at the claim-time decision: `park` inside `completeBudgetPark`
(post-commit, at the parked-log point — so under fan-out each released sibling
counts once, mirroring the `budget_exceeded` event, even though only the first
re-parks the run), and `fail` at both `completeFailure` routes in `budgetCheck`
(max_tokens and the `budgetDecide` fail), each guarded on the completion
returning nil. The downgrade counter increments once per successfully recorded
`model_downgraded`. These are the **first** metric recordings on the
completion-success and budget paths (survey confirmed none existed).

**The `cost_updated` event (`internal/store`).** `AddRunCost` grew a
`RETURNING spent_nano_usd, saved_nano_usd, budget_nano_usd` (now `:one`), so
`ApplyAttemptCost` returns `(store.CostApplied, error)` and — in the **same
completion transaction**, under the run lock, sharing the monotonic `seq` with
the aggregate bump — appends `cost_updated` (`store.EventCostUpdated` mirroring
the new `cost.EventTypeCostUpdated`) carrying `store.CostUpdatedEvent`: the
attempt's charge plus the run's running spend/saved totals + budget after the
bump. Because the append shares the lock and seq with the bump, a run's
`cost_updated` totals are **non-decreasing in seq order by construction** — the
property the M18 meter relies on to render the running total straight off the
event stream, stateless. One event per cost-bearing attempt (same guard as the
ledger row — free tools and no-usage attempts write nothing; cache hits do,
since the saved total moves), and it precedes the `cost_unknown_model` warning
(lower seq) so a consumer sees the total move before the advisory that priced it.

**Dashboard, alert, smoke.** The Engine Grafana board gained a **Cost** row —
spend rate ($/min) and saved-by-cache rate ($/min) by resource, tokens/s, and a
budget-actions/downgrades panel — inserted before the Fleet row (renumbered),
green under the anti-drift `TestDashboardsAndRulesReferenceRegisteredMetrics`.
A dev-scale `BudgetParkRateSpike` example alert joined `prometheus-rules.yml`
with a byte-matched promtool test (`make obs-lint` green). `make smoke-metrics`
gained a temperature=0 mock pair (miss → spend + tokens, hit → saved) and the
positive-value assertions; `make smoke-dashboards` drives the same pair paced 2s
so the spend/saved panels are genuinely non-empty, allowlists the
budget/downgrade panel (no budgeted runs in the smoke workload), and adds
`BudgetParkRateSpike` to the loaded-rules check.

**Tests.** The store `TestApplyAttemptCostAggregatesAndEvents` now asserts one
`cost_updated` per row with monotonic totals ending at the aggregate; the new
`internal/engine/cost_metrics_integration_test.go` proves spend/saved/token
counters equal the ledger (miss+hit via the pricing cache fixture) with the
run's `cost_updated` events matching the aggregate, the budget-park counter on a
metered budget fixture, and the downgrade counter equal to the
`model_downgraded` event count on a metered downgrade fixture. Full
store/engine/api integration suites green (cost-bearing runs now carry an extra
`cost_updated` event — no exact-count assertion broke); golangci-lint clean;
`make obs-lint` green.

**Deferred:** the `cost_updated` events are durable but have **no read API and
no pub/sub** — the event-feed endpoint and live publish path are M16 (ADR-018),
and the run-header cost ticker/meter UI is M18.4.

## Milestone 11 — Output validation & semantic retries

### 11.1 — ADR-013 & validator SPI ✅

M11's differentiator is a validator chain on step outputs plus the semantic
retry loop that repairs failures with a critique-augmented prompt. 11.1 lays
the whole foundation: the validator SPI, the verdict model, the
definition-contract chain config, and — unlike the pure "contract half"
openers (9.1/9.4/10.1) — the engine `validate` stage that actually persists a
verdict per attempt and routes a failing verdict to the `validation_failed`
outcome. It has to ship the stage, because the 11.1 acceptance criterion
"verdict persistence visible in run status API" requires something to *write* a
verdict; 11.2 (built-in validators), 11.3 (JSON repair), 11.4 (the semantic
retry loop), 11.5 (llm_judge), and 11.6 (quality metrics) build on it
additively. **One migration (0018), no new config var, no new metric.**

**ADR-013 (`docs/adr/013-output-validation-and-semantic-retries.md`).** Records
the validator SPI, the verdict model, the chain config, the engine stage's
placement, and the **routing decision table** (the required deliverable):
transport failure vs throttle vs budget vs validation failure, each decided by
a different signal, counted against a different budget, and re-dispatched under
a different reason. The two axes that separate validation from "just another
retry class": what the re-attempt *sends* (identical for transport/throttle/
budget; a different, feedback-augmented request for validation) and which
budget bounds it (transport for retries, none for the administrative outcomes,
semantic for validation). Cross-amends ADR-006 (the `validation_failed` row and
its M11 note — the class is added as an outcome but **not** unlocked in
`retry_on`, revising ADR-006's earlier expectation: validation gets its own
policy so the transport and semantic budgets stay disjoint), ADR-009 (a
"Validator SPI (as built, 11.1)" section), ADR-011 (invalid outputs never
cached; cache hits re-validated), ADR-012 (rule 5 amended — a validation_failed
attempt *has* usage and ledgers). Index row in `docs/adr/README.md`.

**The leaf package `internal/validate`** — the fifth and final ADR-009
kind-owning package, structured exactly like `internal/tools` (imports
`internal/plugin` + `internal/dag` + jsonschema, never exec/engine):
- `validate.go` — the `Validator` interface (`Manifest() + Validate(ctx,
  Input) (Verdict, error)`), the `Input` carrying the whole output, the
  targeted `Value`, the decoded config, the semantic attempt, and a logger;
  `ConfigSchema` (invopop reflector, the `tools.argsSchema` settings) and
  `EmptyConfigSchema` (a no-config validator declares `{}` explicitly).
- `verdict.go` — `Verdict{schema_version, status, score?, issues[],
  results[]}`, `Issue{validator, code, path, message}`, `ValidatorResult`, the
  status enum, `PassVerdict`/`FailVerdict`/`Marshal`. The persisted contract.
- `errors.go` — `*Error{Validator, Class}` (a *transport* failure of the
  validation stage, transient/permanent — distinct from a fail verdict) +
  `Transientf`/`Permanentf`, `*UnknownValidatorError`, `*ConfigValidationError`;
  secret hygiene structural (no payload field), ctx errors unwrapped.
- `registry.go` — the typed facade over `plugin.Registry` (kind validator);
  **compiles each config schema once at registration** (nil rejected) and
  exposes `ValidateConfig` (the pre-flight gate; absent config validates as
  `{}` so a no-config validator accepts it while a required-field one rejects);
  `NewBuiltins()` is **empty in 11.1** (11.2 fills it).
- `chain.go` — `Resolve(reg, policy, stepType) (*Chain, error)` (the pre-flight:
  unknown validator / bad config → typed permanent errors; computes each
  entry's effective target — `/text` default for llm-family) and `Chain.Run`
  (the chain semantics: **all cheap validators run** for a full critique, then
  cost-bearing ones only if every cheap one passed — cheap-first; chain fails
  if any fails; score = min; issues concatenated; a validator error aborts the
  chain). Includes a minimal RFC 6901 JSON-pointer resolver — a target that
  does not resolve is a **fail verdict** (`code: target_not_found`), never a
  panic or a transport error.

**The `dag` contract** — `internal/dag/validation.go` (`ValidationPolicy{
Validators []ValidatorSpec}`, `ValidatorSpec{Name, Config, Target}`,
`MaxValidators = 16`, `pluginNameRe`, `checkJSONPointer`); `Step.Validation`
on the envelope; `decodeValidation` (strict decode — rejects 11.4's reserved
`max_attempts`/`feedback` keys, the ADR-006 reservation discipline — and
compacts each opaque config so the definition round-trips losslessly);
`checkValidation` under new codes `validation_field_required`/
`validation_field_invalid` (≥1 validator, name shape, JSON-pointer syntax,
chain-length cap); regenerated JSON Schema; both kitchen sinks carry a
validation chain (an entry with an explicit target) pinned by the extended
`TestExampleKitchenSinkCoversEveryConstruct`; four decode-corpus fixtures and
two structural fixtures added. The `retry_on` rejection message for
`validation_failed` changed from "reserved until M11" to "configure semantic
retries via the validation policy, not retry_on".

**The store** — migration 0018: `run_steps.validation_policy JSONB` (materialized
at instantiation like `cache_policy`), `step_attempts.verdict JSONB` (the 0012
`usage` precedent), and widened `step_attempts.outcome` + `dead_letters.class`
CHECKs adding `validation_failed`. `finishAttempt` grew a `verdict` param
threaded through all eight call sites; `SucceedStepArgs.Verdict` and
`DeadLetterStepArgs.{Usage,Verdict}` (a validation_failed dead-letter carries
both — the provider billed); `AttemptOutcomeValidationFailed`. `FinishStepAttempt`
+ the `CreateRunStep(s)` inserts gained the columns; sqlc regenerated; the
migrate round-trip test bumped to 18.

**The engine** — `engine/validate.go`: `resolveChain` (pre-flight, after
renderConfig and **before cacheRead/any spend** — a corrupt policy / unknown
validator / bad config is a permanent failure before money is spent);
`runChain`; `completeValidationFailure` (the terminal route — records the
`validation_failed` attempt with usage + verdict, meters the spent cost via the
reused `priceAttempt`/`ApplyAttemptCost`, dead-letters source
`retries_exhausted`, applies the run's `on_failure` disposition; on a cancelling
run settles cancelled instead); `validatorErrorClass`. The stage is wired into
`execute()` (claim.go) **after a successful execute and before cacheWrite** —
so an invalid output is never cached (ADR-011); a fail verdict →
`completeValidationFailure`, a validator error → `completeFailure` with the
carried class, a pass → cacheWrite + `completeSuccess`. `completeSuccess` grew a
`verdict *validate.Verdict` param (marshaled onto the attempt like usage —
drop-with-warning on marshal failure, since a passing verdict is informational;
the failing path treats a marshal failure as a permanent failure, the verdict
being load-bearing evidence). The **cache-hit path re-validates**: `cacheRead`
takes the chain and runs it on a hit — a pass serves (short-circuiting the
provider), a fail or validator error falls through to re-execute (a
stored-but-now-invalid output is re-executed, not served — the cache key does
not cover the validation envelope). New `engine.WithValidators` seam (nil = a
step with no chain runs unchanged, a step that authored one fails permanent at
resolve). `cmd/worker` wires the (empty) registry; `cmd/api` folds its
manifests into `GET /v1/plugins`.

**The API** — `AttemptView.Verdict json.RawMessage` (+ the run-view literal in
`runs.go`); OpenAPI `Verdict`/`VerdictIssue`/`ValidatorResult` components, the
`AttemptOutcome` enum gained `validation_failed` **and the pre-existing-missing
`budget_exceeded`** (a 0017 drift, fixed in the same pass); still lints 100/100.

**Non-obvious decisions.** (1) 11.1 ships the engine stage, not just the SPI —
the "verdict visible in run status API" criterion demands a producer. (2)
Validation failure is *not* a `retry_on` class — it retries a different
request under a separate budget, so ADR-013 gives it its own policy instead of
ADR-006's forecast "unlock in retry_on". (3) A cache hit is re-validated, not
served-blind — validity depends on the current chain, not the key. (4) A
missing target is a fail verdict, not an error — a content problem the semantic
retry can fix. (5) An absent chain-entry config validates as `{}` (a no-config
validator accepts it; a required-field one rejects) rather than `null` (the
tools convention), because "no config key" means "empty config" for a
validator. (6) DLQ source `retries_exhausted` (not a new source) — the semantic
budget is implicitly one attempt in 11.1, so 11.4's "DLQ for exhausted semantic
retries" continues the same source unchanged.

**Verified.** `go build ./...`, `make generate` (schema + sqlc, no diff after
commit), `make lint` (0 issues module-wide), `make openapi-lint` (100/100), the
full unit suite green (`internal/validate` race-clean), and — against the
compose DB at schema 18 (`make migrate-up`) — the store, engine, and api
integration suites green. New integration tests (`internal/engine/
validate_integration_test.go`, `internal/api/validation_integration_test.go`):
verdict persisted on pass (+ visible on `GET /v1/runs/{id}`), validation
failure → `validation_failed` outcome + verdict + usage + DLQ
(`retries_exhausted`/`validation_failed`), invalid output never cached
(provider re-called), unknown validator permanent with **zero** provider calls,
validator error routes as a transport (permanent) failure not a fail verdict,
and cache-hit re-validation (provider once, validator twice).

**Deferred:** no built-in validators yet (`validate.NewBuiltins()` empty —
11.2); no semantic-retry loop (a failing verdict dead-letters on the first
attempt — 11.4); no `llm_judge` / overhead cost (11.5); no quality metrics
(11.6).

### 11.2 — Deterministic validators ✅

**What shipped.** The five built-in deterministic validators that fill 11.1's
empty `validate.NewBuiltins()` — `json_schema`, `regex`, `contains`, `cel`,
`numeric_range` — plus the pre-flight compilation gate and compile cache that
make "bad config → permanent before spend" and "compiled once, reused across
attempts" framework guarantees. No migration, no config var, no metric, no
engine or `internal/dag` change: the 11.1 validate stage runs the built-ins
unchanged, and both deployables already call `NewBuiltins()`, so the five
validators appear on `GET /v1/plugins` with zero wiring change.

**The validators** (one file each in `internal/validate`, all `cacheable`-only
at `1.0.0`, config schema generated from the Go config struct):

- `json_schema` (`{schema}`): compiles the author's JSON Schema with
  `santhosh-tekuri/jsonschema/v6` and reports **one issue per violating
  instance location**, the RFC 6901 pointer in `Path` and a keyword-derived
  code (`type_mismatch`, `required`, `additional_properties`, `below_minimum`,
  `pattern_no_match`, …). A **string** target — the `/text` default for
  llm-family steps — is re-parsed as JSON (LLM JSON arrives as text); a string
  that is not JSON is an `invalid_json` **fail verdict**, never a panic.
- `regex` (`{pattern, negate?}`): RE2 match over the target's string form.
- `contains` (`{substring, case_insensitive?, negate?}`): substring scan.
  (`regex`/`contains` stringify a non-string target as compact JSON — grep-like
  semantics rather than an outright failure.)
- `cel` (`{expr, parse_json?}`): a CEL boolean predicate over a **distinct**
  environment `value` (targeted sub-tree) / `output` (whole payload) /
  `step_type`. Bool-ness is enforced at compile time (pre-flight, permanent),
  but a **runtime** eval error (a missing field on *this* output) is a
  `cel_eval_error` fail verdict, not a transport failure — deterministic in the
  content, repairable by the semantic retry.
- `numeric_range` (`{min?, max?, exclusive_min?, exclusive_max?}`): accepts a
  JSON number or a numeric string (`"42"`, `" 3.14 "`); NaN/±Inf are
  `not_a_number`.

**The pre-flight gate & compile cache.** New optional SPI interface
`ConfigCompiler` (`CompileConfig(config json.RawMessage) error`). The
registry's `ValidateConfig` calls it after the config-schema check, so a
validator can reject content the JSON Schema cannot express — an unparseable
regex, a non-boolean/ill-typed CEL expression, a JSON Schema that will not
compile, a `numeric_range` with no bound or an inverted/unsatisfiable one — as
a permanent `*ConfigValidationError` fired at claim **before any spend**,
exactly like a schema violation. `json_schema`/`regex`/`cel` each hold a
generic `compileCache[T]` keyed on the **SHA-256 of the config bytes**;
`CompileConfig` warms it and `Validate` reads it, so an artifact compiles
**once per distinct config per worker process** and is reused across every
attempt, retry, takeover, and run of the definition (materialized policy bytes
are byte-stable). The cache is bounded (clear-on-overflow) and race-safe
(compile outside the lock; a duplicate racing compile is harmless).

**Tests.** Table tests per validator (good/bad outputs, negate, case-folding,
`parse_json`, numeric strings, boundary conditions), a malformed-JSON case for
every validator asserting a structured verdict and no panic, a path-level
schema-issue test; `TestPreflightRejectsBadConfig` proving each bad artifact is
a permanent `*ConfigValidationError` through the public `Resolve` path;
in-package `compileCount` assertions that each compiling validator compiles a
config exactly once across 100 `Validate` calls (and a concurrent
coalescing test); warm/cold benchmarks (warm `json_schema` ~943ns vs cold
~19.8µs — ~21×, and ~16× fewer allocs); api `TestListPluginsWithValidators`
(kind `validator`, cacheable-only, non-empty self-describing config schema —
the "UI-ready" criterion); and engine integration on the **real** built-ins —
a `contains` chain that passes on the mock's echoed output (verdict persisted,
run succeeds) and a `json_schema` chain over the non-JSON `/text` that records
`validation_failed` with a structured `invalid_json` issue and dead-letters.

**Non-obvious decisions.** (1) `json_schema` auto-parses a string target as
JSON (no knob) because its purpose is structural JSON and the `/text` default
hands it a JSON string; `cel` opts into the same re-parse via `parse_json`
because a predicate may want the raw string. (2) CEL runtime errors are fail
verdicts, not transport failures — the opposite of `dag`'s edge predicates,
where a runtime error is a step failure; for a validator the retry can repair
the output, so the content problem stays a verdict. (3) The compile cache lives
in the leaf validators keyed on config bytes, not in an engine-side chain
cache, so reuse spans runs (not just attempts) with zero engine change. (4) The
`cel` validator builds its own `cel.Env` (`value`/`output`/`step_type`),
separate from `dag`'s edge-predicate env (`output`/`run`).

**Deferred:** the semantic-retry loop (a failing verdict still dead-letters on
the first attempt — 11.4); JSON repair & structured-output modes (11.3); the
`llm_judge` validator + overhead cost (11.5); quality metrics (11.6).

### 11.3 — JSON repair & structured-output modes ✅

**What shipped.** The `output_format` step option — deterministic JSON repair
and provider-native structured output — that shapes an `llm` step's completion
into structured JSON before the validate stage judges it, so bad-JSON failures
are fixed at the source (or turned into a clean `validation_failed` verdict for
11.4's semantic retry) rather than flowing on malformed. One migration (0019),
one new stdlib-only leaf (`internal/jsonrepair`), no new config var, no new
metric, and no new engine hook interface — the repair happens inside the llm
executor and the enforcement reuses the 11.1/11.2 validate stage unchanged.

**Contract (`internal/dag`).** `LLMConfig` gains an optional `OutputFormat`
block (llm-only, like `model_fallbacks`): `type` (`json` | `json_schema`),
`schema` (required iff `json_schema`, a JSON object), `mode` (`auto` default |
`repair_only`). Validated under `config_field_invalid`/`config_field_required`
(type required + known; json_schema requires a JSON-object schema; a plain json
type takes no schema; mode known) via `checkOutputFormat`. Schema
*compilability* is deliberately not a submit-time check (the tool-args /
validator-config precedent — a runtime artifact) but a claim pre-flight one via
the implicit validator below. `decodeStepConfig` compacts the schema RawMessage
(the ToolConfig.Input precedent) so the definition round-trips losslessly.
Regenerated JSON Schema, both kitchen sinks carry an `output_format` (construct
pin extended), and `output_format_bad.json` covers the six invalid cases.

**JSON repair (`internal/jsonrepair`, a stdlib-only leaf).** `Repair(text)`
runs a fixed sequence of conservative, cumulative passes — `strip_code_fence`,
`extract_first_json` (first balanced JSON value, string/escape aware),
`trailing_commas`, `unquoted_keys` — never touching string contents,
short-circuiting the moment the working text parses, and compacting the result.
The outcome is `raw` (already valid), `repaired` (a pass fixed it, with the
changed passes named and the raw text kept), or `unrepairable`. A
fixture-driven `testdata/corpus.json` with an **exact asserted repair rate**
(14 of 17 cases recover — three deliberately unrepairable), per-pass table
tests, an idempotence test (a repaired value re-repairs to itself), and a fuzz
target asserting no panic and "non-unrepairable ⇒ valid JSON".

**Provider-native structured output (`internal/llm`).** `ChatRequest` gains
`ResponseFormat` (nil = free text; `Schema` nil = any JSON object, set =
schema-conforming) and `ChatResponse` gains `Structured` (the provider's native
payload, kept out of `Blocks`). Anthropic offers a forced synthetic tool
(`agentloom_structured_output`, `tool_choice` forced, its input lifted onto
`Structured` and never surfaced as a user tool call); OpenAI sets
`response_format` (`json_object` for a schemaless request, a strict named
`json_schema` otherwise) and lifts a valid-JSON content onto `Structured`; the
mock answers `{"echo": <last user text>}` natively under a `ResponseFormat`
request and honors a scripted `MockOutcome.Structured`. Golden request +
response fixtures both directions per provider (`request_structured.json` /
`success_structured.json`), plus gated live structured smokes.

**Executor (`internal/exec/llmexec.go`).** `buildChatRequest` sets
`ResponseFormat` for `mode: auto` (nil for `repair_only`, so that mode leaves
the request byte-identical). Post-call, `marshalLLMOutput` shapes the output
when an `output_format` is present: native → `json` = the structured payload,
`text` = its canonical compact form, provenance `native`; else `jsonrepair`
over the text → `raw`/`repaired` (`json` = the value, `text` = canonical) or
`unrepairable` (`text` = the raw model output kept, no `json`). The persisted
`llmOutput` gains a `json` field so `${{ steps.<id>.output.json.<field> }}`
flows structured data; `exec.Output.Repair` carries the provenance. The
`output_format` block joins the cache key (`cache.LLMRequest.OutputFormat`,
appended only when present — no `KeySchemaVersion` bump, existing entries stay
valid) and the input-token estimate (schema bytes).

**Implicit validator (engine).** `resolveChain` prepends a synthetic
`json_schema` validator to an llm step's chain whenever it declares an
`output_format` — the author's schema for a `json_schema` type, an empty `{}`
(parseability only) for a plain `json` type — targeting the `/text` default. So
an unrepairable output is a proper `invalid_json` **fail verdict** (routing to
`validation_failed` and, from 11.4, the semantic retry) instead of a silent
success with a missing `json` field, and an uncompilable schema is a permanent
config error at claim before spend (11.2's `ConfigCompiler`) — all reusing the
existing validate machinery with **zero `internal/validate` changes**. The
schema is read off the materialized config (`output_format` is never
templated), so `resolveChain` needs no rendered config.

**Provenance persistence.** Migration 0019 adds `step_attempts.repair` (nullable
JSONB, the usage/verdict precedent — no new outcome or class). `finishAttempt`
threads a `repair` param through all eight call sites; `SucceedStepArgs` and
`DeadLetterStepArgs` gain `Repair`, so both the success and the
validation_failed paths persist it. A cache **hit** carries the miss's
provenance (`cacheEntry.repair`). The API surfaces `attempts[].repair`
(`RepairProvenance` OpenAPI component — `status: native|raw|repaired|
unrepairable`, `steps?`, `raw_text?`; spec still lints 100/100).

**Tests.** The `internal/jsonrepair` corpus/table/fuzz suite; the executor unit
suite (native / repaired / unrepairable / repair_only paths + an
output-format-changes-cache-key assertion); the provider golden fixtures; the
engine integration suite `structured_output_integration_test.go` (a
fenced-and-trailing-comma completion repaired to a succeeded run whose repaired
`.json` flows into a downstream step; a prose completion dead-lettered
`validation_failed` with an `invalid_json` verdict and unrepairable provenance;
a native-echo success with native provenance) — all green on the compose stack;
the migrate round-trip bumped to 19. New canonical
`examples/definitions/structured_extract.json` (corpus-pinned), whose schema
matches the mock's native echo so it runs offline-green.

**Non-obvious decisions.** (1) The **implicit `json_schema` validator** is the
one scope judgment beyond the ticket's literal text — without it, a declared
`output_format` would not actually *enforce* structure (an unrepairable output
would succeed silently), defeating "reduce failures at the source"; it costs
~30 lines in `resolveChain` and reuses 11.1/11.2 wholesale. (2) **Append-only
cache-key component** rather than a `KeySchemaVersion` bump, so pre-11.3 entries
stay valid (a bump would invalidate the whole fleet's cache). (3) `text` is
overwritten with the canonical JSON (not left as the raw model output) so the
`/text` default target — the implicit validator and any author-supplied
validator — sees the repaired structure; the raw text survives as
`repair.raw_text` on the `repaired` provenance. (4) JSONB storage normalizes the
nested `json` field's whitespace, so the integration test compares it
semantically, not byte-wise.

**Deferred:** the semantic-retry loop that consumes a failing verdict to
rebuild the prompt (11.4 — a failing verdict still dead-letters on the first
attempt); the `llm_judge` validator + overhead cost (11.5); quality metrics
including repair rate (11.6).

### 11.4 — Semantic retry engine ✅

**What shipped.** The semantic-retry loop ADR-013 reserved: on a failing
validation verdict the engine now rebuilds the prompt from the critique and
re-attempts under a semantic budget disjoint from the transport one — instead
of dead-lettering on the first bad verdict (unless the budget is one, the
default). Contract, executor hook, store transition, engine router, API surface,
docs, and a canonical example.

**Contract (`internal/dag`).** `ValidationPolicy` gained `max_attempts`
(`1..MaxSemanticAttempts`=10; absent=0 means one attempt — the 11.1 behavior
every unset step keeps, via `EffectiveMaxAttempts()`) and `feedback`
(`FeedbackPolicy{template?, max_output_chars?}`, llm-family-only, requires
`max_attempts >= 2`). `internal/dag/feedback.go` is a **deliberately narrow**
template surface — exactly `${{ feedback.prior_output|issues|attempt|max_attempts }}`
— with one scanner (`scanFeedbackTemplate`) shared by submit-time
`CheckFeedbackTemplate` and retry-time `RenderFeedback`, so a template the
definition accepts can never reference a token the renderer lacks. `checkValidation`
grew `checkSemanticPolicy` and one relaxation: a `validation` block with no
validators is admissible on an llm step with `output_format` (the implicit
json_schema validator is the chain). Regenerated JSON Schema, both kitchen
sinks carry a semantic policy (new 11.4 construct pin), `feedback_test.go`;
`validation_reserved_key.json` repurposed to `validation_feedback_unknown_field.json`
and two structural fixtures added.

**Executor (`internal/exec`).** `exec.Feedback{schema_version, semantic_attempt,
max_attempts, prior_attempt, text}` (the `exec.Repair` precedent) + the
`FeedbackInjector` hook; `LLMExecutor.WithFeedback` re-keys the rendered config
(prompt-suffix with a blank-line separator, or a trailing user message),
byte-identical siblings otherwise — the `WithModel` pattern. Tests: prompt/messages
forms, empty-text no-op, and `TestFeedbackChangesCacheKey` (the augmented request
re-keys the cache).

**Store.** Migration 0020: `run_steps.feedback` (the pending critique the claim
CAS copies onto the next attempt, cleared on succeed/DLQ/poison) +
`step_attempts.feedback` (what each attempt was given) — no new table/outcome/class.
New `SemanticRetryRunStep` query (the 5.2 retry CAS + `feedback = @feedback`);
`CreateStepAttempt` gained a feedback param (copied off `step.Feedback` at claim
in `ClaimStepWithOrigin`); `CountValidationFailures` (validation_failed past the
DLQ baseline — the exact mirror of `CountCountedFailures`); `SucceedRunStep`/
`DeadLetterRunStep`/`PoisonDeadLetterRunStep` clear feedback. New transition
`store.SemanticRetryStep`, event `step_semantic_retry_scheduled` + payload,
outbox reason `semantic_retry`, queue reason `ReasonSemanticRetry`,
`AttemptRepo.CountValidationFailures`. Migrate round-trip bumped to 20.

**Engine.** `engine/feedback.go`: `injectFeedback` (decode `step.Feedback` →
`FeedbackInjector.WithFeedback`; corrupt record or injector error warns and
proceeds un-augmented — feedback is diagnostic, the verdict is load-bearing),
`buildFeedback` (renders the critique for the next attempt; `priorOutputText`
pulls the llm-family `/text` answer, `formatVerdictIssues` bullets the issues),
`decodeValidationPolicy`, `collectVerdictHistory`. `claim.go` injects feedback
**after resolveChain, before cacheRead**, threading the semantic attempt into
`runChain` (11.1's `validationAttemptNumber` constant is gone) and `cacheRead`.
`completeValidationFailure` became a router: in-tx `CountValidationFailures` →
`n < max` ⇒ `SemanticRetryStep` + an **outbox** re-dispatch (reason
`semantic_retry`, `next_attempt_at = now` — a semantic retry has no backoff, so
it goes through the outbox, not the delayed ZSET; `reconcile_retry` heals a
crash) + cost ledger + post-commit `metrics.RetryScheduled("validation_failed")`
+ nudge; else the terminal DLQ (source `retries_exhausted`) with
`semantic_attempts` + `verdict_history` on the record.

**API.** `AttemptView.feedback` (the critique on the wire) +
`StepView.{transport_failures, validation_failures}` (disjoint counters derived
from the attempt list); OpenAPI `Feedback` component + the two StepView fields
(still 100/100). `examples/definitions/semantic_retry.json` (corpus-pinned).

**Non-obvious decisions.** (1) **Feedback stored on the step, not only the
attempt** — so the pending critique survives a crash/takeover and an interleaved
transport retry (429) between semantic attempts; the claim CAS copies it onto
the fresh attempt, and success/DLQ clears it. (2) **Semantic re-dispatch via the
outbox, not the delayed ZSET** — a semantic retry has no backoff, so an in-tx
outbox row (dispatched immediately, reason `semantic_retry`) beats the
scheduler round-trip; the existing overdue-retrying reconciler scan still heals
a commit-then-dispatch crash because the row is `retrying` and due now. (3)
**Two disjoint budgets by construction** — `CountValidationFailures` and
`CountCountedFailures` are separate queries over separate outcome sets, so
neither can drain the other; both surface per step in the status API. (4) **The
feedback template is not the 8.2 expression engine** — four fixed tokens on an
envelope field that is never step-config-rendered; a fuller language would be
unnecessary and a footgun. (5) **A budget park closes the parked attempt
`budget_exceeded`, not validation_failed** — the semantic loop halts before the
provider call, and the pending feedback stays on the (now-ready) step, so
Unpark resumes the critique-carrying re-attempt. (6) **Corrupt feedback warns
and proceeds** (un-augmented, still validated) rather than failing permanently,
unlike a corrupt *policy* (which dead-letters, since the budget can't be read).

**Verified.** `make generate` (no diff), `make lint` (0 issues), `make
openapi-lint` (100/100), the full unit suite, and — against the compose DB at
schema 20 (`make migrate-up`) — the store, engine, and api integration suites
green. New tests: `internal/engine/semantic_retry_integration_test.go`
(fail→fail→pass on 3 semantic attempts with diffable stored feedback + 2
`step_semantic_retry_scheduled` events; exhaustion → DLQ with a 2-entry
`verdict_history` + requeue re-arming the semantic budget; budget-park halting
the loop then SetBudget+Unpark completing it), `internal/api/
validation_integration_test.go::TestSemanticRetryVisibleInStatusAPI` (feedback +
disjoint counters on the wire), plus the dag/exec unit suites.

**Deferred:** the `llm_judge` validator + overhead cost (11.5); quality metrics
— validation-failure rate, semantic-retry depth histogram, judge score
distribution (11.6).

### 11.5 — LLM-judge validator ✅

Added `llm_judge`, the first **cost-bearing** built-in validator — a semantic
quality gate whose failing rationale feeds 11.4's semantic-retry loop, and
whose provider call is billed to the serving step as **overhead** (ADR-012 rule
4). Reuses the whole 11.1–11.4 substrate (verdict shape, routing table, chain
semantics, outcome vocabulary): **no migration, no new config var, no new
metric, no new outcome/class.**

**The validator (`internal/validate/llm_judge.go`).** `cacheable +
cost_bearing` at `1.0.0`. Config: `model` (routed through the `llm.Registry`
like an llm step) + ordered `fallback_models` + `rubric` (static text) +
`threshold` (pass iff `score >= threshold`) + `on_error` (`fail` default |
`skip`) + optional `max_tokens`/`temperature`/`timeout`/`max_output_chars`/
`max_rationale_chars`. `CompileConfig` (11.2's `ConfigCompiler`) is the
pre-flight gate: a blank rubric, an out-of-range threshold, a duplicate/blank
fallback, a bad `on_error`/`timeout`, or a `model`/fallback that does **not
route** (a nil registry — the keyless build — routes nothing) is a permanent
config error at claim, before the productive step spends; the validated
artifact (parsed config + resolved timeout + model chain) is compile-cached by
config bytes. `Validate` builds a fixed content-free system prompt + a user
message pairing the rubric with the delimited candidate output
(`stringOf(in.Value)` — the `/text` default, truncated) + a `ResponseFormat`
(native structured output for real providers; the `jsonrepair` pass reused from
11.3 for text answers) at temperature 0, calls the model chain under a per-call
`context.WithTimeout` (a provider error or the judge's own timeout falls
through to the next model; a caller-context cancel passes through **unwrapped**
so the engine keeps the timeout/cancelled judgment — the `http_request`
precedent), and parses a required `{score∈[0,1], rationale}`.

**Verdict.** Pass → a verdict carrying the score + one `Results[0]` with
`rationale` and `usage`; below-threshold → a `rubric_below_threshold` fail
whose message carries the rationale, so 11.4's `formatVerdictIssues` folds it
into the feedback prompt with no feedback-builder change. New optional
`ValidatorResult.{rationale,usage,error}` + a `ValidatorUsage` type (resolved
resource + served model + token counts) + `validate.Error.Usage` — all additive
JSON on the existing `verdict` JSONB (the `StatusError` per-validator status
11.1 reserved is now produced by the skip path); the `Chain` runner lifts them
from a validator's single-result verdict into the chain verdict.

**`on_error` policy.** `fail` (default) → a classified `*validate.Error` (a
transport failure of the validation stage): a provider availability failure is
transient (retried), a malformed answer permanent (the same model reproduces
it); routed through `completeFailure`. `skip` → a **pass** verdict whose sole
`Results[0]` records `status: error` + the message — an unavailable judge never
blocks a workflow.

**Cost as overhead (ADR-012 rule 4, exercised).** Pure `engine.priceOverheads`
reads judge usage off the verdict's per-validator results and applies
`overhead: true` `cost_ledger` rows under a `judge:<chain index>` entry (the
same-attempt PK slot migration 0016 reserved — no schema change) inside the
same completion transaction on every verdict-carrying route: `completeSuccess`
(a cache **hit** re-judges, so its overhead is real spend though the productive
row is $0 saved), `completeValidationFailure` (both the semantic-retry and the
terminal DLQ branch), and `completeFailure` (the malformed-under-fail case,
`judge:e`). Each row is priced under `PolicyEstimate` (post-call pricing never
fails a completed step; an unknown judge model → fallback + `cost_unknown_model`
warning), bumps `runs.spent_nano_usd`, emits `cost_updated`, and records
`engine_cost_spent_usd_total{resource=<judge model>}` — so judge spend counts
against the run budget on the **next** claim's projection and against a step
`max_usd` cap (`SumByStep` sums all entries). Cost API: `entries[].overhead`
under the `judge:*` entry + a new per-step `overhead_nano_usd` roll-up
(`AggregateCostByStep`, sqlc-regenerated) + `CostByStepView.overhead_nano_usd`/
`overhead_usd`; OpenAPI `ValidatorResult` gained `rationale`/`usage`/`error`, a
new `ValidatorUsage` component, and `CostByStepView` the overhead fields (still
lint 100/100).

**Pre-existing gap closed (asked for).** Before 11.5, a step whose executor
**succeeded and billed** but whose validation chain then **errored** (a judge
under `on_error: fail`) dropped the productive attempt's usage and cost —
`completeFailure` never saw the output. It now takes the executor `exec.Output`
(every mechanical-failure caller passes `exec.Output{}`, a no-op there since an
errored provider call carries no usage): the failing attempt records its usage
(`RetryStepArgs` gained a `Usage` field threaded through `finishAttempt`;
`DeadLetterStepArgs` already had one) and ledgers its productive cost on both
the retry and DLQ branches, and the judge's own billed call (a malformed answer
that spent money) rides its usage on `*validate.Error.Usage` and is metered as a
`judge:e` overhead row. So **every** judge spend is metered regardless of
outcome; only a provider error that billed nothing ledgers nothing.

**Guardrails & limitations.** Judges are terminal — a judge has no `validation`
envelope, the chain never recurses, and a judge is not a step (a judged run has
one step, not two). Two accepted limitations documented (ADR-013/010): judge
calls bypass the M9 fleet rate limiter (the validate stage is not routed
through the `ResourceClaimer` middleware), and judge overhead is metered after
the fact rather than pre-projected at claim (the bounded-overshoot tolerance
concurrent fan-out already accepts). The rubric is static (un-templated).

**Wiring.** `validate.NewBuiltins` gained a `*llm.Registry` parameter — the
`exec.Builtins` precedent; `cmd/worker` passes the provider registry the llm
executor already builds, `cmd/api` passes the one it builds for the plugin
listing (so `GET /v1/plugins` describes `llm_judge` with its config schema even
though the API never runs it). `internal/validate` now imports `internal/llm`
and `internal/jsonrepair` (no cycle).

**Tests.** `internal/validate/llm_judge_test.go` — a full unit matrix
(pass/fail/boundary at `score == threshold`, native structured + fenced-text
answers, malformed under fail/skip, provider error under fail/skip, the
fallback chain serving after a primary error, ctx-cancel passthrough, a
request-shape golden, and the `CompileConfig` matrix). `internal/engine/
llm_judge_integration_test.go` — the headline fail→feedback→pass e2e (2
productive + 2 `judge:*` overhead ledger rows, run aggregate = ledger sum, the
rationale carried into attempt 2's feedback, one step not two); `on_error:fail`
metering the productive **and** judge spend then dead-lettering; `on_error:skip`
degrading to a pass with an `error` result and no overhead. `internal/api/
cost_integration_test.go::TestRunCostJudgeOverhead` — the overhead surface on
the wire (`entries[].overhead`, `by_step[].overhead_nano_usd`). Canonical
`examples/definitions/llm_judge.json` (corpus-pinned). ADR-013 §"LLM-judge
validator (as built, 11.5)"; ADR-012 rule 4, ADR-009 flag table, ADR-010
amended; `docs/plugins.md` updated.

**Deferred:** quality metrics — validation-failure rate by step type/model/
validator, semantic-retry depth histogram, repair rate, judge score
distribution, per-step verdict summaries, the Grafana "output quality" panel
(11.6, closing M11).

### 11.6 — Quality signals & metrics ✅

Surfaced the output-validation health M11 had been persisting all along —
verdicts, structured-output provenance (11.3), semantic depth (11.4), and judge
scores (11.5) — as Prometheus metrics, a per-step status-API roll-up, and a
Grafana "Output quality" row. **Closes M11.** Purely additive: **no migration,
no config var, no store change, no new outcome/class** — every signal reads off
state 11.1–11.5 already record.

**Five instruments, one new `validate` subsystem.** In
`internal/obs/metrics/instruments.go` on the 7.2 instance registry, added to
`engine.Metrics` (+ `nopMetrics`): `engine_validate_verdicts_total`
`{step_type,resource,status}` (pass/fail chain verdicts, by step and the
resolved resource — the failure-rate signal by step & model);
`engine_validate_validator_results_total{validator,status}` (per-validator
pass/fail/skipped/error, one per configured validator per verdict);
`engine_validate_semantic_depth_attempts{outcome}` (histogram, buckets 1..10 =
`MaxSemanticAttempts`, observed once per terminated loop under `succeeded` or
`validation_failed`); `engine_validate_repairs_total{status}` (native/raw/
repaired/unrepairable, one per productive `output_format` llm attempt);
`engine_validate_judge_score_ratio{validator}` (histogram, buckets 0.1..1.0 =
the judge score distribution). `resource` is the pricing-catalog name or the
constant `none`; `validator` is the compiled-in validator catalog — both
bounded. ADR-008 amended: subsystem `validate`, label `validator`, the new
`_attempts`/`_ratio` histogram unit suffixes, five inventory rows; the
conformance `exercise` and unit-suffix rule extended, the stale `outcome` row
corrected (it was missing throttled/budget_exceeded/validation_failed).

**Engine recording** (`engine/validate.go` helpers `recordVerdictQuality` +
`recordRepairQuality` + `qualityResource`, all **post-commit** like the 10.5
cost metrics, so a fenced/rolled-back completion records nothing).
`completeSuccess` (its signature grew `semanticAttempt`; both callers —
`claim.go` and the cache-hit path in `cache.go` — pass it) records the passing
verdict, per-validator results, judge scores, repair provenance (skipped on a
cache hit — the miss counted it), and one `succeeded` depth sample.
`completeValidationFailure` records the failing verdict + results + scores +
repair on **both** the semantic-retry and terminal routes, and the
`validation_failed` depth sample **only** at the terminal dead-letter (not on an
intermediate re-attempt). `completeFailure` records repair provenance on its
retry/DLQ branches (the 11.5 judge-under-`on_error:fail` case carries a shaped
output from a successful executor call). **Two deliberate counting decisions,
documented in ADR-013:** a cache-hit re-validation that *fails* is a bypass +
re-execute (the fresh execution's verdict is counted, not the stale one); a
validator *transport* error is already visible via
`engine_step_retries_total`/`dead_letters_total`, so no separate counter — the
verdict counters count judgments, not stage failures.

**Status API.** `StepView.validation` (`ValidationSummaryView` +
`ValidatorSummaryView`): `summarizeVerdicts` in `internal/api/runs.go` rolls the
step's `attempts[].verdict` (ascending order, last-wins; a corrupt verdict row
is skipped, never a 500) into `{attempts, passed, failed, last_attempt,
last_status, last_score?, last_issue_count, validators[]{name, passed, failed,
skipped, errored, last_status, last_score?}}`, ordered by the latest verdict's
chain order. Present only when a step carried a chain. `internal/api` now imports
the `internal/validate` leaf in production (no cycle). OpenAPI gained
`ValidationSummary` + `ValidatorSummary` (spec still lints 100/100);
`docs/api.md` gained the walkthrough.

**Observability.** Engine dashboard **Output quality** row (verdicts/s by step
type & status, failure ratio by resource, validator results/s, semantic-depth
p50/p95 by outcome, repair status share, judge score p50/p90) — anti-drift
audit green (`normalizeMetricRef` already strips `_bucket`). Dev-scale
`ValidationFailureRatioHigh` alert (guarded by a minimum verdict rate so an idle
stack cannot fire it) + promtool unit tests (positive + all-passing negative),
`make obs-lint` green. `make smoke-metrics`/`smoke-dashboards` drive a
mixed-quality offline workload (a `contains` pass; a `contains` fail under a
3-attempt budget → DLQ; an `output_format: json` native echo; a `repair_only`
prose → unrepairable) asserting the verdict/validator/depth/repair series
non-empty; the judge-score panel is allowlisted quiet (the unscripted mock
emits no parseable judge verdict).

**Tests.** `internal/engine/quality_metrics_integration_test.go` — a reusable
metered fixture over four scenarios: the fail→fail→pass semantic loop (verdicts
pass=1/fail=2, per-validator results, one `succeeded` depth sample of 3);
exhaustion (one `validation_failed` depth sample of 2); a repaired
`output_format` step (`repairs_total{repaired}`=1, implicit-schema pass); and a
scripted llm_judge (two `judge_score_ratio` samples at 0.2/0.9, per-validator
fail+pass). `internal/api/validation_summary_test.go` — the pure summarizer
(nil when unvalidated, roll-up, score + skipped/error statuses, corrupt-row
skip). `TestSemanticRetryVisibleInStatusAPI` extended to assert the wire
`validation` summary — the "run status shows verdict summary per step" contract
test. Conformance + anti-drift audits extended. `make lint`, `make
openapi-lint` (100/100), `make obs-lint`, and — against the compose DB at
schema 20 — the store/engine/api integration suites green.

**Accepted limitation (documented, ADR-013):** the unscripted compose mock
cannot emit a parseable judge verdict offline, so the judge-score histogram is
exercised only by `TestQualityMetricsJudgeScore`, and its dashboard panel is
allowlisted quiet in the dashboard smoke. ADR-013 §"Quality signals & metrics
(as built, 11.6)"; ADR-008 amended. **M11 complete.**

## Milestone 12 — Context & memory management

### 12.1 — ADR-014 & token counting ✅

Opened M12 with **ADR-014** (context & memory model) and the token-counting
primitive `internal/tokens` the rest of the milestone rests on. Like the other
milestone-opener contract tickets (9.1/10.1/11.1), this is deliberately narrow:
a new leaf package + ADR, **no migration, no config var, no engine/executor
change**. The M9.2 chars/4 estimator stays in place — 12.6 swaps it for these
counters ("refines M9.2's token estimator with real counts").

**Why counting comes first.** You cannot set a context budget, assemble-to-a-
cap (12.3), compact-until-under (12.4/12.5), or guard a provider window (12.6)
without knowing, *before the call*, how many tokens a request will cost. A
chars/4 heuristic is 20–40% off on code/JSON — far outside the ±5% the
milestone demands, and a single under-count is a hard `400
context_length_exceeded` that 12.6 exists to prevent. So the counter is a
pre-execution primitive, model-aware, deterministic, and offline.

**Package shape.** `internal/tokens` is a leaf: it imports `internal/llm` (for
the unified `ChatRequest`/`Message`/`Block`/`ToolDef` shapes it counts) plus
stdlib and the tiktoken library, and no other agentloom package — so
`exec`/`engine` and the coming `internal/contextmgr` can depend on it without a
cycle (the `internal/cost`, `internal/cache`, `internal/limits` precedent).
`Counter` is `ID() string` + `Count(text string) int` + `CountRequest(req
llm.ChatRequest) int`; every method is pure — no clock, no network, and no
logging on the count path (selection logs a fallback once).

**Four families.**
- **openai** (`openai.go`) — exact BPE via `github.com/pkoukk/tiktoken-go`. The
  rank tables come from `github.com/pkoukk/tiktoken-go-loader`'s `go:embed`'d
  offline loader, installed once via `tiktoken.SetBpeLoader` in `bpe.go`
  (`offlineLoaderOnce`); the default network loader is never used. Encoding is
  chosen by model prefix (`bpe.go` `openAIEncodingFor`): `o200k_base` for the
  current families (gpt-4o/4.1/4.5, gpt-5, o1/o3/o4), `cl100k_base` for legacy
  gpt-4/gpt-3.5, defaulting o200k for an unrecognized OpenAI model. For text
  this is OpenAI's real tokenizer; only the chat framing is an approximation.
- **anthropic** (`anthropic.go`) — an *estimate*, because Claude publishes no
  tokenizer: counts with the o200k BPE (the closest public reference) ×
  `AnthropicCalibrationFactor` (a pinned constant, default seed 1.15), rounded,
  never below 1 for non-empty text. The gated recorder re-derives the factor by
  Σrecorded/Σbase over the corpus; `TestAnthropicCalibrationFactorMatchesCorpus`
  guards >0.5% drift (skips while the corpus is pending). Optional
  `count_tokens` API use is a documented, off-by-default, *not-yet-implemented*
  seam — the default path is pure and offline.
- **mock** (`mock.go`) — mirrors `internal/llm`'s mock provider input estimator
  **exactly**: `len(mockFlattenPrompt(req))/4 + 1`, where `mockFlattenPrompt`
  replicates the mock's (unexported) `flattenPrompt` (system + each message's
  text and tool_result content, each with a trailing newline). So a mock-driven
  request's counted total equals the mock's reported `Usage.InputTokens` —
  asserted by `TestMockCounterMatchesProviderUsage`. That equality is what lets
  M12's mock-driven exit fixture run offline in CI with *real* accuracy
  assertions rather than a fudge factor.
- **fallback** (`fallback.go`) — `ceil(len(text)/4)`, ≥1 for non-empty. The
  last resort for an unknown model; approximate but better than refusing to
  count.

**CountRequest framing.** The shared `countRequest` walk (`tokens.go`) frames
the whole request, not bare text: reply priming (once) + per-message role
framing + text / tool_use-input-JSON / tool_result-content blocks + per-tool
(name + description + input-schema JSON + a constant) + the structured-output
schema when present. Counting concatenated text alone would miss ~3–15 tokens
of framing per message and drift a multi-turn conversation past ±5%. The
constants are family-scoped (`openAIFraming` = perMessage 3 / perTool 8 / reply
3, following the documented OpenAI chat accounting; `anthropicFraming` the same
shape, with the calibration factor absorbing the aggregate difference).

**Selection & once-logged fallback.** `Registry.Select(provider, model)`
(`registry.go`) takes the **resolved** provider — the one `llm.Registry.Resolve`
returns; routing is not re-implemented here — and returns the counter plus a
`Selection` (family, provider, model, fallback bool). An unknown model gets the
chars/4 fallback, warned **once per (provider, model) per process** via a
`sync.Map` of `sync.Once` (never per call, ADR-014). A nil logger disables the
warning; the counters themselves never log. An internal encoding-load failure
(a programming error, not a runtime condition) also falls back rather than
panicking, so counting can never crash a step.

**Determinism & fingerprints.** Counters are pure functions of their input and
embedded tables. `ID()` is a stable fingerprint (`openai/o200k_base@1`,
`openai/cl100k_base@1`, `anthropic/estimate@1;factor=1.15`, `mock/estimate@1`,
`fallback/chars4@1`), changing only when the tokenization changes (encoding
swap, calibration re-derivation, or a framing-constant change bumps the `@N`).
12.2/12.3 will persist `(token_count, counter_id)` and recompute on mismatch —
so a stored count is never silently stale.

**Non-obvious decisions.**
- *Ground truth without a separate endpoint.* The recorder uses the provider's
  own `Usage.InputTokens` on a tiny real Chat call as ground truth — exactly
  what the provider billed for the prompt — so it needs no Anthropic
  `count_tokens` endpoint and works uniformly for both vendors.
- *OpenAI ±5% is genuinely offline.* tiktoken *is* OpenAI's tokenizer, so the
  six framed fixtures' reference counts (tiktoken content + documented framing)
  equal the API's; the accuracy test passes offline, backed by hand-verified
  exact small cases (`TestOpenAICountRequestExact`) and an independent framing
  walk (`refCount`).
- *Anthropic ±5% is honestly pending.* Claude has no public tokenizer, so the
  three Anthropic fixtures ship `source: "pending"` (count 0) and the accuracy
  assertion skips-with-notice until the gated recorder fills them; the ADR's
  default-budget headroom (5%) is sized to absorb the calibration slop.
- *Offline proven, not assumed.* `TestCountersAreOffline` clears the encoding
  cache and installs an `http.DefaultTransport` tripwire, forcing a cold rank
  reload that must not touch the network (the 8.5 mock-provider precedent).

**Tests.** `internal/tokens/*_test.go` — exact tiktoken cross-checks and
hand-verified framed totals; tools/schema framing vs an independent walk;
Anthropic scale-by-factor; fallback arithmetic; determinism under 64 concurrent
counts; stable-ID pins; the ±5% accuracy suite over the fixture corpus (OpenAI
green, Anthropic skip-until-recorded) + aggregate-error check; the calibration
drift guard; counter selection by (provider, model) incl. legacy vs current
OpenAI encodings and unknown→fallback; fallback-logged-once (serial + 100-way
concurrent + nil-logger); the mock-matches-provider-usage property; the offline
tripwire; a `BenchmarkCountRequest`; and the gated `TestRecordTokenCorpus`
(LIVE_LLM_TESTS=1 + key + `-record`). `make lint` green (two `#nosec G304`
test-only fixture reads); full `go test ./...` + `-race` on `internal/tokens`
green.

**New deps:** `github.com/pkoukk/tiktoken-go` v0.1.8 + `.../tiktoken-go-loader`
v0.0.2 (~6.7 MB embedded BPE tables in the binary; `dlclark/regexp2` pulled in
transitively). **Accepted cost, documented in ADR-014:** exact OpenAI counting
is worth it, and the tables are embedded so there is no runtime download.

**Deferred to later M12 tickets (contract fixed in ADR-014, code later):** the
blackboard store with `token_count` columns (12.2), declarative context
assembly + the byte-identical-assembly goldens (12.3), the deterministic
compaction strategies + `context_revision` audit (12.4), summarization
compaction billed as overhead (12.5), and the provider-window guardrail that
swaps the M9.2 estimator for these counters + the `context_utilization` metric
(12.6). ADR-014 §Decision fixes sources/precedence/pinning, the default budget
(`window − max_tokens − headroom`), the strategy-pipeline semantics and hard
≤-budget guarantee, and the determinism/audit requirements those tickets
conform to, with worked compaction examples.

### 12.2 — Blackboard store ✅

Delivered the run-scoped **blackboard** — the shared, versioned key/value
memory steps read and write during a run (ADR-014). It is the source of
12.3's `blackboard` context source, the substrate M14's multi-agent handoffs
build a thread on, and where 12.5's summaries will land as their own
entries. 12.2 ships the store, the executor-facing read/write API, the
declarative write sugar, and the read API.

**Package shape.** `internal/blackboard` is a **leaf** (stdlib only): the
domain types (`Entry`, `PutArgs`, the `Board` interface), the key/tag/value
rules (`ValidateKey`/`NormalizeTags`/`ValidateValue`, `TagPinned`), the
typed errors (`*VersionConflictError`, `ErrFenced`, `*InvalidKeyError`,
`*InvalidTagError`, `*ValueTooLargeError`), and the shared helpers
`CanonicalValue` (compact JSON — the determinism the stored count rests on)
and `ResolvePointer` (RFC-6901 into a doc). So `exec`, `engine`, and the
coming `internal/contextmgr` depend on it without a cycle (the
`internal/cache`, `internal/limits` precedent). The Postgres implementation
is the subpackage `internal/blackboard/pgboard` (the only importer of
`internal/store`), the `retrieval`/`pgfts` precedent — keeping the SPI a leaf.

**Schema (migration 0021).** `blackboard_entries` is **append-only per key**:
a write never overwrites, it inserts the next `version` (1-based per
`(run_id, key)`), so `History` reconstructs every revision — the audit
ADR-014 requires. A row carries `value` (JSONB), `token_count` +
`token_counter` (the `tokens.Counter.ID()` that produced the count, stored
together so a consumer recomputes on a fingerprint mismatch), `tags`
(`TEXT[]`, GIN-indexed, **per-version and immutable** — re-tagging/pinning is
a new version, so the history stays honest), and the author step + attempt.
`run_steps.blackboard_policy` (nullable JSONB) materializes a step's
declarative-write block at instantiation, like `cache_policy`. Retained past
the run for audit; pruned in M21. The migrate round-trip test pins 21 and
asserts the table/column drop.

**Write protocol.** `store.PutBlackboardEntry` is a transition-style helper
(the `ApplyAttemptCost` template): inside the caller's transaction it takes
the run lock (existence + the uniform run→step ordering, and the
serialization that makes version allocation collision-free), optionally
**fences** on the authoring step's `claim_id` (a taken-over zombie's write is
rejected — unlike the 5.5 journal's first-wins result), evaluates an optional
compare-and-swap guard (`ExpectedVersion`, 0 = "must not exist") against the
current head, inserts `head+1`, and appends a `blackboard_updated` event
(`store.EventBlackboardUpdated`, payload `BlackboardUpdatedEvent`) — all
atomically. A rejected CAS (`*store.BlackboardVersionConflict`, unwraps
`ErrConflict`) or fence (`store.ErrBlackboardFenced`) writes nothing, burning
no event seq. Because every writer holds the run lock, two concurrent
**unconditional** writers of one key never collide — they serialize into v1
and v2, no lost update (asserted end-to-end). `store.BlackboardRepo` serves
the reads (`Head`/`History`/`ListHeads`/`ListHeadsPage`/`ListVersionsPage`);
the heads queries use a `DISTINCT ON (key)` CTE so a tag filter applies to
the *head*, not a superseded version.

**Executor API.** `StepContext.Blackboard` is a step-scoped `blackboard.Board`
(programmatic `Get`/`History`/`List`/`Put`), bound in `claim.go` beside
`Effects`, attributed to the step and fenced on its claim. The `pgboard`
handle validates against the leaf rules, canonicalizes + token-counts the
value, routes through `store.PutBlackboardEntry` in its own short
transaction (the effects-journal model), and **translates** the store's typed
errors back into the leaf's so the engine's classifier sees one vocabulary. A
CAS conflict is **transient** (the retry re-reads the head and can win at the
next version); a fence/bad-input is **permanent**. Token counts use the
step's counter via the new `exec.TokenCounterProvider` hook — the llm
executor resolves the model's tokenizer (`tokens.Registry.Select` over the
resolved provider), every other step gets `tokens.Fallback()` (chars/4); the
engine's `stepCounter` helper picks it. `tokens.Fallback()` was exported for
non-model contexts.

**Declarative writes.** A step's `blackboard` envelope block declares
`write: [{key, from, tags, pinned}]` — on success, the value at the `from`
RFC-6901 pointer into the step's *own* output is published under `key`
(`from: ""` = whole output; `/text` for an llm step). Applied **in the
completion transaction** (`completeSuccess`), after the success CAS and under
the run lock it holds — so a fenced zombie completion never writes, and the
entry is durable exactly with the step's success. Planning (`planBlackboardWrites`,
pointer resolution) is pure and pre-transaction; a pointer that does not
resolve is a permanent step failure (ADR-006 row 15), carrying the output so
its productive cost still ledgers. A cache-hit completion applies the writes
too (same output). Gated on a wired board (`WithBlackboard`) so test layers
that don't opt in see no blackboard behavior. `completeSuccess` grew a
`tokens.Counter` param; both call sites (`claim.go`, the cache-hit path in
`cache.go`) pass `e.stepCounter(executor, sc)`.

**Cache-key note (documented).** Programmatic blackboard reads during
execution are *not* cache-key inputs (the key is built before execution), so
a step depending on blackboard state must be uncacheable (the
`blackboard_write` test executor is `side_effectful`) or receive that state
through the config (templating / 12.3 assembly, both *in* the key). ADR-014
records this; 12.3's declarative sources are assembled pre-cache and keyed
correctly.

**dag contract.** `Step.Blackboard *BlackboardPolicy` (strict-decoded, the
`"blackboard"` key added to the envelope switch), `checkBlackboard`
validation (codes `blackboard_field_required`/`blackboard_field_invalid`, ≤16
writes, keys distinct + valid grammar, `from` pointer syntax, tag grammar —
sharing the leaf's `ValidateKey`/`NormalizeTags` so they cannot drift). New
step type `blackboard_write` (test-only, in `Builtins` not `CoreBuiltins`,
gated behind `AGENTLOOM_WORKER_TEST_EXECUTORS` like counter/effectful_echo) —
config `key`/`value`/`tags`/`expected_version`/`read_key`, its executor
exercising the programmatic read/write + CAS. Regenerated JSON Schema, both
kitchen sinks + a bad fixture (`blackboard_bad.json`) + a decode fixture.
**Declarative reads into a prompt are 12.3's context sources** — 12.2 ships
writes + the programmatic API; the `blackboard.<key>` template read root is
deferred with them (only useful once assembly consumes it), keeping this
ticket out of the template engine.

**API.** `GET /v1/runs/{id}/blackboard` (read scope/class): each key's head
by default, `?history=true` for every version, `?key=`/`?tag=` filters (tag =
AND), opaque keyset cursor. `BlackboardResponse`/`BlackboardEntryView`,
OpenAPI (still lint 100/100), route→scope/route→class/auth-matrix/coverage
tables extended. Read-only — the API never writes the blackboard (steps do,
in the worker), so ADR-002 is untouched. `ctl blackboard <run-id> [--name
--tag --history --limit]` (the key filter is `--name`, not `--key`, since
`--key` is the global bearer flag).

**Wiring.** `engine.WithBlackboard(*pgboard.Board)` + `WithTokenCounters`
(built in `New` with the engine's logger so a chars/4 fallback warns once);
`cmd/worker` builds the board over the shared store and wires it (always-on).

**Tests.** Leaf unit suite (key/tag/value rules, pointer, canonicalize);
store integration (versioning, tag queries incl. the superseded-tag case,
CAS conflict writes-nothing, distinct-key parallel, fence); pgboard
integration (token count on the canonical form + counter fingerprint, pinned,
CAS-translation, invalid-inputs-permanent); exec unit (executor write/read,
CAS→transient, no-board→permanent, the llm `TokenCounter` hook); engine
integration (`TestBlackboardFlow` — distinct-key parallel writes both v1, a
declarative pinned llm write counted with the model counter, a downstream
CAS-success at v2 with `read_key`; `TestBlackboardParallelSameKey` — two
unconditional same-key writes both land, history intact; `TestBlackboardCASConflictDeadLetters`
— an impossible `expected_version` transient-retries then dead-letters,
nothing written); api integration (heads/history/key/tag filters,
pagination, 400/404). Whole `go test ./...` + the integration suites green
against the compose stack.

**Non-obvious decisions.**
- *Fence semantics differ from the effects journal.* A blackboard write is
  claim-fenced (a zombie must not mutate shared memory the new holder owns);
  the 5.5 journal's *result* write is first-wins (step-level fencing already
  rejects the zombie's outcome). Declarative writes ride the success CAS's
  fence, so they need no fence of their own; programmatic writes fence
  explicitly.
- *JSONB reformats deterministically.* Postgres normalizes a stored JSONB
  value (spaces after colons), so the returned bytes are not the canonical
  form — but the `token_count` is computed on `CanonicalValue` before insert,
  so it is deterministic regardless, and JSONB's reformat is itself
  deterministic (byte-identical assembly in 12.3 still holds).
- *`blackboard_write` is test-only.* A production deployment publishes to the
  blackboard through the declarative envelope, not an arbitrary-write step;
  the executor exists to drive the integration tests, so it lives in the
  opt-in `Builtins` set (the split test became `+3`, generalized from
  "filesystem" to "test-only").
- *Declarative reads are 12.3.* ADR-014 assigns blackboard-as-context-source
  to context assembly; 12.2 stays out of the template engine, delivering the
  three done-when boxes (store tests, parallel/CAS behavior, the read
  endpoint) without ballooning.

**Deferred to later M12 tickets:** the `blackboard` context source +
byte-identical assembly (12.3), deterministic compaction respecting `pinned`
(12.4), summarization writing summary entries (12.5), provider-window
guardrails (12.6). ADR-014 §"Blackboard store (as built, 12.2)"; ADR-004
(table + `blackboard_updated` event), ADR-003 (`blackboard` envelope + reads-
are-12.3 note), ADR-006 (CAS=transient / fence=permanent), ADR-007 (route
row) cross-amended.

### 12.3 — Context assembly ✅

Delivered declarative **context assembly**: an llm-family step declares an
ordered `context` spec whose sources — upstream step outputs, blackboard
entries by key/tag, retrieval results, inline literals — are assembled into
the provider request **before the call**, with per-source token caps and
pinned entries always included, deterministic given store state (ADR-014).
This is the `blackboard` context source 12.2 deferred, plus the other three
kinds. 12.3 ships the contract, the assembler leaf, the engine middleware,
the pre-flight token total, and the audit record; compaction (12.4) and the
window guardrail (12.6) build on the counted output.

**Contract (`internal/dag`).** New `context.go`: `ContextSpec{Sources
[]ContextSource}` (`MaxContextSources = 32`), `ContextSourceKind`
(`step_output | blackboard | retrieval | literal`), `ContextMissingPolicy`
(`error` default | `skip`). `Step.Context *ContextSpec` appended to the
envelope (struct order = canonical order); `decodeContext` branch + the
`"context"` known-key case (closed enums checked at decode like `decodeCache`).
`checkContext` (in `checkSteps`) enforces the shape: llm-family only (the
`checkSemanticPolicy` idiom), non-empty ≤ 32, per-kind required/forbidden
fields, name uniqueness, key/tag grammar (shared with the `blackboard` leaf),
`Path`/pointer syntax, `top_k` ≤ 100, `pinned`+`max_tokens` conflict, and a
**no-templating** rule (`${{ }}` inside the block is `context_field_invalid`).
The upstream-ancestry of a `step_output` source is a second pass
(`checkContextGraph`, in the graph-valid gate beside `checkTemplates`) through
the **extracted shared** `classifyUpstreamRef` — the same three-way classify
(unknown / self / not-ancestor) the template lint now also uses, so the two
cannot drift. New codes `context_field_required`/`context_field_invalid`;
`JSONSchema()` for both enums; regenerated `workflow-definition.v1.json`.

**Assembler leaf (`internal/contextmgr`).** Imports only leaves (`dag`,
`blackboard`, `retrieval`, `tokens`) + stdlib, so exec/engine depend on it
without a cycle. `Assemble(spec, counter, Sources) → Assembly` resolves each
source in declaration order (precedence = message order), renders it into a
stable `<context name=… kind=…>…</context>` block (JSON-string values render
as text, else compact canonical JSON; tag-selected entries wrap as
`<entry key=… version=…>`; retrieval docs as `<doc id=…>`), joins with blank
lines, and counts everything with the step's counter. A per-source
`max_tokens` cap truncates on a rune boundary with a fixed `…[truncated]…`
marker via a binary search over the **final** string (BPE counts are not
additive, so measuring the concatenation is the only way to guarantee ≤ cap);
`pinned` sources are never truncated. `Sources` supplies the store-backed
readers as closures/handles (a batched step-output map, the step's blackboard
`Board`, the retriever registry) so the leaf never imports `store`. Typed
errors: `*MissingSourceError` (default policy → permanent), `*ConfigError`
(unwired/unknown backend → permanent regardless of policy); anything else is
wrapped transport. Golden test (byte-exact four-kind preamble), determinism
(repeat → byte-identical, tag entries in key order), the missing→error/skip
matrix per kind, config-error matrix, transport-propagation, and a truncation
**property** test (≤ cap across content sizes × caps, multibyte content).

**Missing-source policy.** `on_missing: error` (the default, the 8.2
strict-reference stance) fails the step permanently before any provider call;
`skip` omits the source and records it on the audit event. "Missing" = an
upstream step that did not succeed, a `step_output` pointer that does not
resolve, a blackboard key with no head, a tag matching no heads, or a
retriever returning no results.

**exec hooks.** Two optional interfaces on `LLMExecutor`:
`ContextInjector.WithContext(sc, preamble)` prepends the preamble (a leading
user message for a messages-form config, a prompt prefix for a prompt-form
config — the `WithFeedback` raw-message re-key mirror, siblings byte-identical),
and `PreflightCounter.PreflightTokens(sc, counter)` counts the whole framed
request (`counter.CountRequest(buildChatRequest(cfg))`). Because the
prompt/messages feed the cache key, the resource estimate, and the provider
call, the one rewrite re-targets the whole pipeline — so the assembled context
is a cache-key input by construction (asserted at the exec layer:
`TestLLMExecutorContextChangesCacheKey`). The `llm.ChatRequest` type stays out
of the engine: preflight returns an `int`.

**Store.** Migration 0022 adds `run_steps.context_policy` (nullable JSONB,
materialized at instantiation like `blackboard_policy`; sqlc regenerated,
`instantiate.go` marshals `step.Context`). `store.RecordContextAssembled`
(the `RecordModelDowngrade` fenced-record precedent — run lock → fetch step →
require running + matching claim → `appendEvent`, no state transition, no new
table) appends the new `context_assembled` event
(`store.ContextAssembledEvent`: counter fingerprint, per-source
`ContextSourceRecord` dispositions, `context_tokens`, `preflight_tokens`).
Migrate round-trip test pins 22 (context_policy drops on the first Down,
blackboard on the second).

**Engine (`engine/context.go`).** `assembleContext` sits in `execute()`
**after feedback injection, before the cache read** (`claim.go`), so the
assembled preamble is prepended into the request the cache binding keys on. It
decodes the materialized spec (no spec → zero-read fast path; corrupt →
permanent), requires the executor to be a `ContextInjector` (else permanent),
builds the readers (one batched `ListByIDs` for step outputs, `sc.Blackboard`
for the board, `e.retrievers.Get` for retrieval), calls `contextmgr.Assemble`,
routes a `MissingSourceError`/`ConfigError` to a permanent completion and a
transport error to a redeliver, injects the preamble, computes the pre-flight
total on the augmented request, and appends the `context_assembled` event in
its own short fenced tx (a fenced caller abandons like `recordDowngrade`).
`engine.WithRetrievers(*retrieval.Registry)` wires the same registry the
retrieve executor uses; `cmd/worker` passes it. Headline integration
`TestContextAssembly` (the canonical `context_assembly.json` on the mock: the
assembled literal/step_output/blackboard reach the provider echo, the
retrieval source skips on an empty corpus, the event records the four
dispositions, and **`preflight_tokens` == the draft attempt's input tokens
exactly** — the ±5% accuracy criterion, exact on the mock); a determinism test
(two runs → byte-identical echo + equal context tokens); and a
missing-source-error test (a required retrieval source on an empty corpus →
DLQ permanent, no `context_assembled` event, no usage, no provider call).

**Fixtures.** Canonical `examples/definitions/context_assembly.json` (offline
mock, added to `exampleFiles`); a `context` block on both kitchen sinks + a
construct-pin block (`TestExampleKitchenSinkCoversEveryConstruct` — all four
kinds, pinned source, per-source cap, skip policy; plus the backfilled 12.2
pinned-blackboard-write assertion); `testdata/invalid/context_unknown_field.json`
+ `context_bad_kind.json` (decode) and `testdata/invalid_structural/context_bad.json`
(eight steps → eight distinct `context_field_*` issues).

**Non-obvious decisions.**
- *Envelope block, not a config field.* `context` applies to the whole llm
  family (llm/planner/agent — M13 planners want it) and materializes like
  `cache`/`validation`/`blackboard`; a config field would not reuse the
  materialization path and would not extend to planner/agent.
- *Assembly runs after feedback, before the cache.* After feedback so the
  pre-flight count reflects the final request; before the cache so the
  assembled content is keyed (ADR-011/014). On a first attempt (no feedback)
  the assembled config is what the cache keys on; a semantic retry is
  cache-bypassed anyway.
- *Recount, don't trust the stored blackboard `token_count`.* The rendered
  wrapper framing differs from the raw value; the honest number is the count
  over the assembled text. ADR-014's recompute-on-mismatch is thus trivially
  honored.
- *No templating in the block.* A dynamic query flows through a `retrieve`
  step + `step_output` source, keeping 12.3 out of the template engine (the
  12.2 stance); the `blackboard.<key>` template root stays deferred.
- *Pre-flight returns an int.* The `PreflightCounter` hook returns a count,
  not an `llm.ChatRequest`, so the engine never imports `llm`.

One migration (0022), no new config var, no new metric (the
`context_utilization` histogram is 12.6). ADR-014 §"Context assembly (as
built, 12.3)"; ADR-003 (`context` envelope), ADR-004 (column +
`context_assembled` event), ADR-006 (row 20: assembly failure permanent /
transport redeliver), ADR-011 (assembled context is a cache-key input)
cross-amended.

**Deferred to later M12 tickets:** deterministic compaction respecting
`pinned` when assembly exceeds the budget (12.4), summarization compaction
(12.5), provider-window guardrails swapping M9.2's chars/4 estimator for the
real counters (12.6).

### 12.4 — Deterministic compaction strategies ✅

Delivered **deterministic context compaction**: when an assembled context
(12.3) makes the framed request exceed a step's token budget, the step's
ordered strategy pipeline runs in the pre-execution path — `sliding_window`,
`truncate_oldest`, `drop_lowest_priority` — until the request fits or a typed
`ContextOverBudget` failure fires **before any provider call**. Pinned sources
are never dropped or truncated; every strategy application is audited as a
`context_revision` event. The summarizer strategy (12.5) and the provider-
window default budget (12.6) build on this. **No new migration** (the budget +
pipeline live in the `context` spec already materialized on
`run_steps.context_policy`), **no new config var, no new metric**
(`context_utilization` is 12.6).

**Contract (`internal/dag`).** `ContextSpec` grew `BudgetTokens *int`
(`budget_tokens`, positive) and `Compaction []CompactionStrategy`
(`compaction`, ≤ `MaxCompactionStrategies`=8, no duplicates);
`CompactionStrategyKind` (`drop_lowest_priority | truncate_oldest |
sliding_window`, plus the **reserved-and-rejected** `summarize` — the
`retry_on: validation_failed` precedent, rejected in `checkCompaction` until
12.5); `CompactionStrategy{Strategy, N *int, MinTokens *int}` with per-strategy
parameter rules (`sliding_window` requires `n ≥ 1`; `truncate_oldest` admits
`min_tokens ≥ 0`; a parameter on the wrong strategy is rejected);
`ContextSource.Priority *int` (default 0, `EffectivePriority()`). Decode grew a
closed-enum check for strategy kinds; validation is `checkCompaction` under the
existing `context_field_required`/`context_field_invalid` codes. Regenerated
`workflow-definition.v1.json` (new `CompactionStrategy` def + the three fields);
both kitchen sinks carry a budget + all three strategies + a priority (construct
pin extended); `testdata/invalid_structural/context_bad.json` grew seven
compaction cases (budget<1, missing/zero `n`, forbidden `n`/`min_tokens`,
reserved `summarize`, duplicate strategy) with exact expectations. A
`compaction` pipeline with no `budget_tokens` is admissible but inert until 12.6
defaults the budget.

**Assembler leaf (`internal/contextmgr`).** `Assemble` now also produces
`Assembly.Entries []Entry` (the rendered non-skipped sources — the compaction
unit; a source maps to exactly one entry so `Render(entries)` reproduces 12.3's
byte-identical preamble, which is how the 12.3 goldens survive unchanged). New
`compact.go`: `Compact(asm, Policy{Budget, Pipeline}, counter, MeasureFunc) →
Compacted` measures the **whole framed request** (`MeasureFunc`, supplied by the
engine as inject-preamble-then-`PreflightTokens`) each pass — the same
arithmetic 12.6's window guardrail uses, and necessary because BPE counts are
not additive. `sliding_window` (one-shot: keep last `n` non-pinned in message
order), `drop_lowest_priority` (evict one non-pinned at a time, lowest priority
then later-declared, re-measuring), `truncate_oldest` (middle-out truncate the
oldest non-pinned above the `min_tokens` floor with a fixed `…[elided]…` marker
— a binary search over the rune split guaranteed ≤ the local target — moving to
the next-oldest while over). Pinned entries are untouchable across every
strategy. On failure a typed `*OverBudgetError{Budget, Tokens, PinnedTokens,
PinnedOnly, Applied}` (`PinnedOnly` = the pins alone exceed the budget). Each
strategy that runs yields a `Revision` (strategy, params, tokens before/after,
per-entry actions, kept names). Tests: per-strategy tables, an
under-budget-noop, pinned-exceeds, empty-pipeline guardrail, determinism, a
Dropped-report check, and a **rapid property test** (arbitrary oversized
assemblies × pipelines → `FinalTokens ≤ budget` or `*OverBudgetError`, and every
pinned entry survives byte-identical).

**Store (`internal/store`).** New `context_revision` event
(`EventContextRevision`) + `ContextRevisionEvent` payload (strategy, params,
budget, tokens before/after, `ContextRevisionActionRecord[]`, kept names).
`RecordContextAssembledArgs` grew a `Revisions []ContextRevisionEvent` slice;
`RecordContextAssembled` appends them **before** the `context_assembled` event
in the same fenced tx (raw → revision* → assembled in seq order, one atomic
audit under the claim fence). `ContextAssembledEvent` grew `BudgetTokens`,
`RawContextTokens`/`RawPreflightTokens` (pre-compaction totals), and `Revisions`
(count); the `Sources` dispositions now include `dropped`. **No migration** —
revisions are events, the compaction spec is already on `context_policy`.

**Engine (`engine/context.go`).** `assembleContext` gained the compaction stage
after `Assemble`: if `spec.BudgetTokens != nil` it requires a `PreflightCounter`
(else permanent — a step that cannot count its request cannot enforce a budget),
builds the `MeasureFunc` (inject candidate preamble → `PreflightTokens`), and
calls `Compact`. An `*OverBudgetError` (or a measure-build failure) is a
**permanent** `contextError` → DLQ before any provider call (ADR-006 row 21),
the DLQ reason carrying the budget/pinned/applied summary. Otherwise the
compacted preamble is injected and the revisions + the extended
`context_assembled` event are recorded together. The no-budget path is 12.3
verbatim (best-effort preflight, zero revisions). The recorded post-compaction
`preflight_tokens` still equals the attempt's reported `Usage.InputTokens`
exactly on the mock.

**Fixtures & tests.** Canonical `examples/definitions/context_compaction.json`
(offline mock): a five-turn conversation, each turn written to the blackboard
under `turn_N` (tag `turn`), and a `summary` step assembling a pinned scribe
instruction + all five turns by key with `budget_tokens: 200` and the pipeline
`[sliding_window n=3, truncate_oldest min_tokens=32, drop_lowest_priority]`.
Integration tests (`context_integration_test.go`): `TestContextCompaction` (the
summary output carries the pin + recent turns but not the sliding-window-dropped
turn₁/turn₂; the event records a budget, ≥1 revision, a shrunk preflight ≤
budget with the raw total above, ≥2 dropped sources, an untouched pin, exact
preflight==input-tokens; the `context_revision` events are queryable per step
attempt, start with `sliding_window`, and report non-increasing tokens — the
audit contract test), `TestContextCompactionDeterministic` (two runs →
byte-identical output + equal revision count), and
`TestContextOverBudgetPinnedDeadLetters` (two pinned literals over a budget of 5
→ DLQ permanent, no `context_assembled` event, no usage, no provider call).

**Non-obvious decisions.**
- *Budget over the whole framed request, not the preamble.* ADR-014's identity
  "assembled ≤ budget" == "assembled + max_tokens ≤ window" requires measuring
  the same number 12.6 guards; compaction shrinks the only movable part (the
  preamble) and re-measures the request via the executor's `PreflightCounter`.
- *Entry = source (1:1).* A tag/multi-doc source renders as one block, so
  strategies operate at source granularity and `Render` reproduces the 12.3
  byte-identical preamble unchanged (goldens survive).
- *Re-measure each pass.* BPE counts are non-additive, so a per-entry sum would
  be wrong; each strategy re-measures the framed request rather than trusting a
  running total.
- *No migration.* The budget + pipeline extend the `context` spec already
  materialized on `context_policy`; revisions are events. Compaction adds no
  column.
- *Failed-attempt revisions not persisted.* An over-budget failure's summary
  rides the DLQ reason (the dead-letter is the record); per-revision events are
  written only for a productive attempt — a documented limitation.

One ADR (§"Deterministic compaction (as built, 12.4)" + worked examples);
ADR-003 (`context` block compaction fields), ADR-004 (`context_revision` event),
ADR-006 (row 21) cross-amended.

**Deferred to later M12 tickets:** summarization compaction billed as overhead
(12.5), provider-window guardrails swapping M9.2's chars/4 estimator for the
real counters + the `context_utilization` histogram (12.6).

### 12.5 — Summarization compaction ✅

Added the fourth compaction strategy, `summarize` — the only one that is not a
pure function. It replaces the oldest evicted non-pinned span with a
cheap-model summary written to the run blackboard, chaining across steps through
the summary key's version history. It is the last compaction strategy to build
because it needs the whole M12 stack (counter, blackboard, assembly, the
deterministic strategies) plus the M10.2 cost ledger.

**Contract (`internal/dag`).** Un-reserved `summarize` on the `context`
envelope's `compaction` pipeline. `CompactionStrategy` grew four summarize-only
fields: `model` (required, routed through the llm registry like a step's model —
routability checked at claim pre-flight, not submit), `key` (the blackboard key
the summary lands under; default `context_summary`), `max_tokens` (the summary
completion bound; default 256), and `timeout` (per-call deadline; default 60s,
≤ 10m). `checkCompaction` validates them and rejects each on a non-summarize
strategy (and `n`/`min_tokens` on summarize); at most one summarize per pipeline
(the no-duplicate rule). Effective-value helpers default the unset fields. No
migration — the fields materialize on the existing `run_steps.context_policy`
with the rest of the spec. Regenerated JSON Schema; both kitchen sinks + the
construct-pin test gained a summarize strategy; `context_bad.json` grew five
summarize cases (missing model, bad key, bad timeout, zero max_tokens, model on
a non-summarize strategy).

**Strategy + summarizer (`internal/contextmgr`).** New `summarize.go`: the
`Summarizer` interface (`Summarize(ctx, SummarizeRequest) (SummaryResult,
error)`), the concrete `LLMSummarizer` (routes `req.Model`, one temperature-0
`Chat` under an optional per-call `context.WithTimeout`; a per-call timeout is an
ordinary error the caller falls back on, a **parent-context cancellation passes
through unwrapped** so the engine keeps its timeout/cancelled judgment; an empty
summary is an error), the deterministic `SummaryChatRequest` (a fixed system
prompt, the span as a **leading** user turn, a fixed instruction as the trailing
turn — the mock echoes the short trailing instruction, so the offline fixture
shrinks without scripting while a real provider gets material-then-task), and the
`SummaryValue`/`SummaryAction` types + `SummarizerVersion`. In `compact.go` the
`summarize` strategy gathers the oldest non-pinned live entries oldest-first
until their token sum reaches `over + max_tokens` (or the set is exhausted),
renders the span, calls the summarizer once, writes a versioned `SummaryValue`
to the blackboard (`parent_version` = the prior head → the key's history **is**
the chain), drops the span (recorded as `summarized` actions), and **splices a
synthetic `summary`-kind entry** into the working `entries` at the span's oldest
position — so it re-renders where the oldest evicted turn was and can be folded
again (chaining within a pipeline, and across steps that read the running summary
via a `blackboard` source). A summary is committed only if it is genuinely
smaller than its span (progress guard). The compaction working set was
refactored from source-index ordering to **message-position ordering** (so a
spliced synthetic entry orders correctly among authored ones — behavior-identical
for the no-splice case the 12.4 tests exercise). `Compact` gained a `ctx` param
and `Policy{Summarizer, Board}`; `Revision` gained `Summaries []SummaryAction` +
`Error`; `OverBudgetError` gained `Revisions`/`Summaries` (so the engine can
audit and bill a summarizer that ran before the pipeline gave up); `Compacted`
gained `Summaries`; disposition `Summarized` added.

**Determinism restored operationally.** A model call breaks the pure-function
property the deterministic strategies have. Two mechanisms restore it: the fixed
temp-0 prompt, and ADR-011 caching. The engine's `cachedSummarizer`
(`engine/summarize.go`) decorates the summarizer with read-through/write-through
over the response cache, keying the deterministic `ChatRequest` under a dedicated
`context_summarizer` plugin namespace at `SummarizerVersion`, **global** scope; a
hit returns the stored summary marked `CacheHit`. Every cache error is fail-safe
(unbuildable key / corrupt entry / store error → summarize uncached), reusing the
`engine_cache_*` metrics under a bounded `summarizer` label. `stepSummarizer`
wraps the wired summarizer in the decorator only when a cache is configured.

**Cost — overhead ledgered before the call.** New `store.CompactionEntry(i)` =
`compaction:<i>` (the same-attempt PK slot migration 0016 reserved). New
`priceSummaryOverheads` (engine) prices each summarization under
`PolicyEstimate` — a cache-hit summary is a $0 row carrying the counterfactual
`saved`, exactly like a cached provider call. Unlike a judge (post-completion),
a summarizer runs pre-execution, so its overhead row is written **inside the
fenced context-record transaction** — `RecordContextAssembled` grew a `CostRows`
slice, and a new `RecordContextRevisions` records the revisions + overhead for
the **over-budget** path (a summarizer may bill before a later strategy fails).
Both apply the rows via `ApplyAttemptCost` under the claim fence and run lock,
before the step's own provider call, so the spend is metered even if the step
later fails/retries (the 11.5 discipline). This also closes 12.4's documented
"failed-attempt revisions not persisted" gap whenever a summarizer billed. Cost
metrics are recorded post-commit (`recordCostRows`).

**Failure never blocks the step.** A summarizer that is unavailable, errors,
times out, or returns a non-shrinking summary is a *fallback*: the assembly is
left unchanged, the failure is recorded on the strategy's `context_revision`
(the `error` field is the warning event), and the pipeline continues to the next
deterministic strategy — so `[summarize, drop_lowest_priority]` degrades to
`drop_lowest_priority` when the summarizer is down. The one fatal case is a
caller-context cancellation (the step's own timeout/cancel), which the engine
returns bare so the delivery redelivers (ADR-006 rows 3/8, not row 21).

**Store/API.** `ContextRevisionEvent` grew `summaries[]` + `error`;
`ContextAssembledEvent` grew `summaries`; the assembled `sources` gained the
`summarized` disposition and a synthetic `summary`-kind source. No migration
(all events), no new config var, no new metric.

**Wiring & fixture.** `cmd/worker` wires
`engine.WithSummarizer(contextmgr.NewLLMSummarizer(providers))`. Canonical
`examples/definitions/context_summarization.json`: a sixteen-turn conversation
(offline mock, `mock/sim-1`) writing each turn to the blackboard, with four
`rollup` steps that each assemble the four most recent turns (and, from rollup 2
on, the running summary) under a tight budget and summarize with `mock/cheap`,
producing four chained `context_summary` versions.

**Tests.** `internal/contextmgr` summarize unit matrix (shrink + blackboard
value/parent_version, chaining, fallback-with-error, ctx-cancel-fatal, the
`SummaryChatRequest` golden) plus the extended property test (random pipelines
now include summarize; still `FinalTokens ≤ budget` or `*OverBudgetError`, pins
byte-identical). Engine integration (`summarize_integration_test.go`):
`TestContextSummarization` (four chained versions with parent links, every
rollup under budget, `summarized`/`summary` dispositions, four `compaction:*`
`overhead` rows at `mock:cheap`), `TestContextSummarizationCacheHit` (two runs
share one cache; the second run's `compaction:*` rows are `cache_hit` $0 saved
rows — no second billed call), `TestContextSummarizerFallsBack` (unroutable
summarizer model → an unchanged summarize revision with an `error`, the
deterministic fallback runs, the step completes under budget, no summary/overhead
written).

**Non-obvious decisions & limitations.**
- *Message-position ordering.* The synthetic summary entry has no source index,
  so "oldest"/"last-N"/"lowest-priority-tiebreak" switched from source index to
  slice position. Identical for authored-only assemblies (12.4 tests unchanged).
- *One call per summarize application.* The strategy folds the oldest span in a
  single call (greedy to `over + max_tokens`), not one-fold-per-turn — bounded
  provider calls; chaining is primarily *across steps* via the blackboard key,
  not repeated folds within one step.
- *Global summarizer cache scope + fixed namespace.* A span summary is reusable
  across runs (global). The `context_summarizer` namespace is fixed (not the
  resolved provider), so the decorator never resolves the model registry — the
  trade is that a 9.6 bust-by-provider does not sweep summaries (derived,
  short-TTL). `SummarizerVersion` bump invalidates them.
- *Accepted (mirroring the judge).* Summarizer calls bypass the M9 fleet limiter
  and are metered after the call, not pre-projected by the M10 budget check.

One ADR (§"Summarization compaction (as built, 12.5)" + worked example);
ADR-012 (rule 4 — `compaction:<i>` overhead, ledgered pre-call), ADR-011
(§"Summarizer cache (as built, 12.5)"), ADR-006 (row 21 — a summarizer failure
is never a step failure) cross-amended.

**Deferred to 12.6 (the last M12 ticket):** provider-window guardrails —
a `context_window` column in the pricing/model catalog, the default budget
(`window − max_tokens − headroom`) so a bare pipeline fires, the pre-flight hard
check, the `context_utilization` histogram, and swapping M9.2's chars/4
estimator for the real counters.
