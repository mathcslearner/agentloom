# Baseline campaign findings (M19.3)

**Status:** complete. This document records the baseline load campaign that
executes [`plan.md`](plan.md) — the knee point per scenario with saturation
evidence, the top bottleneck named from profile/query evidence, and the baseline
numbers that anchor the before/after in 19.4 (remediation #1) and 19.5.

Every empirical number here is reproducible from `scripts/load-campaign.sh`
(`make load-campaign`), which captures the full evidence bundle (loadgen report,
pprof, `pg_stat_statements`, `pg_stat_activity`, Redis `INFO`/`LATENCY`, docker
stats, Prometheus range series) into `results/<scenario>-<utc>/`.

> **Reproduce.** `make load-nuke && make load-up`, then per scenario:
> ```bash
> scripts/load-campaign.sh linear-10     --ramp 1:8:1:30s      --warmup 30s --duration 4m --run-timeout 15m
> scripts/load-campaign.sh fanout-50     --ramp 0.2:2.4:0.2:30s --warmup 20s --duration 3m --run-timeout 15m
> scripts/load-campaign.sh planner-heavy --ramp 0.5:4:0.5:25s   --warmup 20s --duration 3m --run-timeout 15m
> scripts/load-campaign.sh agent-loop    --ramp 1:8:1:25s       --warmup 20s --duration 2m30s --run-timeout 15m
> scripts/load-campaign.sh mixed         --ramp 0.5:4:0.5:25s   --warmup 20s --duration 3m --run-timeout 15m
> # K-scaling probe (the H1 discriminator): rerun linear-10 with the fleet resized
> AGENTLOOM_LOAD_WORKERS=4  make load-up && scripts/load-campaign.sh linear-10 --ramp 1:6:1:25s ...
> AGENTLOOM_LOAD_WORKERS=12 make load-up && scripts/load-campaign.sh linear-10 --ramp 1:9:1:25s ...
> ```

## 1. Environment

| field | value |
|---|---|
| host | Apple Silicon, 8 logical CPUs, 16 GiB (`darwin/arm64`) |
| Docker VM | 8 vCPU, ~7.6 GiB |
| worker replicas (K) | 8 (baseline); K-scaling probe at 4 / 8 / 12 |
| Postgres pin | 2 CPU / 2 GiB, `pg_stat_statements` on |
| Redis pin | 1 CPU / 768 MiB, AOF on |
| api pin | 1 CPU / 768 MiB, cache off, pprof on |
| worker pin | 0.5 CPU / 384 MiB each, cache off, load mock, pprof on |
| campaign | ramp-to-knee per scenario (open loop); mock provider, offline |

**Deviations from plan §4.** (1) The Docker VM has ~7.6 GiB, below the plan's
recommended ≥ 12 GiB. Worker limits are ceilings, not reservations, and real
worker RSS at load is well under the 384 MiB cap (the fleet is >97 % idle — §4),
so the campaign ran cleanly at K=8; record a higher-memory VM before a
1,000-active-run sustained run (19.6). (2) The K=12 probe requests 6 vCPU of
worker limits on an 8-vCPU VM; since the workers are near-idle this does not
contend, but it is a documented over-subscription for that probe only.

## 2. Method

Open-loop ramp per scenario until the first SLO breach (scheduling p99 > 2 s, or
end-to-end p99 diverging). The knee is read two ways that agree: the loadgen
report's **ramp-step table** (`summary.md` §"Ramp steps" — e2e p99 diverging per
offered rate) and the Prometheus `engine_step_scheduling_latency_seconds` p99
series over the campaign window (`prom/sched_p99.json`). Worker/Postgres/Redis
saturation is read from pprof, `pg_stat_statements`, `pg_stat_activity`, and
`docker stats`.

## 3. Results per scenario (K=8)

The **fleet step throughput is ~45–50 steps/s regardless of scenario shape** —
the single most important number. The per-scenario knee in *runs*/s is that
ceiling divided by the steps per run.

| scenario | steps/run | knee (runs/s) | fleet steps/s @ knee | e2e p99 across the knee | saturated resource | lost | non-delib DLQ | quiescent |
|---|---|---|---|---|---|---|---|---|
| linear-10 | 10 | ~4.5 | ~45 | 5.4 s → 18.3 s (rate 4→5) | worker (serial) | 0 | 0 | ✓ |
| fanout-50 | 53 | ~0.9 | ~48 | 12.7 s → 27.9 s (rate 0.8→1.0) | worker (serial) | 0 | 0 | ✓ |
| planner-heavy | 21 | ~2.2 | ~46 | 5.7 s → 14.0 s (rate 2.0→2.5) | worker (serial) | 0 | 0 | ✓ |
| agent-loop | 9 | ~6.0 | ~54 | 5.0 s → 13.3 s (rate 6→7) | worker (serial) | 0 | 0 | ✓ |
| mixed | ~21 | ~2.2 | ~45 | 9.3 s → 22.4 s (rate 2.0→2.5) | worker (serial) | 0 | 0 | ✓ |

