# ADR-009: Plugin SPI — kinds, registration, config schemas, capability flags

- **Status:** Accepted
- **Date:** 2026-08-14
- **Ticket:** ROADMAP.md ticket 8.1

## Context

M8 is where the engine meets AI: LLM steps, tool calls, retrieval, and —
in later milestones — validators all need concrete implementations behind
the engine's execution pipeline. Ticket 4.1 shipped a deliberately minimal
executor SPI (`internal/exec`): an `Executor` interface and an
instance-based registry keyed by step type, with a note that the full
plugin SPI would arrive in M8 and grow it rather than replace it.

Three consumers now force the SPI to become explicit architecture rather
than an internal convenience:

1. **The engine** looks plugins up by identity and, starting with M9's
   middleware chain, must make behavioral decisions it cannot infer from
   the implementation — may this output be cached? does this call spend
   money? does it fire external side effects that must be journaled?
2. **The API** must serve a machine-usable plugin listing
   (`GET /v1/plugins`) so operators and tooling can see what a deployment
   can execute, with each plugin's config schema in a form UI forms can
   consume (M17.4 builds the builder's config panels directly from these
   schemas — one contract, three consumers).
3. **Future plugin kinds** — tools (8.7), retrievers (8.8), model
   providers (8.3/8.4), validators (M11) — need one registration
   discipline instead of four ad-hoc registries invented independently.

The forces in tension: the SPI must be general enough that every later AI
feature registers through it, but M8 must not build speculative machinery
(out-of-process isolation, dynamic discovery, hot reload) that nothing on
the roadmap needs. And the ~15 existing call sites constructing
`exec.Registry` instances in tests must not be churned by the refactor.

## Decision

### Plugin kinds

We define a closed vocabulary of five plugin kinds:

| Kind | Interface (owning package) | Arrives |
|---|---|---|
| `executor` | `exec.Executor` (`internal/exec`) | M4 (refactored here) |
| `tool` | tool SPI (`internal/tools`) | 8.7 |
| `retriever` | retriever SPI (`internal/…`, 8.8) | 8.8 |
| `model_provider` | provider interface (`internal/llm`) | 8.3 (interface), 8.4 (registry + routing) |
| `validator` | validator SPI (`internal/validate`) | M11 |

A plugin's identity is the pair **(kind, name)**. Names are
`^[a-z][a-z0-9_]*$`, at most 64 characters — the same spelling as step
types, which is deliberate: an executor plugin's name **is** its step
type. Names are unique per kind, not globally (a `tool` named `retrieve`
would be confusing but is not a registry error).

### In-process compilation model

Plugins are ordinary Go packages compiled into the deployable binaries.
Registration happens at process boot into instance-based registries
(never package globals — the 4.1 decision stands); registries are
read-only after boot, so lookups take no locks. An invalid or duplicate
registration **fails startup** — a mis-wired plugin is a build/deploy
bug, and shadowing or skipping it silently would surface as baffling
runtime behavior instead.

Out-of-process plugins (subprocess, gRPC) and WASM sandboxing are
**explicitly deferred to the backlog**. Nothing on the v1 roadmap needs
untrusted third-party plugins; the SPI's shape (manifest + interface
value) does not preclude adding an out-of-process adapter later.

Consequence of in-process compilation: **the plugin set is a build-time
property**. Both deployables can therefore construct the same catalog
from the shared packages, which is how the API serves `GET /v1/plugins`
without any registration handshake (below).

### The manifest and the generic registry

`internal/plugin` is a new leaf package (imported by kind-owning
packages, importing none of them — no cycles) that owns the SPI's shared
vocabulary:

- `plugin.Kind` — the five-value enum above.
- `plugin.Capabilities` — the three capability flags (below).
- `plugin.Manifest` — the self-description every plugin carries: kind,
  name, semver version string, optional human description, capability
  flags, and an optional JSON Schema for its config
  (`json.RawMessage`; nil means "takes no config"). `Validate()`
  enforces the naming rule, the kind vocabulary, the version format, and
  that a present schema is a JSON object.
- `plugin.Registry` — the generic registry: `Register(Manifest, impl
  any)` (rejecting invalid manifests, nil impls, and duplicate
  (kind, name) with typed errors), `Lookup(kind, name)`, and sorted
  `Manifests()` / `ManifestsByKind(kind)` listings.

Kind-owning packages wrap it in **typed facades**: `exec.Registry` keeps
its 4.1 surface (`NewRegistry(execs ...Executor)`, `Get(stepType)
(Executor, error)` with the established `*UnknownTypeError`) but is now
backed by a `plugin.Registry` and additionally exposes `Manifests()`.
The facade pattern repeats for providers/tools/retrievers/validators as
they land: type-safe lookup at the call site, one shared registration
discipline and listing shape underneath.

### Self-description, and the test-double escape hatch

Production plugins describe themselves: an executor implements

```go
PluginManifest() plugin.Manifest
```

(the optional `exec.SelfDescribing` interface) and the registry validates
that the manifest's kind is `executor` and its name equals `Type()` — a
plugin may not claim an identity other than the one it executes under.

