# Load campaign — mixed

- generated: 2026-08-19 21:50:15 UTC
- agentloom: dev | host: darwin/arm64 (8 CPU)
- API: http://localhost:8080 | track: firehose | sched-sample: 0.10 | inline: false
- steady window: 180s (warmup 20s)

## Arrival rate (open-loop accuracy)

| offered/s | achieved/s | error | intended | submitted | accepted | pacer-lag p99 | pacer-lag max |
|---|---|---|---|---|---|---|---|
| 2.4 | 2.4 | +0.0% | 439 | 439 | 439 | 5.1 ms | 12.2 ms |

## Throughput & active runs

- terminal throughput (steady): **2.4 runs/s**
- peak concurrently-active runs: **172**

## Latency (steady window)

| metric | count | p50 | p90 | p99 | p999 | max | mean |
|---|---|---|---|---|---|---|---|
| submit_rtt | 439 | 5.7 ms | 17.2 ms | 42.5 ms | 102.2 ms | 102.2 ms | 8.9 ms |
| submit_from_intended | 439 | 6.7 ms | 18.4 ms | 54.5 ms | 102.3 ms | 102.3 ms | 9.8 ms |
| end_to_end | 439 | 24278.8 ms | 62482.5 ms | 71110.9 ms | 74615.7 ms | 74615.7 ms | 28578.8 ms |
| scheduling | 983 | 2437.8 ms | 8289.3 ms | 12842.8 ms | 14658.4 ms | 14658.4 ms | 3357.3 ms |

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
| 0 | 0.5 | 13 | 13 | 13 | 0 | 13 | 0 | 1756 ms | 3820 ms |
| 1 | 1.0 | 24 | 24 | 24 | 0 | 24 | 0 | 1510 ms | 4542 ms |
| 2 | 1.5 | 38 | 38 | 38 | 0 | 38 | 0 | 2303 ms | 4808 ms |
| 3 | 2.0 | 50 | 50 | 50 | 0 | 50 | 0 | 3866 ms | 9254 ms |
| 4 | 2.5 | 62 | 62 | 62 | 0 | 62 | 0 | 9233 ms | 22419 ms |
| 5 | 3.0 | 75 | 75 | 75 | 0 | 75 | 0 | 24686 ms | 60349 ms |
| 6 | 3.5 | 88 | 88 | 88 | 0 | 88 | 0 | 62145 ms | 71817 ms |
| 7 | 4.0 | 99 | 99 | 99 | 0 | 99 | 0 | 47158 ms | 60881 ms |

## Integrity

- lost runs: **0**
- non-deliberate dead letters: **0** (DLQ open 6 → 6)
- quiescence reached: **true** (ready 0, pending 0, delayed 0, outbox 0 after 0s)

## SLO evaluation

- scheduling p50: 2437.8ms vs 250ms → FAIL
- scheduling p99: 12842.8ms vs 2s → FAIL
- api submit p99: 42.5ms vs 100ms → PASS
- end-to-end p99: 71110.9ms vs 30s → FAIL

_clock skew (server−client) estimate: -1.8 ms_
