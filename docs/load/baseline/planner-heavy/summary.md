# Load campaign — planner-heavy

- generated: 2026-08-19 21:45:38 UTC
- agentloom: dev | host: darwin/arm64 (8 CPU)
- API: http://localhost:8080 | track: firehose | sched-sample: 0.10 | inline: false
- steady window: 180s (warmup 20s)

## Arrival rate (open-loop accuracy)

| offered/s | achieved/s | error | intended | submitted | accepted | pacer-lag p99 | pacer-lag max |
|---|---|---|---|---|---|---|---|
| 2.4 | 2.4 | +0.0% | 439 | 439 | 439 | 3.6 ms | 6.9 ms |

## Throughput & active runs

- terminal throughput (steady): **2.4 runs/s**
- peak concurrently-active runs: **164**

## Latency (steady window)

| metric | count | p50 | p90 | p99 | p999 | max | mean |
|---|---|---|---|---|---|---|---|
| submit_rtt | 439 | 5.1 ms | 14.1 ms | 31.5 ms | 67.1 ms | 67.1 ms | 7.2 ms |
| submit_from_intended | 439 | 6.1 ms | 15.4 ms | 32.5 ms | 67.3 ms | 67.3 ms | 8.1 ms |
| end_to_end | 439 | 20297.5 ms | 63107.3 ms | 70406.8 ms | 70855.5 ms | 70855.5 ms | 29091.3 ms |
| scheduling | 819 | 2945.1 ms | 11397.3 ms | 14186.4 ms | 14928.0 ms | 14928.0 ms | 4384.9 ms |

## Outcomes

| class | count | examples |
|---|---|---|
| run_succeeded | 449 |  |

## Ramp steps (knee finder)

The offered rate climbs per step; the knee is where **backlog** (accepted−terminal)
starts growing and **e2e p99** diverges. Cross-check against the Prometheus
scheduling-latency series (the authoritative source).

| step | rate/s | intended | accepted | terminal | backlog | ok | fail | e2e p50 | e2e p99 |
|---|---|---|---|---|---|---|---|---|---|
| 0 | 0.5 | 13 | 13 | 13 | 0 | 13 | 0 | 710 ms | 1980 ms |
| 1 | 1.0 | 24 | 24 | 24 | 0 | 24 | 0 | 2203 ms | 6335 ms |
| 2 | 1.5 | 38 | 38 | 38 | 0 | 38 | 0 | 1874 ms | 3368 ms |
| 3 | 2.0 | 50 | 50 | 50 | 0 | 50 | 0 | 3230 ms | 5731 ms |
| 4 | 2.5 | 62 | 62 | 62 | 0 | 62 | 0 | 6537 ms | 14020 ms |
| 5 | 3.0 | 75 | 75 | 75 | 0 | 75 | 0 | 19080 ms | 32589 ms |
| 6 | 3.5 | 88 | 88 | 88 | 0 | 88 | 0 | 53386 ms | 69825 ms |
| 7 | 4.0 | 99 | 99 | 99 | 0 | 99 | 0 | 60797 ms | 70323 ms |

## Integrity

- lost runs: **0**
- non-deliberate dead letters: **0** (DLQ open 6 → 6)
- quiescence reached: **true** (ready 0, pending 0, delayed 0, outbox 0 after 0s)

## SLO evaluation

- scheduling p50: 2945.1ms vs 250ms → FAIL
- scheduling p99: 14186.4ms vs 2s → FAIL
- api submit p99: 31.5ms vs 100ms → PASS
- end-to-end p99: 70406.8ms vs 30s → FAIL

_clock skew (server−client) estimate: -1.4 ms_
