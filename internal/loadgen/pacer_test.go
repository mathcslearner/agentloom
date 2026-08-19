package loadgen

import (
	"context"
	"sync"
	"testing"
	"time"
)

// manualClock is a fake clock the test advances explicitly. SleepUntil never
// blocks — it fast-forwards to the target (or stays put if already past), so
// the pacer loop runs instantly and the test controls "wall time".
type manualClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *manualClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *manualClock) set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if t.After(c.now) {
		c.now = t
	}
}

func (c *manualClock) SleepUntil(_ context.Context, t time.Time) { c.set(t) }

// TestPacerNoCoordinatedOmission proves a slow dispatch does not shift a later
// fire's intended time. Inside dispatch we jump the clock far ahead (simulating
// a submitter that fell way behind); the pacer must still target start+offset
// for every subsequent fire, and report the true lag.
func TestPacerNoCoordinatedOmission(t *testing.T) {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	clk := &manualClock{now: start}
	fires := buildSchedule(constScenario(10), time.Second, 0, nil) // 10 fires, 100ms apart

	var targets []time.Duration
	var lags []time.Duration
	dispatch := func(idx int, f fire, lag time.Duration) {
		// The intended target is start+offset regardless of prior slowness.
		targets = append(targets, f.Offset)
		lags = append(lags, lag)
		// Simulate a very slow submit on the 3rd fire: jump wall-time 5s ahead.
		if idx == 2 {
			clk.set(start.Add(5 * time.Second))
		}
	}
	runPacer(context.Background(), fires, start, clk, dispatch)

	if len(targets) != 10 {
		t.Fatalf("dispatched %d fires, want 10", len(targets))
	}
	// Intended offsets are exactly the schedule's — unshifted by the stall.
	for i := 0; i < 10; i++ {
		want := time.Duration(i) * 100 * time.Millisecond
		if targets[i] != want {
			t.Errorf("fire %d intended offset = %v, want %v (schedule shifted!)", i, targets[i], want)
		}
	}
	// After the stall, fires 3..9 are already overdue → large but real lag.
	if lags[3] < 4*time.Second {
		t.Errorf("fire 3 lag = %v, want a large positive lag after the stall", lags[3])
	}
}

func TestPacerRespectsCancel(t *testing.T) {
	start := time.Now()
	clk := &manualClock{now: start}
	fires := buildSchedule(constScenario(1), time.Hour, 100, nil)
	ctx, cancel := context.WithCancel(context.Background())
	n := 0
	dispatch := func(_ int, _ fire, _ time.Duration) {
		n++
		if n == 3 {
			cancel()
		}
	}
	runPacer(ctx, fires, start, clk, dispatch)
	if n != 3 {
		t.Fatalf("dispatched %d, want 3 (stopped at cancel)", n)
	}
}
