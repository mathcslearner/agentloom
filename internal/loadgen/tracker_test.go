package loadgen

import (
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mathcslearner/agentloom/internal/api"
	"github.com/mathcslearner/agentloom/internal/event"
)

func TestTrackerLifecycleAndTaxonomy(t *testing.T) {
	tr := newTracker(1.0, 0) // sample everything, no skew
	base := time.Now()

	// 0: accepted → succeeds via firehose.
	tr.registerFire(0, "", base, true)
	rid0 := uuid.NewString()
	tr.recordSubmit(0, submitResult{RunID: rid0, Status: 201, RTT: 5 * time.Millisecond}, base.Add(1*time.Millisecond))
	if !tr.applyEvent(feedEvent{RunID: rid0, Type: event.TypeRunSucceeded, Ts: base.Add(500 * time.Millisecond)}) {
		t.Fatal("run 0 should have transitioned to terminal")
	}

	// 1: accepted → fails via poll body.
	tr.registerFire(1, "", base, true)
	rid1 := uuid.NewString()
	tr.recordSubmit(1, submitResult{RunID: rid1, Status: 201, RTT: 5 * time.Millisecond}, base)
	fin := base.Add(800 * time.Millisecond)
	tr.applyRunBody(api.RunResponse{Run: api.RunView{ID: rid1, Status: "failed", StepsTotal: 3, StepsFailed: 1, FinishedAt: &fin}})

	// 2: rejected 429.
	tr.registerFire(2, "", base, true)
	tr.recordSubmit(2, submitResult{Status: 429, Code: "rate_limited"}, base)

	// 3: skipped by inflight cap.
	tr.registerFire(3, "", base, true)
	tr.recordSkip(3)

	tax := tr.taxonomy(10)
	if tax[classRunSucceeded].Count != 1 {
		t.Errorf("succeeded = %d, want 1", tax[classRunSucceeded].Count)
	}
	if tax[classRunFailed].Count != 1 {
		t.Errorf("failed = %d, want 1", tax[classRunFailed].Count)
	}
	if tax[classSubmit4xx].Count != 1 {
		t.Errorf("submit_4xx = %d, want 1", tax[classSubmit4xx].Count)
	}
	if tax[classInflightCap].Count != 1 {
		t.Errorf("inflight_cap = %d, want 1", tax[classInflightCap].Count)
	}
	if tr.e2e.Count() != 2 {
		t.Errorf("e2e samples = %d, want 2 (both terminal runs in steady)", tr.e2e.Count())
	}
}

func TestTrackerSchedulingPairing(t *testing.T) {
	tr := newTracker(1.0, 0)
	base := time.Now()
	tr.registerFire(0, "", base, true)
	rid := uuid.NewString()
	tr.recordSubmit(0, submitResult{RunID: rid, Status: 201, RTT: time.Millisecond}, base)

	// ready → claimed 40ms later ⇒ one scheduling sample.
	tr.applyEvent(feedEvent{RunID: rid, Type: event.TypeStepReady, StepID: "a", Ts: base.Add(100 * time.Millisecond)})
	tr.applyEvent(feedEvent{RunID: rid, Type: event.TypeStepClaimed, StepID: "a", Ts: base.Add(140 * time.Millisecond)})
	// A retry's claim (no preceding ready pair) must NOT record a sample.
	tr.applyEvent(feedEvent{RunID: rid, Type: event.TypeStepClaimed, StepID: "a", Ts: base.Add(300 * time.Millisecond)})

	if tr.sched.Count() != 1 {
		t.Fatalf("scheduling samples = %d, want 1 (retry claim excluded)", tr.sched.Count())
	}
	got := tr.sched.ValueAtQuantile(0.5)
	if got < 39_000 || got > 41_000 { // ~40ms in µs, within bucket error
		t.Errorf("scheduling p50 = %dµs, want ~40000", got)
	}
}

func TestTrackerFinalizeOpenAndLost(t *testing.T) {
	tr := newTracker(0, 0)
	base := time.Now()
	tr.registerFire(0, "", base, true)
	rid := uuid.NewString()
	tr.recordSubmit(0, submitResult{RunID: rid, Status: 201, RTT: time.Millisecond}, base)
	// Never terminal → finalizeOpen classes it a timeout.
	tr.finalizeOpen()
	if c := classOf(&tr.runRows()[0]); c != classRunTimeout {
		t.Errorf("class = %q, want %q", c, classRunTimeout)
	}
	// Reconciliation cannot find it → lost.
	tr.markLost(rid)
	if c := classOf(&tr.runRows()[0]); c != classRunLost {
		t.Errorf("class after markLost = %q, want %q", c, classRunLost)
	}
}

// TestTrackerStalePollDoesNotClobberTerminal guards the high-concurrency race
// (seen in the 19.3 linear-10 campaign) where a poll/reconcile read issued
// before a run finished returns "running" and lands *after* the fresh terminal
// firehose event. The terminal status must win, or classOf misreads the run as
// run_failed.
func TestTrackerStalePollDoesNotClobberTerminal(t *testing.T) {
	tr := newTracker(1.0, 0)
	base := time.Now()
	tr.registerFire(0, "", base, true)
	rid := uuid.NewString()
	tr.recordSubmit(0, submitResult{RunID: rid, Status: 201, RTT: 5 * time.Millisecond}, base.Add(time.Millisecond))

	// Fresh terminal event marks it succeeded.
	if !tr.applyEvent(feedEvent{RunID: rid, Type: event.TypeRunSucceeded, Ts: base.Add(500 * time.Millisecond)}) {
		t.Fatal("run should have gone terminal on the succeeded event")
	}
	// A stale reconcile body (status still "running") lands afterwards.
	tr.applyRunBody(api.RunResponse{Run: api.RunView{ID: rid, Status: "running", StepsTotal: 10}})

	tax := tr.taxonomy(0)
	if got := tax[classRunSucceeded].Count; got != 1 {
		t.Errorf("run_succeeded = %d, want 1 (stale poll clobbered terminal status)", got)
	}
	if tax[classRunFailed] != nil && tax[classRunFailed].Count != 0 {
		t.Errorf("run_failed = %d, want 0", tax[classRunFailed].Count)
	}
}
