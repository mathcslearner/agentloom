package loadgen

import (
	"testing"
	"time"

	"github.com/mathcslearner/agentloom/internal/loadtest"
)

func TestRampStepStats(t *testing.T) {
	start := time.Unix(1_700_000_000, 0).UTC()
	arr := loadtest.Arrival{
		Mode: loadtest.ArrivalRamp,
		Ramp: &loadtest.Ramp{
			FromPerSec:   1,
			ToPerSec:     3,
			StepPerSec:   1,
			StepDuration: loadtest.Duration(10 * time.Second),
		},
	}
	// Rows: two per step across three staircase steps (offsets 0-30s).
	// Step 0 (rate 1): both terminal-succeeded. Step 1 (rate 2): one succeeded,
	// one still open (backlog). Step 2 (rate 3): one accepted-not-terminal, one
	// rejected submit (not accepted).
	mk := func(offset time.Duration, status int, terminal bool, st string, e2e float64) runState {
		return runState{
			intended:     start.Add(offset),
			submitStatus: status,
			terminal:     terminal,
			status:       st,
			e2eMs:        e2e,
		}
	}
	rows := []runState{
		mk(1*time.Second, 200, true, "succeeded", 100),
		mk(3*time.Second, 200, true, "succeeded", 200),
		mk(11*time.Second, 200, true, "succeeded", 500),
		mk(13*time.Second, 200, false, "running", 0),
		mk(21*time.Second, 200, false, "running", 0),
		mk(23*time.Second, 429, false, "", 0),
	}
	got := rampStepStats(arr, start, 10*time.Second, rows)
	if len(got) != 3 {
		t.Fatalf("want 3 ramp steps, got %d", len(got))
	}

	// Step 0: rate 1, warmup (whole [0,10) span inside the 10s warmup window).
	if got[0].RatePerSec != 1 || !got[0].Warmup {
		t.Errorf("step 0: rate=%.1f warmup=%v, want 1 true", got[0].RatePerSec, got[0].Warmup)
	}
	if got[0].Intended != 2 || got[0].Accepted != 2 || got[0].Terminal != 2 || got[0].Succeeded != 2 || got[0].Backlog != 0 {
		t.Errorf("step 0 counts wrong: %+v", got[0])
	}
	if got[0].E2EP50Ms != 100 || got[0].E2EP99Ms != 100 {
		t.Errorf("step 0 e2e: p50=%.0f p99=%.0f, want 100 100 (nearest-rank, n=2)", got[0].E2EP50Ms, got[0].E2EP99Ms)
	}

	// Step 1: rate 2, not warmup, one terminal + one open → backlog 1.
	if got[1].RatePerSec != 2 || got[1].Warmup {
		t.Errorf("step 1: rate=%.1f warmup=%v, want 2 false", got[1].RatePerSec, got[1].Warmup)
	}
	if got[1].Accepted != 2 || got[1].Terminal != 1 || got[1].Backlog != 1 {
		t.Errorf("step 1 backlog wrong: %+v", got[1])
	}

	// Step 2: rate 3, one accepted-open + one rejected → accepted 1, backlog 1.
	if got[2].RatePerSec != 3 || got[2].Accepted != 1 || got[2].Terminal != 0 || got[2].Backlog != 1 {
		t.Errorf("step 2 wrong: %+v", got[2])
	}
}

func TestRampStepStatsNonRamp(t *testing.T) {
	arr := loadtest.Arrival{Mode: loadtest.ArrivalConstant, RatePerSec: 10}
	if got := rampStepStats(arr, time.Now(), 0, []runState{{intended: time.Now()}}); got != nil {
		t.Errorf("constant arrival should yield nil ramp steps, got %d", len(got))
	}
}
