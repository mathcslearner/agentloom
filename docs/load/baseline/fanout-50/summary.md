# Load campaign — fanout-50

- generated: 2026-08-19 21:28:05 UTC
- agentloom: dev | host: darwin/arm64 (8 CPU)
- API: http://localhost:8080 | track: firehose | sched-sample: 0.10 | inline: false
- steady window: 180s (warmup 20s)

## Arrival rate (open-loop accuracy)

| offered/s | achieved/s | error | intended | submitted | accepted | pacer-lag p99 | pacer-lag max |
|---|---|---|---|---|---|---|---|
| 0.8 | 0.8 | +0.0% | 150 | 150 | 150 | 5.5 ms | 15.5 ms |

## Throughput & active runs

- terminal throughput (steady): **0.8 runs/s**
- peak concurrently-active runs: **53**

## Latency (steady window)

| metric | count | p50 | p90 | p99 | p999 | max | mean |
|---|---|---|---|---|---|---|---|
| submit_rtt | 150 | 13.3 ms | 24.1 ms | 99.0 ms | 110.6 ms | 110.6 ms | 16.6 ms |
| submit_from_intended | 150 | 14.1 ms | 25.3 ms | 99.0 ms | 110.9 ms | 110.9 ms | 17.6 ms |
| end_to_end | 150 | 20096.6 ms | 63738.4 ms | 69709.7 ms | 71106.9 ms | 71106.9 ms | 28512.8 ms |
| scheduling | 1058 | 7283.5 ms | 19700.6 ms | 29624.8 ms | 35084.8 ms | 35367.7 ms | 9634.1 ms |

## Outcomes

| class | count | examples |
|---|---|---|
| run_succeeded | 154 |  |

## Ramp steps (knee finder)

The offered rate climbs per step; the knee is where **backlog** (accepted−terminal)
starts growing and **e2e p99** diverges. Cross-check against the Prometheus
scheduling-latency series (the authoritative source).

| step | rate/s | intended | accepted | terminal | backlog | ok | fail | e2e p50 | e2e p99 |
|---|---|---|---|---|---|---|---|---|---|
| 0 | 0.2 | 6 | 6 | 6 | 0 | 6 | 0 | 3111 ms | 6024 ms |
| 1 | 0.4 | 12 | 12 | 12 | 0 | 12 | 0 | 4361 ms | 5551 ms |
| 2 | 0.6 | 19 | 19 | 19 | 0 | 19 | 0 | 3781 ms | 7768 ms |
| 3 | 0.8 | 23 | 23 | 23 | 0 | 23 | 0 | 7167 ms | 12679 ms |
| 4 | 1.0 | 30 | 30 | 30 | 0 | 30 | 0 | 17464 ms | 27940 ms |
| 5 | 1.2 | 36 | 36 | 36 | 0 | 36 | 0 | 48597 ms | 69694 ms |
| 6 | 1.4 | 28 | 28 | 28 | 0 | 28 | 0 | 55545 ms | 68591 ms |

## Integrity

- lost runs: **0**
- non-deliberate dead letters: **0** (DLQ open 0 → 0)
- quiescence reached: **true** (ready 0, pending 0, delayed 0, outbox 0 after 0s)

## SLO evaluation

- scheduling p50: 7283.5ms vs 250ms → FAIL
- scheduling p99: 29624.8ms vs 2s → FAIL
- api submit p99: 99.0ms vs 100ms → PASS
- end-to-end p99: 69709.7ms vs 30s → FAIL

_clock skew (server−client) estimate: -6.2 ms_