**Scheduling latency** stays tiny (p50 ~7–10 ms) until the fleet saturates, then
the p99 tail climbs sharply (linear-10: 76 ms → 2.3 s as the offered rate crosses
~4.5/s) while queue ready-depth grows — the classic "queue backs up, latency
tail explodes" saturation shape. Integrity held at every load: **0 lost runs, 0
non-deliberate dead letters, and full queue quiescence** (stream lag 0, PEL 0,
delayed 0, outbox 0) after each drain.

## 4. Evidence

### 4.1 K-scaling probe — the H1 discriminator

Same `linear-10` ramp at K = 4 / 8 / 12. **Throughput scales ~linearly with K
while worker CPU stays near-idle** — the textbook H1 signature (plan §8):
serialization, not compute.

| K | fleet steps/s (saturated) | worker CPU (sum of cores) | worker CPU per replica | ready-depth @ saturation |
|---|---|---|---|---|
| 4 | ~23 | 0.15 | ~3.7 % of 0.5 | 485 |
| 8 | ~45 | ~0.19 | ~2.3 % of 0.5 | 500+ |
| 12 | ≥ 60 (did not fully saturate in the short probe) | ~0.20 | ~1.7 % of 0.5 | grows past rate 6/s |

steps/s ≈ **5.5 × K**. Doubling the fleet doubles throughput; the workers are
never CPU-bound.

### 4.2 Worker pprof at the knee — the smoking gun

A 25 s CPU profile of one `linear-10` worker replica *at peak saturation* (queue
500+ deep, e2e p99 > 100 s):

```
Duration: 25s, Total samples = 580ms ( 2.32%)
   150ms 25.86%  internal/runtime/syscall/linux.Syscall6   (blocking waits: mock latency + Redis I/O)
    20ms  3.45%  runtime.(*timers).run
    ...          (no engine hot function dominates)
```

**The worker consumed 580 ms of CPU over 25 s = 2.3 % utilisation while the queue
was deeply backed up.** The other scenarios' saturation profiles are the same:
fanout-50 worker CPU 6.5 % of the 0.5-core cap, planner-heavy 2.35 %. Compute is
not the binder — the worker spends its time blocked on the (simulated) provider
call inside a *serial* claim loop.

### 4.3 Root cause in the code

`internal/queue/consumer.go` reads a batch with `XREADGROUP` and then processes
each message **serially** in a blocking `for` loop (`c.deliver(ctx, msg, 1)`),
and `cmd/worker/main.go` constructs exactly **one** `Consumer` per process. So a
worker process executes **one step at a time**: fleet step-concurrency = replica
count. With a mock mean step latency of ~175 ms + ~20 ms engine overhead, one
worker sustains ~5–6 steps/s, and 8 workers ~45 steps/s — exactly the measured
ceiling.

### 4.4 Postgres is not the binder at K=8 (H2/H3/H4/H7 not reached)

`pg_stat_statements` over the full ~9-min `linear-10` campaign — top statements
by total execution time:

| total time | calls | mean | statement |
|---|---|---|---|
| 5.45 s | 55 440 | 0.10 ms | `AllocateEventSeq` (UPDATE runs SET next_seq) |
| 5.29 s | 101 356 | 0.05 ms | `GetRun` (heavy — partly loadgen polling) |
| 3.76 s | 13 200 | 0.28 ms | `step_logs` COPY (steplog flush) |
| 2.90 s | 55 440 | 0.05 ms | `AppendEvent` INSERT |
| 2.21 s | 100 320 | 0.02 ms | `LockRun` |
| 1.80 s | 13 200 | 0.14 ms | `SucceedRunStep` |
| 1.50 s | 13 200 | 0.11 ms | `ClaimRunStep` |

The **entire top-25 sums to ~43 s of execution over 540 s of wall clock** on a
2-CPU Postgres — ~8 % of one core. Every statement is sub-millisecond.
`pg_stat_activity` during `fanout-50` saturation (the 50-way join — the H4
suspect) showed **zero lock waits** (34 idle connections, 1 active query, no
`Lock`/`LWLock` wait events). So at K=8 the join-counter hot row (H4), outbox
drain contention (H3), and pool exhaustion (H7) are **not reached** — Postgres
has ample headroom behind the serial-worker ceiling.

