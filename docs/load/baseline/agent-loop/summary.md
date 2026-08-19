# Load campaign — agent-loop

- generated: 2026-08-19 21:38:21 UTC
- agentloom: dev | host: darwin/arm64 (8 CPU)
- API: http://localhost:8080 | track: firehose | sched-sample: 0.12 | inline: false
- steady window: 150s (warmup 20s)

## Arrival rate (open-loop accuracy)

| offered/s | achieved/s | error | intended | submitted | accepted | pacer-lag p99 | pacer-lag max |
|---|---|---|---|---|---|---|---|
| 4.3 | 4.3 | +0.0% | 645 | 645 | 645 | 2.1 ms | 10.2 ms |

## Throughput & active runs

- terminal throughput (steady): **4.3 runs/s**
- peak concurrently-active runs: **61**

## Latency (steady window)

| metric | count | p50 | p90 | p99 | p999 | max | mean |
|---|---|---|---|---|---|---|---|
| submit_rtt | 645 | 4.9 ms | 14.6 ms | 41.2 ms | 195.4 ms | 195.4 ms | 7.7 ms |
| submit_from_intended | 645 | 5.7 ms | 15.9 ms | 42.1 ms | 196.7 ms | 196.7 ms | 8.5 ms |
| end_to_end | 645 | 1863.4 ms | 7965.9 ms | 12715.6 ms | 15137.7 ms | 15137.7 ms | 2997.7 ms |
| scheduling | 717 | 18.8 ms | 587.5 ms | 2587.7 ms | 3693.0 ms | 3693.0 ms | 204.2 ms |

## Outcomes

| class | count | examples |
|---|---|---|
| run_succeeded | 665 |  |

## Ramp steps (knee finder)

The offered rate climbs per step; the knee is where **backlog** (accepted−terminal)
starts growing and **e2e p99** diverges. Cross-check against the Prometheus
scheduling-latency series (the authoritative source).

| step | rate/s | intended | accepted | terminal | backlog | ok | fail | e2e p50 | e2e p99 |
|---|---|---|---|---|---|---|---|---|---|
| 0 | 1.0 | 25 | 25 | 25 | 0 | 25 | 0 | 1191 ms | 1819 ms |
| 1 | 2.0 | 50 | 50 | 50 | 0 | 50 | 0 | 1419 ms | 2871 ms |
| 2 | 3.0 | 76 | 76 | 76 | 0 | 76 | 0 | 1441 ms | 2861 ms |
| 3 | 4.0 | 99 | 99 | 99 | 0 | 99 | 0 | 1633 ms | 3275 ms |
| 4 | 5.0 | 125 | 125 | 125 | 0 | 125 | 0 | 1360 ms | 2459 ms |
| 5 | 6.0 | 150 | 150 | 150 | 0 | 150 | 0 | 2217 ms | 5006 ms |
| 6 | 7.0 | 140 | 140 | 140 | 0 | 140 | 0 | 7755 ms | 13322 ms |

## Integrity

- lost runs: **0**
- non-deliberate dead letters: **0** (DLQ open 3 → 3)
- quiescence reached: **true** (ready 0, pending 0, delayed 0, outbox 0 after 0s)

## SLO evaluation

- scheduling p50: 18.8ms vs 250ms → PASS
- scheduling p99: 2587.7ms vs 2s → FAIL
- api submit p99: 41.2ms vs 100ms → PASS
- end-to-end p99: 12715.6ms vs 30s → PASS

_clock skew (server−client) estimate: -4.1 ms_
