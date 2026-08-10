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
