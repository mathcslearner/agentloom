# Load test plan (M19)

**Status:** reviewed and committed (ticket 19.1). The campaign that executes
this plan is 19.3; the load generator it uses is 19.2. Findings and certified
numbers land in `findings-baseline.md` (19.3) and `BENCHMARKS.md` (19.6).

## 1. Goal

Prove the scale claim — *"scales to thousands of concurrent executions"* — with
real, reproducible numbers on a documented local machine, **before** touching
the cloud (M20 re-runs this matrix on EKS). Concretely, the M19 exit criteria:

- **≥ 1,000 concurrently active runs sustained for 10 minutes** on the pinned
  environment (§4), with p50/p99 scheduling latency and throughput published.
- **Zero lost runs and zero duplicate side effects** at that load (§6).
- **Top bottleneck found, fixed, and the improvement quantified** (19.4/19.5).

If we fall short of 1,000, the shortfall is documented with analysis rather than
hidden — an honest number beats an aspirational one.

## 2. Definitions

The words below are used precisely throughout the campaign so the SLOs are
unambiguous.

| Term | Definition | Source |
|---|---|---|
| **Concurrently active run** | A run that has been submitted and has not reached a terminal status (`succeeded`/`failed`/`cancelled`). | loadgen lifecycle tracking (19.2) + `GET /v1/runs?status=` |
| **Scheduling latency** | `ready → running` wall time for a step: how long a ready step waits before a worker claims it. **This is the primary saturation signal** — a starved queue shows here first. | `engine_step_scheduling_latency_seconds` histogram (7.2), measured at the claim CAS |
| **End-to-end run latency** | submit → terminal, per run. | loadgen HDR histogram |
| **API submit latency** | `POST /v1/runs` server-side latency. | `engine_api_request_duration_seconds` + loadgen client HDR |
| **Throughput** | terminal runs/second in steady state. | loadgen + `rate(engine_run_duration_seconds_count[...])` |
| **Steady state** | the measurement window after warmup, before cooldown (§5). | — |

The scheduling-latency SLO is what keeps "active" honest: a run that is
submitted but whose steps sit un-claimed still counts as *active*, so without a
latency ceiling the active-run count could be inflated by a starved fleet. The
p99 target is the guard against that.

## 3. SLO targets (proposed; ratified by the baseline in 19.3)

| Metric | Target | Notes |
|---|---|---|
| Concurrently active runs sustained | ≥ 1,000 for 10 min | the headline |
| Scheduling latency p50 | ≤ 250 ms | steady state |
| Scheduling latency p99 | ≤ 2 s | steady state; the knee is where this breaks |
| API submit p99 | ≤ 100 ms | the submission path must not be the binder |
| Lost runs | 0 | reconciler heals every crash gap |
| Duplicate side effects | 0 | idempotency keys + journal (§6) |
| Non-deliberate dead letters | 0 | only the deliberate-failure probe may DLQ |
| End-to-end p99 | ≤ 30 s | scenario-dependent; a soft target |

These are dev-scale targets for the pinned machine below, not production SLAs.
The baseline campaign (19.3) may revise them with evidence — a target the
hardware cannot meet is recorded as such, not quietly relaxed.

## 4. Pinned environment

The knee point must be a property of a documented machine, not the host's spare
capacity, so the load stack is a **resource-pinned compose overlay**:
`docker-compose.load.yml` layered over `docker-compose.yml`. One command boots
it:

```bash
make load-up          # AGENTLOOM_LOAD_WORKERS=8 by default
```

`make load-down` stops it (keeping the dedicated volumes); `make load-nuke`
drops the volumes for a pristine next run. `make load-status` shows replica
count and health.

### Resource pins

| Service | CPU limit | Memory limit | Notes |
|---|---|---|---|
| postgres | 2.0 | 2 GiB | `pg_stat_statements` on; `shared_buffers=512MB`, `work_mem=16MB`, `max_connections=200`, `max_wal_size=4GB` |
| redis | 1.0 | 768 MiB | AOF on (production-representative) |
| api | 1.0 | 768 MiB | response cache **off**; API rate limits raised far above arrival; pprof on |
| worker × K | 0.5 | 384 MiB each | K = `AGENTLOOM_LOAD_WORKERS` (default 8); cache off; load mock script; pprof on |

