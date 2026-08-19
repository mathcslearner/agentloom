# Load generator (`cmd/loadgen`)

The load generator (ticket 19.2) drives the [scenario corpus](../../test/load/)
against a running API under **open-loop arrival control**, tracks each run's
full lifecycle (submit → terminal), and writes an HDR-percentile report
artifact. It is the tool the baseline campaign (19.3) and the certified re-run
(19.6) use; the methodology, SLOs, and pre-registered hypotheses are in
[`plan.md`](plan.md).

It talks to the engine only through the public HTTP API and reuses
`internal/loadtest`'s scenario parser (so the generator and the CI-validated
corpus share one contract).

## Quick start

Boot the pinned load environment, then run a scenario:

```bash
make load-up                       # resource-pinned stack, 8 workers (ticket 19.1)
export AGENTLOOM_API_URL=http://localhost:8080
export AGENTLOOM_API_KEY=sk_...    # a submit+read key (or the root key)

# 100-run dry run (report under results/linear-10-<utc>/)
go run ./cmd/loadgen --scenario linear-10 --runs 100 --rate 20

# full sustained campaign at the scenario's configured windows
go run ./cmd/loadgen --scenario mixed --out results/mixed
```

`make load-dry-run` is the one-liner for the dry run (override `SCENARIO`,
`RUNS`, `RATE`). `go run ./cmd/loadgen --list-scenarios` prints the corpus.

## Evidence-capturing campaign (19.3)

`scripts/load-campaign.sh` (also `make load-campaign SCENARIO=… ARGS=…`) wraps
one loadgen run and captures the full evidence bundle the baseline campaign
needs — the loadgen report **plus** pprof (worker + API, CPU + heap, fired
mid-steady-window), `pg_stat_statements` (reset at start, top-25 by time and by
calls at the end), `pg_stat_activity` wait-event samples, Redis `INFO`/`LATENCY`,
`docker stats`, and Prometheus range series over the exact `[campaign_start,
arrivals_end]` window — into `results/<scenario>-<utc>/`. It must run against the
pinned `make load-up` stack (pprof and `pg_stat_statements` are enabled only
there).

```bash
make load-up                                    # pinned stack, pprof + pgss on
scripts/load-campaign.sh linear-10 --ramp 1:10:1:60s --run-timeout 15m
scripts/load-campaign.sh fanout-50 --ramp 0.2:2.4:0.2:60s --run-timeout 20m
```

Bundle layout: `summary.{json,md}` + CSVs (the loadgen report), `pprof/*.pprof`
+ `pprof/*.top.txt` (rendered top functions), `pgss-by-{time,calls}.csv`,
`pg-activity.txt`, `redis.*.txt`, `docker-stats.csv`, `prom/*.json`, `env.txt`
(git SHA, host, resolved compose config). The findings doc
([`findings-baseline.md`](findings-baseline.md)) is written from these bundles.

## How it works

- **Open-loop, no coordinated omission.** Submissions fire on a schedule that
  is a pure function of the arrival rate (`buildSchedule`); a slow or saturated
  system never shifts a later submission's *intended* time — it shows up as
  growing latency and pacer lag, not as a throttled submitter. Each fire
  dispatches a submit on its own goroutine, so a stalled request never delays
  the next fire. The report cross-checks the achieved submit rate against the
  offered rate (target: within ±5%).
- **Lifecycle tracking, two channels.** In `firehose` mode the generator tails
  `/v1/events/ws` (a read bearer on the upgrade — no ticket dance) for
  low-latency terminal detection and, on a sampled fraction of runs
  (`--sched-sample`), pairs each step's `step_ready`→`step_claimed` events into
  a scheduling-latency sample. Per-run **polling is the authority**: any
  accepted run past its grace period is polled to terminal, and a final
  reconciliation sweep (`GET /v1/runs?definition_id=`) cross-checks the whole
  submitted set — a firehose gap is never fatal. `--track poll` disables the
  WebSocket entirely.
- **Fresh definition per campaign.** Each scenario's definition is registered
  (or a new version appended if the name exists), so every campaign targets a
  unique `definition_id` and the reconciliation walk is exact.
