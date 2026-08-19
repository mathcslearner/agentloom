package loadgen

import (
	"sort"
	"time"

	"github.com/mathcslearner/agentloom/internal/loadtest"
)

// RampStepStat is the per-ramp-step client-side breakdown of a ramp campaign.
// A ramp climbs the arrival rate in a staircase (rateAt), so binning the tracked
// runs by the staircase step their intended fire falls in turns the whole-run
// report into a knee-finding table: the step where Backlog (accepted − terminal)
// starts growing monotonically and E2E p99 diverges is the client-side knee,
// cross-checked against the Prometheus scheduling-latency series (the plan's §7
// authoritative source). Emitted only for ramp campaigns.
type RampStepStat struct {
	Step           int     `json:"step"`
	OffsetStartSec float64 `json:"offset_start_sec"`
	RatePerSec     float64 `json:"rate_per_sec"`
	Warmup         bool    `json:"warmup"`
	Intended       int     `json:"intended"`
	Accepted       int     `json:"accepted"`
	Terminal       int     `json:"terminal"`
	Succeeded      int     `json:"succeeded"`
	Failed         int     `json:"failed"`
	Backlog        int     `json:"backlog"` // accepted − terminal: the client-side saturation signal
	E2EP50Ms       float64 `json:"e2e_p50_ms"`
	E2EP99Ms       float64 `json:"e2e_p99_ms"`
}

// rampStepStats bins the tracked runs by the ramp staircase step their intended
// fire falls in. It is pure — a function of the arrival profile, the campaign
// start, the warmup window, and the run rows — so it is unit-testable exactly.
// Returns nil for a non-ramp arrival.
func rampStepStats(a loadtest.Arrival, start time.Time, warmup time.Duration, rows []runState) []RampStepStat {
	if a.Mode != loadtest.ArrivalRamp || a.Ramp == nil {
		return nil
	}
	stepDur := a.Ramp.StepDuration.D()
	if stepDur <= 0 {
		return nil
	}
	type acc struct {
		stat RampStepStat
		e2e  []float64
	}
	bins := map[int]*acc{}
	for i := range rows {
		rs := &rows[i]
		if rs.intended.IsZero() {
			continue
		}
		off := rs.intended.Sub(start)
		if off < 0 {
			off = 0
		}
		idx := int(off / stepDur)
		b := bins[idx]
		if b == nil {
			startOff := time.Duration(idx) * stepDur
			b = &acc{stat: RampStepStat{
				Step:           idx,
				OffsetStartSec: startOff.Seconds(),
				RatePerSec:     rateAt(a, startOff),
				// A bin is warmup iff its whole span sits inside the warmup window.
				Warmup: startOff+stepDur <= warmup,
			}}
			bins[idx] = b
		}
		b.stat.Intended++
		if rs.submitStatus >= 200 && rs.submitStatus < 300 {
			b.stat.Accepted++
		}
		if rs.terminal {
			b.stat.Terminal++
			switch rs.status {
			case "succeeded":
				b.stat.Succeeded++
			case "failed", "cancelled":
				b.stat.Failed++
			}
			if rs.e2eMs > 0 {
				b.e2e = append(b.e2e, rs.e2eMs)
			}
		}
	}
	out := make([]RampStepStat, 0, len(bins))
	for _, b := range bins {
		b.stat.Backlog = b.stat.Accepted - b.stat.Terminal
		if b.stat.Backlog < 0 {
			b.stat.Backlog = 0
		}
		b.stat.E2EP50Ms = pctl(b.e2e, 0.50)
		b.stat.E2EP99Ms = pctl(b.e2e, 0.99)
		out = append(out, b.stat)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Step < out[j].Step })
	return out
}

// pctl is an exact nearest-rank percentile over a small slice (per-bin e2e
// samples are few, so an HDR histogram would be overkill). Returns 0 for empty.
func pctl(xs []float64, q float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	s := make([]float64, len(xs))
	copy(s, xs)
	sort.Float64s(s)
	rank := int(q * float64(len(s)-1))
	if rank < 0 {
		rank = 0
	}
	if rank >= len(s) {
		rank = len(s) - 1
	}
	return s[rank]
}