Bare `Executor`s without `PluginManifest` remain registrable. The many
test doubles across the engine, queue, and crash suites construct
registries with minimal stub executors, and forcing a manifest onto every
one of them buys nothing. For a non-describing executor the registry
**synthesizes a conservative manifest**: version `0.0.0`, and the flags
set to the assumptions that are safe when nothing is known —
`side_effectful: true` (never skip journaling for an unknown plugin),
`cacheable: false` (never serve a cached result for an unknown plugin),
`cost_bearing: false` (nothing to meter). A conformance test pins that
every executor in the shipped builtin sets self-describes, so synthesized
manifests can exist only in test code.

### Capability flags

Three boolean flags, chosen because they are exactly what M9 (cache,
rate-limit middleware) and M10 (cost ledger) must know without executing
the plugin:

- **`side_effectful`** — the plugin performs externally observable side
  effects. Its executions must go through the side-effect journal /
  idempotency keys (5.5), and its outputs must never be served from
  cache (replaying the output would skip the effect).
- **`cacheable`** — the plugin's output is a function of (config, input)
  *and* caching is semantically sound. This is deliberately stronger
  than "deterministic": `sleep` is pure but not cacheable (a cache hit
  would skip the wait, which is the step's whole meaning), and
  `fail_n_times` branches on the attempt number. LLM calls are
  non-deterministic yet *are* cacheable — M9's response cache exists
  precisely to reuse expensive non-deterministic outputs under a
  deliberate key design.
- **`cost_bearing`** — executing the plugin spends money (provider
  tokens, paid APIs). M10's ledger and budget enforcement meter these;
  M9's cache-hit accounting records counterfactual spend for them.

Flags describe the plugin's **contract, not today's implementation
shortcut**: the three dev stubs (`llm`, `tool`, `retrieve`) carry the
flags of the real semantics that will replace them in place (8.3–8.8),
so middleware written against the flags behaves correctly across that
swap without a flag migration.

The builtin flag table (the conformance baseline):

| Plugin (executor) | Version | side_effectful | cacheable | cost_bearing |
|---|---|---|---|---|
| `noop` | 1.0.0 | – | ✓ | – |
| `echo` | 1.0.0 | – | ✓ | – |
| `sleep` | 1.0.0 | – | – | – |
| `fail_n_times` | 1.0.0 | – | – | – |
| `join` | 1.0.0 | – | – | – |
| `branch` | 1.0.0 | – | – | – |
| `counter` | 1.0.0 | ✓ | – | – |
| `effectful_echo` | 1.0.0 | ✓ | – | – |
| `llm` (stub) | 0.1.0-stub | – | ✓ | ✓ |
| `tool` (stub) | 0.1.0-stub | ✓ | – | – |
| `retrieve` (stub) | 0.1.0-stub | – | ✓ | – |

(`join`/`branch` are pure but not cacheable: they are control flow whose
meaning is readiness and edge firing — caching them is noise. `tool` is
side-effectful because an unknown tool must be assumed to act on the
world; specific built-in tools may declare otherwise in 8.7.)

### Plugin version strings

Versions are semantic versions (`MAJOR.MINOR.PATCH` with optional
pre-release/build suffix, validated at registration). They exist for one
concrete consumer — **M9's cache keys**: a cached output is keyed on
(plugin name, plugin version, config, input) among other components, so
the bump rule is behavioral, not cosmetic: *bump the version whenever a
change should invalidate previously cached outputs*. Real builtins start
at `1.0.0`; the dev stubs carry `0.1.0-stub` so a stubbed plugin is
unmistakable in any listing, and their replacement by real executors is
itself the kind of behavior change that mandates the bump to `1.0.0`.

### Config schemas

Per-plugin config schemas are **generated from the same Go structs the
decoder uses** (the project invariant since ADR-003) — never
hand-written. For executors, the config structs already live in the dag
package's step-config catalog; a new `dag.StepConfigSchema(StepType)`
reflects one step type's config struct into a standalone JSON Schema
document (invopop/jsonschema, same reflector settings as the definition
schema: strict `additionalProperties: false` matching
`DisallowUnknownFields`), and `exec.Registry` attaches it automatically
at registration, since executor name = step type. Non-executor kinds
supply schemas in their manifests using the same generator against
their own config structs.

The API serves each schema verbatim (`config_schema` in
`GET /v1/plugins`) as an embedded JSON Schema 2020-12 document — the
machine-usable form M17.4's UI forms will render directly.

### API exposure and the single-build assumption

`GET /v1/plugins` (read scope, read rate-limit class) lists the catalog
**compiled into the API binary**, sorted by (kind, name). There is no
worker→Postgres registration handshake: under the in-process model the
plugin set is a build-time property, and API and workers ship from the
same build (the compose stack and every planned deployment build both
images from one source tree).

One configuration wrinkle: the worker's registry is gated by
`AGENTLOOM_WORKER_TEST_EXECUTORS` (6.2 — the filesystem-writing test
executors are opt-in). The API mirrors this with
`AGENTLOOM_API_TEST_EXECUTORS` (default false), so a deployment sets
both knobs alike and the listing matches the fleet. We accept that a
heterogeneous fleet (mixed builds or mismatched knobs) could make the
listing inaccurate; dynamic fleet introspection is deferred until
something real needs it, and the ADR records the assumption so that
change has a trigger.

`ctl plugins list` renders the listing as a table (kind, name, version,
capability flags).

### Model-provider registry & routing (as built, 8.4)

The `model_provider` kind gets the same typed-facade treatment the
executor kind got in 8.1: `llm.Registry` wraps `plugin.Registry`
(kind `model_provider`, name = provider name), with `Register`
identity-checking the manifest's kind, `Get(name)`, `Manifests()`, and
`Names()`. Registration failures (nil provider, wrong kind, duplicate
name) fail boot, exactly like executors.

Providers are the one kind whose **listing is configuration-gated, not
just build-gated**. An executor is compiled in unconditionally; a
provider can only be constructed with an API key, and a worker running
no llm steps boots keyless (8.3). So `llm.NewRegistryFromKeys` — the one
constructor both deployables call — builds Anthropic iff its key is
present, OpenAI iff its key is present, and an empty registry is valid.
This directly satisfies 8.4's "either provider absent without breaking
startup": the *catalog* a deployment lists matches the *keys* it holds.
`internal/llm` stays a leaf (imports `dag`, `plugin`, stdlib): the
constructor takes a plain `ProviderKeys` struct, so `internal/config`
never enters llm's import graph.

**Routing.** `Registry.Resolve(explicitProvider, model)` is the entry
point the 8.6 llm executor calls; it returns the provider and the
canonical model the provider should receive, applying three rules in
priority order:

1. an explicit `provider` step-config field wins (unknown name →
   `*ProviderUnavailableError`);
2. a `"<provider>/<model>"` namespace prefix routes by the named
   provider and strips the prefix (this is the form 8.5's mock provider
   uses, `mock/...`, so it is reserved now);
3. a bare model matches the longest vendor prefix in a small static
   table (`claude`→anthropic; `gpt-`/`chatgpt-`/`o1`/`o3`/`o4`→openai).

Two typed errors are deliberately distinct: `*UnknownModelError` (no
rule matched — the ticket's required typed error) versus
`*ProviderUnavailableError` (a rule resolved to a provider that isn't
configured, i.e. its key is absent). The split makes a misconfiguration
diagnosable — "no provider knows this model" needs a different fix than
"you didn't configure this provider" — and both are deterministic, so
8.6 maps them to permanent failures.

## Consequences

Easier:

- Every later AI feature (8.3–8.8, M11) registers through one
  discipline: implement the kind's interface, return a manifest, add to
  the deployables' catalog construction. Registration bugs fail boot.
- M9/M10 middleware reads capability flags and versions off the manifest
  instead of maintaining per-type knowledge, and behaves correctly
  across the stub→real executor swaps without changes.
- The UI's config panels (M17.4) consume the same generated schemas the
  API serves — no second source of truth for config shapes.
- The exec facade keeps its 4.1 surface, so the refactor touches no
  engine code and no existing test.

Harder / accepted costs:

- `GET /v1/plugins` reflects the API binary's catalog, not measured
  fleet capability; a mixed fleet can make it lie (accepted,
  single-build assumption documented above).
- The synthesized-manifest escape hatch means a production executor
  *could* ship without self-describing if the conformance test were
  deleted; the test is the guard.
- Capability flags are declared, not verified — a plugin can lie about
  `side_effectful`. In-process plugins are trusted code; verification
  only becomes meaningful with the deferred isolation work.
- One more package (`internal/plugin`) beyond the planned layout;
  recorded here as a layout addendum.

## Alternatives considered

- **Grow the whole SPI inside `internal/exec`** (the 4.1 comment's
  literal reading). Rejected: model providers and validators are not
  executors, and forcing `exec.Manifest` onto them misnames the concept;
  a leaf `internal/plugin` package gives the shared vocabulary a home
  with zero import-cycle risk.
- **Require every executor (including test doubles) to self-describe.**
  Rejected: it churns ~15 test call sites for no information gain; the
  synthesized conservative manifest plus the builtin conformance test
  achieves the same production guarantee.
- **Worker-registered plugin rows in Postgres, API serves the table.**
  Rejected for 8.1: it adds a migration, liveness/GC semantics for
  departed workers, and a merge policy for heterogeneous fleets — all to
  answer a question the in-process model answers at compile time. The
  deferred trigger is real fleet heterogeneity.
- **Capability flags as a free-form string set.** Rejected: the three
  consumers (cache, journal, ledger) are known, and a closed struct
  keeps them type-checked; a fourth flag is one field away.
- **Full semver parsing/ordering (Masterminds/semver dependency).**
  Rejected: nothing orders versions yet — M9 only needs equality in a
  cache key. A format regex suffices; a library can arrive when ordering
  does.
- **Serving schemas by reference (`/v1/plugins/{name}/schema`).**
  Rejected: the whole catalog with embedded schemas is a few tens of KB,
  served rarely, and one round trip is simpler for the UI.
