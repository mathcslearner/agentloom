package loadgen

import (
	"context"
	"fmt"
	"io"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/mathcslearner/agentloom/internal/api"
	"github.com/mathcslearner/agentloom/internal/loadtest"
	"github.com/mathcslearner/agentloom/internal/version"
)

// parseRampOverride parses "from:to:step:dur" (e.g. "2:60:2:30s").
func parseRampOverride(s string) (*loadtest.Ramp, bool) {
	parts := strings.Split(s, ":")
	if len(parts) != 4 {
		return nil, false
	}
	from, e1 := strconv.ParseFloat(parts[0], 64)
	to, e2 := strconv.ParseFloat(parts[1], 64)
	step, e3 := strconv.ParseFloat(parts[2], 64)
	dur, e4 := time.ParseDuration(parts[3])
	if e1 != nil || e2 != nil || e3 != nil || e4 != nil {
		return nil, false
	}
	return &loadtest.Ramp{FromPerSec: from, ToPerSec: to, StepPerSec: step, StepDuration: loadtest.Duration(dur)}, true
}

// pollOverdue polls every accepted-but-not-terminal run past its grace period,
// through a bounded worker pool, folding each body into the tracker.
func pollOverdue(ctx context.Context, client *apiClient, tr *tracker, cfg Config, now time.Time) {
	ids := tr.overdueForPoll(cfg.PollAfter, now)
	if len(ids) == 0 {
		return
	}
	sem := make(chan struct{}, cfg.PollWorkers)
	var wg sync.WaitGroup
	for _, id := range ids {
		if ctx.Err() != nil {
			break
		}
		sem <- struct{}{}
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			defer func() { <-sem }()
			body, status, err := client.getRun(ctx, id)
			if err != nil {
				return
			}
			if status == 200 {
				tr.applyRunBody(body)
			}
		}(id)
	}
	wg.Wait()
}