**Reference host** (record the actual host in each campaign): Apple M2, 8
cores, 16 GiB; Docker Desktop VM 8 vCPU / ~7.6 GiB. At K=8 the worker fleet
alone requests 4 CPU, so the fleet + Postgres + Redis fit the VM with headroom;
push K higher only after raising the Docker VM's CPU/memory allocation, and
record the change. **The default Docker Desktop memory (often 8 GiB) is tight —
raise it to ≥ 12 GiB before a full-matrix run** (Docker Desktop → Settings →
Resources).

### Deliberate environment choices (and why)

- **Response cache disabled** (`AGENTLOOM_CACHE_ENABLED=false` on api + worker).
  The M9 cache is global-scoped and keyed on the request; an identical-prompt
  load run would be served for $0 with no provider call, so every repeated step
  would short-circuit and the numbers would measure the cache, not the engine.
- **API rate limits raised**, not disabled. Defaults are 20 cap / 10 rps on the
  submit bucket — a loadgen would 429 on the first burst. The overlay raises
  them ~200× so they never bind, while the 429 code path stays wired and
  representative.
- **OTel export off by default** (`AGENTLOOM_LOAD_OTEL=false`). Span export and
  sampling add per-request overhead that would confound the CPU profiles; turn
  it on (`AGENTLOOM_LOAD_OTEL=true make load-up`) only for a targeted trace of a
  specific slow path.
- **Step-log capture left at its default (`info`).** Step logs are themselves an
  input to the write-amplification hypothesis (H2) — measuring the real system
  means measuring them. If H2 implicates steplog flushing, 19.4 tunes it and
  re-measures; it is a knob, not a pre-emptive exclusion.
- **Dedicated load volumes** (`postgres-load-data`, `redis-load-data`) so a
  campaign starts pristine and never perturbs dev/CI data.
- **Mock provider, scripted** (`test/load/mock.json`): lognormal latency
  (p50 120 ms / p99 900 ms) and uniform token draws (input 400–1200, output
  80–400), plus the always-revise critic rule the agent-loop scenario needs.
  The mock is the load workhorse — cheap, deterministic, offline, no API key.

### pprof capture (19.3)

pprof is mounted on the in-network admin port (`:9090`), never published to the
host, so capture from inside the compose network:

```bash
# 30s CPU profile of one worker replica
docker compose -f docker-compose.yml -f docker-compose.load.yml exec worker \
  wget -qO- 'http://localhost:9090/debug/pprof/profile?seconds=30' > worker.cpu.pprof
# heap
docker compose ... exec worker wget -qO- 'http://localhost:9090/debug/pprof/heap' > worker.heap.pprof
# API CPU
docker compose ... exec api wget -qO- 'http://localhost:9090/debug/pprof/profile?seconds=30' > api.cpu.pprof
```

## 5. Scenarios

Each scenario is a named JSON config under `test/load/scenarios/`, parsed by
the `internal/loadtest` package (so the corpus is CI-validated before the
generator exists) and consumed by `cmd/loadgen` (19.2). Their workflow
definitions live in `test/load/definitions/` and run fully offline on the mock.

| Scenario | Definition | Shape | Steps/run | Stresses |
|---|---|---|---|---|
| `linear-10` | `linear_10.json` | 10 chained `llm` steps | 10 | serial per-worker throughput (H1) — the cleanest H1 probe |
| `fanout-50` | `fanout_50.json` | seed → 50 parallel `llm` → `join(all)` → final | 53 | write amplification (H2), join-counter hot row (H4), instantiation cost (H6) |
| `planner-heavy` | `planner_heavy.json` | 2 sequential planner expansions, each 8 workers → join | 5 authored + 16 injected | completion-tx expansion path (H2), expansion write amp |
| `agent-loop` | `agent_loop.json` | writer⇄critic, always-revise, 3 loop-backs then exit | 3 authored + 6 injected | loop unrolling, thread growth, expansion under load |
| `mixed` | *(composite)* | weighted blend 0.4/0.2/0.2/0.2 | — | the representative steady-state mix for the 1,000-run target |

