package loadgen

import (
	"testing"
	"time"

	"github.com/mathcslearner/agentloom/internal/loadtest"
)

func constScenario(rate float64) *loadtest.Scenario {
	return &loadtest.Scenario{Arrival: loadtest.Arrival{Mode: loadtest.ArrivalConstant, RatePerSec: rate}}
}

func TestBuildScheduleConstant(t *testing.T) {
	s := constScenario(10)
	fires := buildSchedule(s, 5*time.Second, 0, nil)
	// 10/s for 5s → offsets at 0, 100ms, ... up to <5s ⇒ 50 fires.
	if len(fires) != 50 {
		t.Fatalf("got %d fires, want 50", len(fires))
	}
	if fires[0].Offset != 0 {
		t.Errorf("first offset = %v, want 0", fires[0].Offset)
	}
	if fires[1].Offset != 100*time.Millisecond {
		t.Errorf("second offset = %v, want 100ms", fires[1].Offset)
	}
	// Intended offsets are unaffected by anything downstream — they are a pure
	// function of the rate (the open-loop guarantee's schedule half).
	if fires[49].Offset != 4900*time.Millisecond {
		t.Errorf("last offset = %v, want 4.9s", fires[49].Offset)
	}
}

func TestBuildScheduleMaxRuns(t *testing.T) {
	fires := buildSchedule(constScenario(1000), time.Hour, 100, nil)
	if len(fires) != 100 {
		t.Fatalf("got %d fires, want 100 (maxRuns cap)", len(fires))
	}
}

func TestBuildScheduleRamp(t *testing.T) {
	s := &loadtest.Scenario{Arrival: loadtest.Arrival{
		Mode: loadtest.ArrivalRamp,
		Ramp: &loadtest.Ramp{FromPerSec: 2, ToPerSec: 6, StepPerSec: 2, StepDuration: loadtest.Duration(time.Second)},
	}}
	// [0,1)s rate 2 → 2 fires, [1,2)s rate 4 → 4, [2,3)s rate 6 → 7. The last
	// second yields 7 not 6 because the gap is integer nanoseconds: 1e9/6
	// floors to 166_666_666ns, so six steps span only 999_999_996ns, leaving
	// room for a seventh fire before 3s. Exact and deterministic.
	fires := buildSchedule(s, 3*time.Second, 0, nil)
	if len(fires) != 13 {
		t.Fatalf("got %d fires, want 13", len(fires))
	}
	if rateAt(s.Arrival, 2500*time.Millisecond) != 6 {
		t.Errorf("rate at 2.5s = %v, want 6", rateAt(s.Arrival, 2500*time.Millisecond))
	}
	// Ramp is clamped at to_per_sec.
	if rateAt(s.Arrival, 10*time.Second) != 6 {
		t.Errorf("rate past top = %v, want 6 (clamped)", rateAt(s.Arrival, 10*time.Second))
	}
}

func TestMixDrawDeterministicAndWeighted(t *testing.T) {
	entries := []loadtest.MixEntry{{Scenario: "a", Weight: 0.7}, {Scenario: "b", Weight: 0.3}}
	m1 := newMixDraw(entries, 42)
	m2 := newMixDraw(entries, 42)
	counts := map[string]int{}
	for i := 0; i < 10000; i++ {
		x1, x2 := m1.next(), m2.next()
		if x1 != x2 {
			t.Fatalf("mix draw not deterministic at %d: %q vs %q", i, x1, x2)
		}
		counts[x1]++
	}
	// ~70/30 within a loose tolerance.
	if counts["a"] < 6600 || counts["a"] > 7400 {
		t.Errorf("weight a = %d/10000, want ~7000", counts["a"])
	}
}
