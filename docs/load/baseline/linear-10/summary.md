# Load campaign — linear-10

- generated: 2026-08-19 21:01:23 UTC
- agentloom: dev | host: darwin/arm64 (8 CPU)
- API: http://localhost:8080 | track: firehose | sched-sample: 0.20 | inline: false
- steady window: 240s (warmup 30s)

## Arrival rate (open-loop accuracy)

| offered/s | achieved/s | error | intended | submitted | accepted | pacer-lag p99 | pacer-lag max |
|---|---|---|---|---|---|---|---|
| 5.4 | 5.4 | +0.0% | 1290 | 1290 | 1290 | 3.5 ms | 48.8 ms |

## Throughput & active runs

- terminal throughput (steady): **5.4 runs/s**
- peak concurrently-active runs: **657**

## Latency (steady window)

| metric | count | p50 | p90 | p99 | p999 | max | mean |
|---|---|---|---|---|---|---|---|
| submit_rtt | 1290 | 5.1 ms | 14.2 ms | 194.7 ms | 1251.6 ms | 1460.7 ms | 15.1 ms |
| submit_from_intended | 1290 | 6.0 ms | 15.1 ms | 194.7 ms | 1251.6 ms | 1462.5 ms | 16.0 ms |
| end_to_end | 1290 | 87636.5 ms | 119302.0 ms | 123955.5 ms | 123955.5 ms | 123955.5 ms | 65380.5 ms |
| scheduling | 2716 | 6273.7 ms | 14328.3 ms | 16969.1 ms | 17658.1 ms | 17951.1 ms | 6328.0 ms |

## Outcomes

| class | count | examples |
|---|---|---|
| run_failed | 4 | 9fca8310-2383-4fd2-9452-28945505630e, 32c1e0cf-fe50-485a-bfee-330214a8ec9c, 4d75… |
| run_succeeded | 1316 |  |

## Ramp steps (knee finder)

The offered rate climbs per step; the knee is where **backlog** (accepted−terminal)
starts growing and **e2e p99** diverges. Cross-check against the Prometheus
scheduling-latency series (the authoritative source).

| step | rate/s | intended | accepted | terminal | backlog | ok | fail | e2e p50 | e2e p99 |
|---|---|---|---|---|---|---|---|---|---|
| 0 (warmup) | 1.0 | 30 | 30 | 30 | 0 | 30 | 0 | 1226 ms | 4459 ms |
| 1 | 2.0 | 60 | 60 | 60 | 0 | 60 | 0 | 2044 ms | 4705 ms |
| 2 | 3.0 | 91 | 91 | 91 | 0 | 91 | 0 | 1743 ms | 3250 ms |
| 3 | 4.0 | 119 | 119 | 119 | 0 | 119 | 0 | 2805 ms | 5438 ms |
| 4 | 5.0 | 150 | 150 | 150 | 0 | 149 | 0 | 8712 ms | 18312 ms |
| 5 | 6.0 | 180 | 180 | 180 | 0 | 179 | 0 | 31307 ms | 80208 ms |
| 6 | 7.0 | 210 | 210 | 210 | 0 | 209 | 0 | 104125 ms | 120353 ms |
| 7 | 8.0 | 240 | 240 | 240 | 0 | 239 | 0 | 119152 ms | 123667 ms |
| 8 | 8.0 | 240 | 240 | 240 | 0 | 240 | 0 | 106842 ms | 118015 ms |

## Integrity

- lost runs: **0**
- non-deliberate dead letters: **0** (DLQ open 0 → 0)
- quiescence reached: **true** (ready 0, pending 0, delayed 0, outbox 0 after 0s)

## SLO evaluation

- scheduling p50: 6273.7ms vs 250ms → FAIL
- scheduling p99: 16969.1ms vs 2s → FAIL
- api submit p99: 194.7ms vs 100ms → FAIL
- end-to-end p99: 123955.5ms vs 30s → FAIL

_clock skew (server−client) estimate: -4.7 ms_