Determinism note: `agent-loop`'s critic always returns `{"verdict":"revise"}`
(the `load-test critic` rule in the mock script), so every run does exactly
`max_iterations` (3) loop-backs and exits via `on_exhausted: proceed` —
constant work per run regardless of the mock's per-process call cursor.

## 6. Integrity assertions at load

"Zero lost runs / duplicate side effects" is verified, not assumed. These
checks are pre-specified here so 19.6 measures against a fixed contract, not an
invented one. They are asserted on a drained, quiesced stack after the steady
window:

1. **No lost runs.** Every submitted run reaches a terminal status; the loadgen
   reconciles its submitted set against `GET /v1/runs`.
2. **Exactly-once step execution.** Every `succeeded` step has exactly one
   `succeeded` attempt; per-run `events.seq` is gap-free; run counters equal the
   step-status tally.
3. **No duplicate side effects.** A dedicated integrity workload (added in 19.6)
   uses the journaled `effectful_echo` executor over a shared worker volume: its
   ledger holds exactly one line per run regardless of kills/retries (the
   idempotency-key guarantee), with the unjournaled `counter` executor as the
   at-least-once contrast. *(Deferred from 19.1: the four baseline scenarios are
   pure mock-`llm` work to keep them offline and volume-free; the integrity
   probe needs a shared writable volume + per-run unique paths, wired in 19.6.)*
4. **Queue quiescence.** After drain: stream group lag 0, PEL empty, delayed
   ZSET empty, `task_outbox` empty (the chaos-suite `WaitQuiescent` invariant).
5. **Only deliberate dead letters.** No `dead_letters` rows except those the
   integrity workload's deliberate-failure step is expected to produce.

## 7. Measurement methodology

- **Windows.** 60 s warmup (discarded) → 10 min steady-state measurement →
  cooldown until quiescent. Percentiles come only from the steady window.
- **Arrival control — open loop.** The generator submits at the configured rate
  regardless of completion speed, so a saturated system manifests as growing
  latency and active-run backlog, never as a throttled submitter (the
  coordinated-omission guard). `constant` for the sustained target; `ramp` to
  find the knee.
- **Knee point.** Ramp the arrival rate until the first SLO breach (scheduling
  p99 > 2 s is the usual first signal) or an unambiguous resource saturation
  (§ saturation evidence). Record the rate, the active-run count, and which
  resource saturated.
- **Percentile sourcing.** Scheduling / step-duration / API latency + queue
  depths from Prometheus histograms (7.2); submit + end-to-end latency from the
  loadgen's own HDR histograms (client-side, so they include the full round
  trip). Two independent sources cross-check each other.
- **Saturation evidence — what "the binder" means per resource:**
  - *Worker CPU:* pprof CPU profile (which functions), `docker stats` sampling
    (which container). H1's signature: queue ready-depth grows while worker CPU
    is *idle* → the binder is serialization, not compute.
  - *Postgres:* `pg_stat_statements` (total_time / calls / rows per statement —
    ranks the write-amplification suspects), `pg_stat_activity` (lock waits,
    pool saturation), WAL rate.
  - *Redis:* `INFO` (used_cpu, ops/sec, blocked_clients), `LATENCY` (command
    latency spikes).
  - *Queue:* the 7.2 depth/lag/dispatch-latency histograms and the Engine
    dashboard's queue row.
- **Reproducibility.** Fresh volumes per campaign (`make load-nuke && make
  load-up`), pinned image tags, recorded git SHA, a dumped effective env
  (`docker compose ... config`), and the exact loadgen invocation in the
  findings doc. Every published number must be reproducible from the documented
  commands (a 19.6 acceptance box).

### `pg_stat_statements` capture recipe

```bash
# reset at the start of the steady window
docker compose ... exec postgres psql -U agentloom -d agentloom \
  -c "SELECT pg_stat_statements_reset();"
# snapshot at the end, ranked by total time
docker compose ... exec postgres psql -U agentloom -d agentloom -c \
  "SELECT calls, total_exec_time, mean_exec_time, rows, query
     FROM pg_stat_statements ORDER BY total_exec_time DESC LIMIT 25;"
```

## 8. Pre-registered hypotheses

