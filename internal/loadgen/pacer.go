package loadgen

import (
	"context"
	"time"
)

// clock abstracts time so the pacer's open-loop property is unit-testable with
// a controlled clock (no coordinated omission).
type clock interface {
	Now() time.Time
	// SleepUntil blocks until t (or ctx is done). It returns immediately if t
	// is already in the past.
	SleepUntil(ctx context.Context, t time.Time)
}

// realClock is the production clock.
type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }

func (realClock) SleepUntil(ctx context.Context, t time.Time) {
	d := time.Until(t)
	if d <= 0 {
		return
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
	case <-timer.C:
	}
}

// runPacer walks the schedule, dispatching each fire at its intended time
// (start + offset). dispatch is called with the actual-minus-intended lag and
// MUST NOT block (production spawns a submit goroutine), so a saturated system
// never shifts a later fire's intended time — the coordinated-omission guard.
// It returns when the schedule is exhausted or ctx is cancelled.
func runPacer(ctx context.Context, fires []fire, start time.Time, clk clock, dispatch func(idx int, f fire, lag time.Duration)) {
	for i, f := range fires {
		target := start.Add(f.Offset)
		clk.SleepUntil(ctx, target)
		if ctx.Err() != nil {
			return
		}
		lag := clk.Now().Sub(target)
		if lag < 0 {
			lag = 0
		}
		dispatch(i, f, lag)
	}
}