// drainRuns waits for accepted runs to reach terminal after arrivals stop,
// bounded by RunTimeout measured from arrivalsEnd. It keeps polling and prints
// a drain progress line.
func drainRuns(ctx context.Context, tr *tracker, cfg Config, clk clock, arrivalsEnd time.Time, out io.Writer, start time.Time) {
	deadline := arrivalsEnd.Add(cfg.RunTimeout)
	tick := time.NewTicker(500 * time.Millisecond)
	defer tick.Stop()
	for {
		snap := tr.snapshot()
		if snap.Active == 0 {
			return
		}
		if !clk.Now().Before(deadline) {
			printProgress(out, "drain-to", clk.Now().Sub(start).Seconds(), snap, api.SystemStatsResponse{}, nil)
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// reconcile lists every campaign definition's runs and cross-checks the
// submitted set: an accepted run that is genuinely absent from the listing is
// lost; any listed terminal status is folded in. Returns the lost count.
func reconcile(ctx context.Context, client *apiClient, tr *tracker, comps []*component, start time.Time) int {
	seen := map[string]string{} // run id → status
	for _, c := range comps {
		rows, err := client.listRunsByDefinition(ctx, c.defID, start.Add(-time.Minute))
		if err != nil {
			continue // best-effort; the poll backstop already terminated most runs
		}
		for _, rv := range rows {
			seen[rv.ID] = rv.Status
			tr.applyRunBody(api.RunResponse{Run: rv})
		}
	}
	// Any accepted run id absent from every listing is lost.
	lost := 0
	for _, id := range tr.acceptedRunIDs() {
		if _, ok := seen[id]; !ok {
			if tr.isOpen(id) {
				tr.markLost(id)
				lost++
			}
		}
	}
	return lost
}

// waitQuiescent polls the stats endpoint until the queue and outbox drain
// (ready/pending/delayed/outbox all zero) or DrainTimeout elapses.
func waitQuiescent(ctx context.Context, client *apiClient, cfg Config, clk clock) (quiesceView, api.SystemStatsResponse) {
	deadline := clk.Now().Add(cfg.DrainTimeout)
	begin := clk.Now()
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	var last api.SystemStatsResponse
	for {
		stats, err := client.systemStats(ctx)
		if err == nil {
			last = stats
			q := stats.Queue
			ready, pending, delayed := int64(0), int64(0), int64(0)
			if q != nil {
				ready, pending, delayed = q.ReadyDepth, q.Pending, q.Delayed
			}
			outbox := stats.Outbox.Backlog
			if ready <= 0 && pending == 0 && delayed == 0 && outbox == 0 {
				return quiesceView{Reached: true, ReadyDepth: ready, Pending: pending, Delayed: delayed, OutboxBacklog: outbox, WaitedSec: clk.Now().Sub(begin).Seconds()}, last
			}
			if !clk.Now().Before(deadline) {
				return quiesceView{Reached: false, ReadyDepth: ready, Pending: pending, Delayed: delayed, OutboxBacklog: outbox, WaitedSec: clk.Now().Sub(begin).Seconds()}, last
			}
		} else if !clk.Now().Before(deadline) {
			return quiesceView{Reached: false, WaitedSec: clk.Now().Sub(begin).Seconds()}, last
		}
		select {
		case <-ctx.Done():
			return quiesceView{Reached: false, WaitedSec: clk.Now().Sub(begin).Seconds()}, last
		case <-tick.C:
		}
	}
}

// assembleReport builds the final Report from the tracker and campaign state.
func assembleReport(cfg Config, sc *loadtest.Scenario, comps []*component, tr *tracker, pacerLag *Histogram, ts []tsSample, skew time.Duration, start, steadyStart, steadyEnd, arrivalsEnd time.Time, quiesce quiesceView, lost int, dlqStart, dlqEnd int64) Report {
	steadySec := steadyEnd.Sub(steadyStart).Seconds()
	if steadySec <= 0 {
		steadySec = arrivalsEnd.Sub(start).Seconds()
	}

	rows := tr.runRows()
	steadyIntended, steadyTerminal, dlqTotal := 0, 0, 0
	var firstSub, lastSub time.Time
	for i := range rows {
		if rows[i].inSteady {
			steadyIntended++
			if rows[i].terminal {
				steadyTerminal++
			}
			if rows[i].runID != "" && !rows[i].submittedAt.IsZero() {
				if firstSub.IsZero() || rows[i].submittedAt.Before(firstSub) {
					firstSub = rows[i].submittedAt
				}
				if rows[i].submittedAt.After(lastSub) {
					lastSub = rows[i].submittedAt
				}
			}
		}
		dlqTotal += rows[i].dlqCount
	}
	// The rate/throughput denominator is the actual steady arrival span (so a
	// --runs dry run, whose arrivals end before the configured duration, still
	// reports an honest achieved rate). Fall back to the configured window.
	steadySpan := lastSub.Sub(firstSub).Seconds()
	if steadySpan <= 0 {
		steadySpan = steadySec
	}

	defs := map[string]string{}
	for _, c := range comps {
		key := c.name
		if key == "" {
			key = sc.Name
		}
		defs[key] = c.defID
	}

	// Offered rate: the configured constant rate is the authoritative target;
	// for a ramp there is no single rate, so derive it from the intended count.
	offered := float64(steadyIntended) / steadySpan
	if sc.Arrival.Mode == loadtest.ArrivalConstant && sc.Arrival.RatePerSec > 0 {
		offered = sc.Arrival.RatePerSec
	}
	achieved := float64(tr.submitted) / steadySpan
	rateErr := 0.0
	if offered > 0 {
		rateErr = (achieved - offered) / offered * 100
	}

	activeMax := 0
	for _, s := range ts {
		if s.Active > activeMax {
			activeMax = s.Active
		}
	}

	scRaw, _ := marshalScenario(sc)
	rep := Report{
		SchemaVersion: 1,
		GeneratedAt:   time.Now().UTC(),
		Version:       version.Version,
		Host:          hostView{OS: runtime.GOOS, Arch: runtime.GOARCH, CPUs: runtime.NumCPU()},
		Scenario:      scRaw,
		Config: configView{
			APIBase: cfg.APIBase, Scenario: cfg.Scenario, Track: string(cfg.Track),
			SchedSample: cfg.SchedSample, Inline: cfg.Inline, MaxRuns: cfg.MaxRuns,
			MaxInflight: cfg.MaxInflight, Seed: cfg.Seed,
		},
		Windows: windowsView{
			CampaignStart: start.UTC(), SteadyStart: steadyStart.UTC(), SteadyEnd: steadyEnd.UTC(),
			ArrivalsEnd: arrivalsEnd.UTC(), WarmupSec: sc.Warmup.D().Seconds(), SteadySec: steadySec,
		},
		Definitions: defs,
		ClockSkewMs: float64(skew.Microseconds()) / 1000,
		Rate: rateView{
			SteadyIntended: steadyIntended, SteadySubmitted: int(tr.submitted), SteadyAccepted: int(tr.accepted),
			OfferedPerSec: offered, AchievedPerSec: achieved, RateErrorPct: rateErr,
			PacerLagP99Ms: usToMs(pacerLag.ValueAtQuantile(0.99)), PacerLagMaxMs: usToMs(pacerLag.Max()),
		},
		Counts:   countsOf(tr),
		Taxonomy: tr.taxonomy(10),
		Latency: map[string]Percentiles{
			"submit_rtt":           tr.submitRTT.Snapshot(),
			"submit_from_intended": tr.submitCorr.Snapshot(),
			"end_to_end":           tr.e2e.Snapshot(),
			"scheduling":           tr.sched.Snapshot(),
		},
		ThroughputPerSec: float64(steadyTerminal) / steadySpan,
		ActiveMax:        activeMax,
		Quiescence:       quiesce,
		Integrity: integrityView{
			LostRuns: lost, NonDeliberateDLQ: dlqTotal, DLQOpenStart: dlqStart, DLQOpenEnd: dlqEnd,
		},
	}
	rep.RampSteps = rampStepStats(sc.Arrival, start, sc.Warmup.D(), rows)
	rep.SLO = evaluateSLO(sc, rep, pacerLag)
	return rep
}

// countsOf tallies final classes.
func countsOf(tr *tracker) map[string]int {
	out := map[string]int{}
	for name, t := range tr.taxonomy(0) {
		out[name] = t.Count
	}
	return out
}

// evaluateSLO compares the steady percentiles to the scenario's SLO block
// (reporting only — enforcement is the campaign tooling in 19.3/19.6).
func evaluateSLO(sc *loadtest.Scenario, rep Report, _ *Histogram) *sloView {
	if sc.SLO == nil {
		return nil
	}
	v := &sloView{}
	var detail []string
	check := func(label string, target loadtest.Duration, gotUS int64, dst **bool) {
		if target.D() <= 0 {
			return
		}
		pass := gotUS <= target.D().Microseconds()
		b := pass
		*dst = &b
		detail = append(detail, fmt.Sprintf("- %s: %.1fms vs %s → %s", label, float64(gotUS)/1000, target.D(), passLabel(pass)))
	}
	check("scheduling p50", sc.SLO.SchedulingP50, rep.Latency["scheduling"].P50, &v.SchedulingP50Pass)
	check("scheduling p99", sc.SLO.SchedulingP99, rep.Latency["scheduling"].P99, &v.SchedulingP99Pass)
	check("api submit p99", sc.SLO.APIP99, rep.Latency["submit_rtt"].P99, &v.APIP99Pass)
	check("end-to-end p99", sc.SLO.EndToEndP99, rep.Latency["end_to_end"].P99, &v.EndToEndP99Pass)
	v.Detail = strings.Join(detail, "\n")
	return v
}

func passLabel(pass bool) string {
	if pass {
		return "PASS"
	}
	return "FAIL"
}

// marshalScenario re-encodes a scenario for the report (verbatim record of what
// ran, overrides included).
func marshalScenario(sc *loadtest.Scenario) ([]byte, error) {
	return jsonMarshal(sc)
}
