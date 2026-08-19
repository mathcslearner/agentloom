# Baseline campaign evidence (M19.3)

Curated excerpts from the baseline campaign bundles (`results/` is gitignored).
See [`../findings-baseline.md`](../findings-baseline.md) for the analysis.

Per scenario: `summary.md` (loadgen report incl. the ramp-step knee table),
`pgss-by-time.csv` (top `pg_stat_statements`), `worker.cpu.top.txt` (worker CPU
profile at saturation — the H1 evidence), `env.txt` (git SHA, host, K).

`k-scaling/` holds the K=4/8/12 worker CPU profiles + probe notes — the H1
discriminator (throughput ∝ K while CPU stays idle). Reproduce any bundle with
`scripts/load-campaign.sh` (commands in the findings doc).