- **Clock-skew corrected end-to-end latency.** Server terminal timestamps are
  corrected into the client frame by an estimated skew (probed from
  `/v1/system/stats`'s `observed_at`), so e2e latency is free of both
  server↔client skew and firehose/poll delivery lag.

## Report artifact

Written to `--out` (default `results/<scenario>-<utc>/`):

| file | contents |
|---|---|
| `summary.json` | the full machine-readable report: config, windows, definition ids, rate accuracy, latency percentiles (submit RTT / submit-from-intended / end-to-end / scheduling), throughput, active-run peak, **failure taxonomy**, integrity (lost runs, non-deliberate dead letters), quiescence, SLO evaluation, clock skew |
| `summary.md` | the human-readable report (tables) |
| `runs.csv` | one row per submission: intended/submitted offsets, run id, http status, taxonomy class, status, submit/e2e latency, step counts, DLQ, in-steady flag |
| `timeseries.csv` | per-progress-tick series: submitted/accepted/active/terminal + queue depth/PEL/delayed/outbox |
| `hist-*.csv` | the HDR percentile distribution for each latency histogram |

### Ramp-step breakdown (knee finder)

A **ramp** campaign's `summary.json` carries a `ramp_steps[]` array (and a "Ramp
steps" table in `summary.md`): the tracked runs binned by the arrival staircase
step their intended fire fell in, each row reporting the offered `rate_per_sec`,
`intended`/`accepted`/`terminal` counts, `backlog` (accepted−terminal — the
client-side saturation signal), succeeded/failed, and e2e p50/p99. The knee is
the first step where `backlog` starts growing monotonically and `e2e_p99`
diverges, cross-checked against the Prometheus `engine_step_scheduling_latency_seconds`
series (the authoritative source per [`plan.md`](plan.md) §7). Constant-rate
campaigns omit it.

### Failure taxonomy

Every submission lands in exactly one class, and the classes sum to the total:

- `run_succeeded` / `run_failed` / `run_cancelled` — terminal run outcomes
- `run_timeout` — accepted, never reached terminal by `--run-timeout`
- `run_lost` — accepted, but reconciliation could not find the run at all
  (must be 0; `--fail-on-lost` exits non-zero otherwise)
- `submit_http_4xx` (examples carry the API error code, e.g. `rate_limited`) /
  `submit_http_5xx` / `submit_transport_error` / `submit_timeout`
- `skipped_inflight_cap` — a fire suppressed by `--max-inflight` (never silently
  omitted)

## Key flags

| flag | default | meaning |
|---|---|---|
| `--scenario` | — | scenario name (required); `--list-scenarios` to enumerate |
| `--rate` | scenario | override constant arrival rate (per second) |
| `--ramp from:to:step:dur` | scenario | override with a ramp (e.g. `2:60:2:30s`) — the knee-finding profile |
| `--duration` / `--warmup` | scenario | override the steady / warmup windows |
| `--runs N` | 0 (unbounded) | cap total submissions — the dry-run knob |
| `--track firehose\|poll` | firehose | terminal-detection channel |
| `--sched-sample f` | 0.1 | fraction of runs whose scheduling latency is sampled (0 disables step events; Prometheus stays the authoritative source) |
| `--inline` | false | submit the definition body every run (exercises the submission-path cost, H6) |
| `--max-inflight N` | 0 | cap concurrent in-flight submits (0 = pure open loop) |
| `--drain-timeout` | 2m | max wait for queue quiescence after arrivals stop |
| `--out` | `results/<scenario>-<utc>` | report directory |

## Caveats

- **Quiescence needs a dedicated stack.** The post-campaign quiescence check
  requires the *global* queue/outbox to drain to zero. On a shared dev stack
  with unrelated activity it will report "not reached" and wait out
  `--drain-timeout`; run campaigns against `make load-up`'s dedicated
  environment for a clean result.
- **API rate limits.** Against the default dev stack (submit bucket 20/10rps) a
  loadgen above ~10/s will 429 (surfaced as `submit_http_4xx: rate_limited`).
  The load overlay raises the limits ~200× so they never bind while the 429
  path stays wired.
- **Firehose tracking bound.** The firehose caps tracked runs per connection
  (`MaxTrackedRuns`, default 2048); above that, discovery/backfill thrash is
  possible, which is exactly why polling is the authority for terminal
  detection. At knee-scale (>2k active runs) prefer `--sched-sample 0` and lean
  on Prometheus for scheduling latency.