The bottleneck investigation is honest only if the suspects are named before
the data comes in. Ranked by prior likelihood, each with the metric that would
confirm it and the pre-authorized remediation (19.4 picks from these per
findings — the fix is scoped strictly to the identified binder).

| # | Hypothesis | Confirming signal | Candidate remediation |
|---|---|---|---|
| **H1** | **Serial per-worker execution.** One in-flight step per worker process (`internal/queue` consumer is a serial `XREADGROUP` loop; `cmd/worker` runs one consumer). Fleet step-concurrency = replica count, so steps/s ≈ K ÷ mean step latency. | Queue ready-depth grows while worker CPU is idle; scheduling p99 climbs; throughput flat vs offered load but scales linearly with K. | In-worker concurrency (a bounded pool of claim loops per process) **or** simply scale K — the honest first question is which the architecture needs. Documented in ADR-002/005. |
| **H2** | **Postgres write amplification per completion.** Each step completion writes events + attempt + outbox + step-row updates (+ expansion rows for planner/loop); steplog flushes add more. | `pg_stat_statements` dominated by INSERT/UPDATE on `events`/`step_attempts`/`task_outbox`; high WAL rate; Postgres CPU-bound. | Batch event/attempt inserts; collapse per-completion round-trips; pgx batching; steplog flush tuning. |
| **H3** | **Outbox drain contention.** All workers drain one `task_outbox` via `FOR UPDATE SKIP LOCKED`. | Dispatch-lag histogram climbs; lock-wait time on `task_outbox` in `pg_stat_activity`. | LISTEN/NOTIFY-driven drain + adaptive batching; index/keyset correction. |
| **H4** | **Join-counter hot row.** `fanout-50`'s 50 completions decrement one `run_steps` join row; row-lock serialization. | Row-lock waits concentrated on the join step's row; `fanout-50` knee far below `linear-10`. | Counter-contention fix (sharded decrement / conditional readiness recompute). |
| **H5** | **Single-stream serialization / Redis CPU.** One `steps:ready` stream; `XREADGROUP`/`XAUTOCLAIM` on one key. | Redis CPU-bound; command latency spikes; throughput flat as K rises. | **Stream sharding** — the ADR-005 lever: shard into K streams by `hash(run_id)`, workers consume all shards (implemented in 19.5 if the queue binds). |
| **H6** | **Submission-path cost.** `fanout-50` instantiates 53 rows in one tx; API-side. | API submit p99 climbs with fan-out width; API CPU-bound. | Batch the instantiation insert; pool tuning. |
| **H7** | **pgx pool exhaustion.** Default pool size caps concurrent DB work fleet-wide. | `pg_stat_activity` connection waits; throughput plateaus with idle CPU everywhere. | Pool-size / `MaxConns` tuning (per-deployable). |

The *likely* first binder is **H1** — with a ~120 ms p50 mock latency and 8
workers, naive steps/s ≈ 8 ÷ 0.12 ≈ 67, so sustaining 1,000 active runs of
10-step chains needs either in-worker concurrency or a larger K. The baseline
campaign (19.3) confirms or refutes this with the idle-CPU-vs-growing-queue
signature and selects the real top target.

## 9. Out of scope for local M19

- Real provider calls (the mock is the workhorse; real-provider cost/latency is
  a different measurement).
- Cloud / multi-node scaling (M20 re-runs this matrix on EKS).
- Cross-run fairness / per-run throttle caps (ADR-010's documented-but-deferred
  fairness lever) — implemented only if a load run shows one fan-out starving
  the fleet.

## 10. Artifacts this plan produces

- `docker-compose.load.yml` + `deploy/load/postgres-init.sql` — the pinned env.
- `scripts/load-env.sh` + `make load-up|status|down|nuke` — one-command boot.
- `test/load/definitions/*.json` — the four offline workflow fixtures.
- `test/load/scenarios/*.json` — the five named scenario configs.
- `test/load/mock.json` — the fleet mock script.
- `internal/loadtest` — the scenario contract + parser (CI-validated corpus).
- `cmd/loadgen` + [`docs/load/loadgen.md`](loadgen.md) — the load generator
  (delivered 19.2). (19.3) `docs/load/findings-baseline.md`;
  (19.6) `BENCHMARKS.md`.
