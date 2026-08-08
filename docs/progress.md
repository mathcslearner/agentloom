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
