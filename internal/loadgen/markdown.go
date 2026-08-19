package loadgen

import (
	"fmt"
	"sort"
	"strings"
)

// renderMarkdown produces the human-readable campaign report (summary.md).
func renderMarkdown(r Report) string {
	var b strings.Builder
	p := func(format string, a ...any) { fmt.Fprintf(&b, format, a...) }

	p("# Load campaign — %s\n\n", r.Config.Scenario)
	p("- generated: %s\n", r.GeneratedAt.UTC().Format("2006-01-02 15:04:05 UTC"))
	p("- agentloom: %s | host: %s/%s (%d CPU)\n", r.Version, r.Host.OS, r.Host.Arch, r.Host.CPUs)
	p("- API: %s | track: %s | sched-sample: %.2f | inline: %v\n", r.Config.APIBase, r.Config.Track, r.Config.SchedSample, r.Config.Inline)
	p("- steady window: %.0fs (warmup %.0fs)\n\n", r.Windows.SteadySec, r.Windows.WarmupSec)

	p("## Arrival rate (open-loop accuracy)\n\n")
	p("| offered/s | achieved/s | error | intended | submitted | accepted | pacer-lag p99 | pacer-lag max |\n")
	p("|---|---|---|---|---|---|---|---|\n")
	p("| %.1f | %.1f | %+.1f%% | %d | %d | %d | %.1f ms | %.1f ms |\n\n",
		r.Rate.OfferedPerSec, r.Rate.AchievedPerSec, r.Rate.RateErrorPct,
		r.Rate.SteadyIntended, r.Rate.SteadySubmitted, r.Rate.SteadyAccepted,
		r.Rate.PacerLagP99Ms, r.Rate.PacerLagMaxMs)

	p("## Throughput & active runs\n\n")
	p("- terminal throughput (steady): **%.1f runs/s**\n", r.ThroughputPerSec)
	p("- peak concurrently-active runs: **%d**\n\n", r.ActiveMax)

	p("## Latency (steady window)\n\n")
	p("| metric | count | p50 | p90 | p99 | p999 | max | mean |\n")
	p("|---|---|---|---|---|---|---|---|\n")
	for _, name := range sortedKeys(r.Latency) {
		q := r.Latency[name]
		p("| %s | %d | %s | %s | %s | %s | %s | %.1f ms |\n",
			name, q.Count, ms(q.P50), ms(q.P90), ms(q.P99), ms(q.P999), ms(q.Max), q.Mean/1000)
	}
	p("\n")

	p("## Outcomes\n\n| class | count | examples |\n|---|---|---|\n")
	for _, name := range sortedTaxonomy(r.Taxonomy) {
		t := r.Taxonomy[name]
		ex := strings.Join(t.Examples, ", ")
		if len(ex) > 80 {
			ex = ex[:80] + "…"
		}
		p("| %s | %d | %s |\n", name, t.Count, ex)
	}
	p("\n")

	if len(r.RampSteps) > 0 {
		p("## Ramp steps (knee finder)\n\n")
		p("The offered rate climbs per step; the knee is where **backlog** (accepted−terminal)\n")
		p("starts growing and **e2e p99** diverges. Cross-check against the Prometheus\n")
		p("scheduling-latency series (the authoritative source).\n\n")
		p("| step | rate/s | intended | accepted | terminal | backlog | ok | fail | e2e p50 | e2e p99 |\n")
		p("|---|---|---|---|---|---|---|---|---|---|\n")
		for _, rs := range r.RampSteps {
			label := fmt.Sprintf("%d", rs.Step)
			if rs.Warmup {
				label += " (warmup)"
			}
			p("| %s | %.1f | %d | %d | %d | %d | %d | %d | %.0f ms | %.0f ms |\n",
				label, rs.RatePerSec, rs.Intended, rs.Accepted, rs.Terminal, rs.Backlog,
				rs.Succeeded, rs.Failed, rs.E2EP50Ms, rs.E2EP99Ms)
		}
		p("\n")
	}

	p("## Integrity\n\n")
	p("- lost runs: **%d**\n", r.Integrity.LostRuns)
	p("- non-deliberate dead letters: **%d** (DLQ open %d → %d)\n", r.Integrity.NonDeliberateDLQ, r.Integrity.DLQOpenStart, r.Integrity.DLQOpenEnd)
	p("- quiescence reached: **%v** (ready %d, pending %d, delayed %d, outbox %d after %.0fs)\n\n",
		r.Quiescence.Reached, r.Quiescence.ReadyDepth, r.Quiescence.Pending, r.Quiescence.Delayed, r.Quiescence.OutboxBacklog, r.Quiescence.WaitedSec)

	if r.SLO != nil {
		p("## SLO evaluation\n\n%s\n", r.SLO.Detail)
	}
	p("\n_clock skew (server−client) estimate: %.1f ms_\n", r.ClockSkewMs)
	return b.String()
}

func ms(us int64) string { return fmt.Sprintf("%.1f ms", float64(us)/1000) }

func sortedKeys(m map[string]Percentiles) []string {
	// Stable, purpose-ordered so the report reads submit → e2e → sched.
	order := []string{"submit_rtt", "submit_from_intended", "end_to_end", "scheduling"}
	var out []string
	for _, k := range order {
		if _, ok := m[k]; ok {
			out = append(out, k)
		}
	}
	for k := range m {
		if !contains(order, k) {
			out = append(out, k)
		}
	}
	return out
}

func sortedTaxonomy(m map[string]*classTally) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}
