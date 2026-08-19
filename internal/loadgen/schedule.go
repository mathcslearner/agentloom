package loadgen

import (
	"math"
	"math/rand"
	"time"

	"github.com/mathcslearner/agentloom/internal/loadtest"
)

// fire is one scheduled submission: its intended offset from the campaign
// start (t=0) and which mix component it draws. Component is "" for a
// single-definition scenario.
type fire struct {
	Offset    time.Duration
	Component string
}

// buildSchedule produces the deterministic open-loop fire sequence for a
// scenario over [0, total). It is pure — no clock, no IO — so the arrival
// profile is unit-testable exactly (fire counts per window, ramp step counts).
//
// The rate at time t is:
//   - constant: a fixed rate_per_sec for the whole window;
//   - ramp: a piecewise-constant staircase that holds each rate for
//     step_duration before stepping by step_per_sec toward to_per_sec.
//
// Inter-arrival gaps are the reciprocal of the instantaneous rate at the
// current offset, so a saturated system never slows the *intended* schedule —
// the generator submits on this timetable regardless of completion speed (the
// coordinated-omission guard). components draws the per-fire mix component;
// pass nil for a single-definition scenario. total bounds the window; maxRuns
// > 0 additionally caps the total number of fires (the --runs dry-run knob).
func buildSchedule(s *loadtest.Scenario, total time.Duration, maxRuns int, mix *mixDraw) []fire {
	var out []fire
	off := time.Duration(0)
	for off < total {
		comp := ""
		if mix != nil {
			comp = mix.next()
		}
		out = append(out, fire{Offset: off, Component: comp})
		if maxRuns > 0 && len(out) >= maxRuns {
			break
		}
		rate := rateAt(s.Arrival, off)
		if rate <= 0 {
			break // a zero rate would never advance; treat as end of schedule
		}
		gap := time.Duration(float64(time.Second) / rate)
		if gap <= 0 {
			gap = time.Nanosecond
		}
		off += gap
	}
	return out
}

// rateAt returns the instantaneous arrival rate (per second) at offset off.
func rateAt(a loadtest.Arrival, off time.Duration) float64 {
	switch a.Mode {
	case loadtest.ArrivalConstant:
		return a.RatePerSec
	case loadtest.ArrivalRamp:
		if a.Ramp == nil {
			return 0
		}
		r := a.Ramp
		steps := math.Floor(off.Seconds() / r.StepDuration.D().Seconds())
		rate := r.FromPerSec + steps*r.StepPerSec
		if rate > r.ToPerSec {
			rate = r.ToPerSec
		}
		if rate < 0 {
			rate = 0
		}
		return rate
	default:
		return 0
	}
}

// mixDraw is a seeded weighted picker over a composite scenario's components.
// It yields a deterministic sequence given the seed, so a mixed campaign is
// reproducible.
type mixDraw struct {
	rng      *rand.Rand
	names    []string
	cumWeigh []float64
}

// newMixDraw builds a picker from mix entries (weights need not be
// pre-normalized; the scenario parser already enforces they sum to 1).
func newMixDraw(entries []loadtest.MixEntry, seed int64) *mixDraw {
	m := &mixDraw{rng: rand.New(rand.NewSource(seed))} //nolint:gosec // G404: non-crypto; deterministic mix draw
	var cum float64
	for _, e := range entries {
		cum += e.Weight
		m.names = append(m.names, e.Scenario)
		m.cumWeigh = append(m.cumWeigh, cum)
	}
	return m
}

// next draws the next component name.
func (m *mixDraw) next() string {
	if len(m.names) == 0 {
		return ""
	}
	total := m.cumWeigh[len(m.cumWeigh)-1]
	x := m.rng.Float64() * total
	for i, c := range m.cumWeigh {
		if x < c {
			return m.names[i]
		}
	}
	return m.names[len(m.names)-1]
}