The write-amplification ranking above is still the map for *when* H1 is lifted:
once workers run steps concurrently, event-seq allocation + append, `GetRun`
reads, and steplog COPY become the next things to watch (H2).

## 5. Hypothesis verdicts

| # | hypothesis | verdict | evidence |
|---|---|---|---|
| **H1** | serial per-worker execution | **CONFIRMED — the top binder** | worker 2.3 % CPU while queue 500+ deep; throughput scales linearly with K (5.5×K steps/s); serial `deliver` loop + one consumer per process in code |
| **H2** | Postgres write amplification | **not reached at K=8** | top-25 = 43 s exec / 540 s wall on 2 CPU (~8 % of a core); all sub-ms. Ranked for post-H1 re-measurement (19.5) |
| **H3** | outbox drain contention | **not reached** | outbox backlog ~0 at all loads; no `task_outbox` lock waits |
| **H4** | join-counter hot row | **REFUTED at K=8** | fanout-50 saturates at the *same* ~48 steps/s as linear-10; zero lock waits during the 50-way join |
| **H5** | single-stream / Redis CPU | **not reached** | Redis idle; throughput tracks K, not a Redis ceiling |
| **H6** | submission-path cost | **not the binder** | API submit p99 194 ms only under saturation-induced contention; API CPU 13.8 % (mostly loadgen `GetRun` polling) |
| **H7** | pgx pool exhaustion | **not reached** | 34 idle Postgres connections at saturation; no connection waits |

## 6. Selected top target for remediation (19.4)

**H1 — serial per-worker execution — is the top bottleneck**, confirmed by both
the linear-K-scaling law and the ~2 % worker CPU at saturation. It is
graph-shape-independent: linear chains, wide fan-outs, planner expansions, and
agent loops all hit the same ~45–50 steps/s ceiling at K=8.

**Remediation (plan §8):** introduce **bounded in-worker step concurrency** — a
pool of N concurrent claim/execute loops per worker process — so fleet
step-concurrency becomes K × N instead of K, decoupling throughput from replica
count. (Scaling K alone also works and is proven linear, but in-worker
concurrency is the cheaper lever for an I/O-bound workload where each worker is
98 % idle.) This is **not** in ROADMAP 19.4's default pre-authorised list (batch
inserts / LISTEN-NOTIFY drain / indexes / join-counter) — those target the
Postgres binders (H2–H4/H7), which are not yet saturated and move to 19.5's
post-fix re-measurement. 19.4's scope is therefore in-worker concurrency, with an
**ADR-002/ADR-005 amendment** documenting the change.

**Quantified target for 19.4:** at K=8, raise sustained fleet throughput from the
baseline **~45 steps/s** to **≥ 4× (~180 steps/s)** with in-worker concurrency
N≈8, keeping scheduling p99 ≤ 2 s at the new knee and worker CPU the binding
signal (i.e. CPU should finally start rising). Correctness suites (5.8 chaos,
13.5 expansion matrix) must stay green — in-worker concurrency multiplies the
in-flight-lease count per process, so the lease/heartbeat/fencing paths get
re-verified under it.

## 7. SLO ratification (plan §3)

The plan §3 targets are **not met at K=8** — but that is the expected pre-fix
state, not a hardware limit. Scheduling p50 ≤ 250 ms holds comfortably *below*
the knee (measured ~7–10 ms); the p99 ≤ 2 s and the 1,000-active-run target break
at the H1 ceiling (~45 steps/s ⇒ ~4.5 concurrent linear-10 runs/s ⇒ the 10-min
active-run count is bounded by throughput, not by the definition of "active").
The targets are retained unchanged; 19.4/19.5 close the gap and 19.6 certifies
against them.

## 8. Side findings (surfaced by the campaign)

- **loadgen false `run_failed` at high concurrency (fixed).** A stale
  poll/reconcile read (issued before a run finished) could land *after* the
  fresh terminal firehose event and clobber the run's status back to `running`;
  `classOf` then read `terminal && status==running` as `run_failed`. Fixed in
  `internal/loadgen/tracker.go` (never overwrite an already-terminal status) with
  a regression test. The engine was correct throughout (all runs succeeded, 0
  DLQ) — this was a measurement artifact.
- **planner-heavy did not expand on the load mock (fixed).** The load mock's
  `default` text response (needed to keep linear-10's chained echo bounded)
  suppressed the mock's structured plan-echo, so `planner` steps returned prose
  and dead-lettered on the implicit `json_schema` validator. Fixed by adding a
  `test/load/mock.json` rule that matches the planner prompt (substring
  `"schema_version"`) with an empty outcome, which makes the mock echo the plan
  verbatim as native structured output. planner-heavy now expands and succeeds
  offline.
