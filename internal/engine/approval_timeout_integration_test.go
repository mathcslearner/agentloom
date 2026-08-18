//go:build integration

package engine_test

import (
	"encoding/json"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/mathcslearner/agentloom/internal/dag"
	"github.com/mathcslearner/agentloom/internal/engine"
	"github.com/mathcslearner/agentloom/internal/obs/metrics"
	"github.com/mathcslearner/agentloom/internal/queue"
	"github.com/mathcslearner/agentloom/internal/queue/queuetest"
	"github.com/mathcslearner/agentloom/internal/store"
	"github.com/mathcslearner/agentloom/internal/store/gen"
)

// Ticket 15.4's integration suite: approval timeouts end to end. Timing is
// fully injected — the engine runs on a fake clock and the spawned consumer's
// promoter tick is parked, so the tests fire the delayed expiry by hand.

// timeoutGate is a parked human_approval gate whose expiry is scheduled in the
// delayed queue, with the fake clock and metrics registry that drive it.
type timeoutGate struct {
	s        *store.Store
	h        *queuetest.Harness
	eng      *engine.Engine
	clk      *fakeClock
	mreg     *prometheus.Registry
	runID    uuid.UUID
	approval gen.Approval
}

// parkTimeoutGate spins up a fake-clock worker on a fixture with a
// human_approval gate carrying a timeout, waits for the gate to park and its
// expiry to be scheduled in the delayed queue, and returns the harness.
func parkTimeoutGate(t *testing.T, fixture string) timeoutGate {
	t.Helper()
	ctx := t.Context()
	s, h, runID := setupWithParams(t, readDef(t, fixture), json.RawMessage(`{"topic": "turtles"}`))
	clk := newFakeClock(testNow)
	d := startDispatcher(t, s, h.Queue())
	mreg := prometheus.NewRegistry()
	wm := metrics.NewWorkerMetrics(mreg)
	eng, err := engine.New(s, approvalRegistry(t), "worker-a",
		engine.WithDispatchNudge(d.Nudge),
		engine.WithClock(clk.Now),
		engine.WithRetryScheduler(h.Delayed()),
		engine.WithExpiryCanceller(h.Delayed()),
		engine.WithMetrics(wm))
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	h.Spawn("worker-a", eng.Handle, retryWorkerConfig())

	waitStep(t, s, runID, "approve_publish", "awaiting_human", func(st gen.RunStep) bool {
		return st.Status == store.StepStatusAwaitingHuman
	})
	waitDelayedLen(t, h, 1)
	approvals, err := s.Approvals().ListByRun(ctx, runID)
	if err != nil || len(approvals) != 1 {
		t.Fatalf("ListByRun approvals = %+v, err %v; want one pending", approvals, err)
	}
	return timeoutGate{s: s, h: h, eng: eng, clk: clk, mreg: mreg, runID: runID, approval: approvals[0]}
}

// fireTimeout advances the fake clock past the gate's deadline and promotes the
// due expiry onto the ready stream — the delayed-queue path a real timeout
// takes. The spawned worker picks it up and applies the on_timeout policy.
func (g timeoutGate) fireTimeout(t *testing.T) {
	t.Helper()
	g.clk.Set(g.approval.TimeoutAt.Add(time.Second))
	g.h.PromoteDue(t.Context(), g.clk.Now(), 16)
}

// timeoutDelivery synthesizes an approval_timeout delivery for the gate, for
// tests that drive the handler directly (the race test) rather than through the
// delayed queue.
func (g timeoutGate) timeoutDelivery() queue.Delivery {
	return queue.Delivery{
		Envelope: queue.Envelope{
			RunID: g.runID, StepID: "approve_publish", Reason: queue.ReasonApprovalTimeout,
		},
		DeliveryCount: 1,
	}
}

func waitDelayedLen(t *testing.T, h *queuetest.Harness, want int64) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if h.DelayedLen(t.Context()) == want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("delayed set never reached length %d (have %d)", want, h.DelayedLen(t.Context()))
}

// TestApprovalTimeoutRejects: the reject policy (approval_gate.json:
// on_timeout=reject, on_reject=fail) fires through the delayed queue,
// dead-letters the gate, fails the run, records the approval expired with a
// timeout-sourced reject decision, and emits one approval_expired event plus
// the two timeout metrics.
func TestApprovalTimeoutRejects(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	g := parkTimeoutGate(t, "approval_gate.json")

	g.fireTimeout(t)
	waitRun(t, g.s, g.runID, store.RunStatusFailed)

	gate, _ := g.s.Steps().Get(ctx, g.runID, "approve_publish")
	if gate.Status != store.StepStatusDeadLettered {
		t.Fatalf("gate status = %q, want dead_lettered", gate.Status)
	}
	ap, _ := g.s.Approvals().Get(ctx, g.approval.ID)
	if ap.Status != store.ApprovalStatusExpired {
		t.Errorf("approval status = %q, want expired", ap.Status)
	}
	if ap.Decision == nil || *ap.Decision != "reject" {
		t.Errorf("decision = %v, want reject", ap.Decision)
	}
	if ap.DecisionSource == nil || *ap.DecisionSource != store.ApprovalSourceTimeout {
		t.Errorf("decision_source = %v, want timeout", ap.DecisionSource)
	}
	if ap.DecidedBy == nil || *ap.DecidedBy != store.ApprovalActorTimeout {
		t.Errorf("decided_by = %v, want system:timeout", ap.DecidedBy)
	}
	if ap.ExpiredAt == nil {
		t.Error("expired_at not stamped")
	}
	if n := countEvents(t, g.s, g.runID, store.EventApprovalExpired); n != 1 {
		t.Errorf("approval_expired events = %d, want 1", n)
	}
	if n := countEvents(t, g.s, g.runID, store.EventApprovalDecided); n != 0 {
		t.Errorf("approval_decided events = %d, want 0 (a timeout is not a human decision)", n)
	}
	if got := counterValue(t, g.mreg, "engine_approval_timeouts_total", map[string]string{"action": "rejected"}); got != 1 {
		t.Errorf("timeouts_total{rejected} = %v, want 1", got)
	}
	if got := counterValue(t, g.mreg, "engine_approval_decisions_total",
		map[string]string{"decision": "reject", "source": "timeout"}); got != 1 {
		t.Errorf("decisions_total{reject,timeout} = %v, want 1", got)
	}
}

// TestApprovalTimeoutApproves: the approve policy (approval_timeout_approve.json)
// auto-approves the original payload, succeeds the gate, and the downstream
// publish consumes the approved payload to run completion.
func TestApprovalTimeoutApproves(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	g := parkTimeoutGate(t, "approval_timeout_approve.json")

	g.fireTimeout(t)
	waitRun(t, g.s, g.runID, store.RunStatusSucceeded)

	gate, _ := g.s.Steps().Get(ctx, g.runID, "approve_publish")
	if gate.Status != store.StepStatusSucceeded {
		t.Fatalf("gate status = %q, want succeeded", gate.Status)
	}
	var out struct {
		Decision string          `json:"decision"`
		Payload  json.RawMessage `json:"payload"`
		Edited   bool            `json:"edited"`
		Source   string          `json:"source"`
	}
	if err := json.Unmarshal(gate.Output, &out); err != nil {
		t.Fatalf("gate output not a decision: %v (%s)", err, gate.Output)
	}
	if out.Decision != "approve" || out.Edited || out.Source != "timeout" {
		t.Errorf("decision output = %+v, want approve/timeout/not-edited (original payload)", out)
	}
	ap, _ := g.s.Approvals().Get(ctx, g.approval.ID)
	if ap.Status != store.ApprovalStatusExpired {
		t.Errorf("approval status = %q, want expired", ap.Status)
	}
	if got := counterValue(t, g.mreg, "engine_approval_timeouts_total", map[string]string{"action": "approved"}); got != 1 {
		t.Errorf("timeouts_total{approved} = %v, want 1", got)
	}
}

// TestApprovalTimeoutParkResumable is DoD-3: the park policy parks the run
// (reason awaiting_human) with the approval still pending, a redelivered expiry
// does not re-park, and the run stays resumable — a human decision settles the
// gate and unpark resumes it to completion.
func TestApprovalTimeoutParkResumable(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	g := parkTimeoutGate(t, "approval_timeout_park.json")

	g.fireTimeout(t)
	// The run parks; the approval stays pending with expired_at marked.
	waitRun(t, g.s, g.runID, store.RunStatusParked)
	ap, _ := g.s.Approvals().Get(ctx, g.approval.ID)
	if ap.Status != store.ApprovalStatusPending {
		t.Errorf("approval status = %q, want still pending under park", ap.Status)
	}
	if ap.ExpiredAt == nil {
		t.Error("expired_at not stamped under park")
	}
	run, _ := g.s.Runs().Get(ctx, g.runID)
	if run.ParkReason == nil || *run.ParkReason != store.ParkReasonAwaitingHuman {
		t.Errorf("park reason = %v, want awaiting_human", run.ParkReason)
	}
	if n := countEvents(t, g.s, g.runID, store.EventApprovalExpired); n != 1 {
		t.Errorf("approval_expired events = %d, want 1", n)
	}

	// A redelivered expiry (the reconciler-heal / duplicate-delivery shape) must
	// not re-park or re-fire the policy — expired_at is the idempotence marker.
	if err := g.eng.Handle(ctx, g.timeoutDelivery()); err != nil {
		t.Fatalf("redelivered expiry Handle: %v", err)
	}
	if n := countEvents(t, g.s, g.runID, store.EventApprovalExpired); n != 1 {
		t.Errorf("approval_expired events after redelivery = %d, want still 1", n)
	}

	// A human decision settles the gate (Decide tolerates a parked run), and
	// unpark resumes the run's other work to completion.
	if _, err := g.eng.Decide(ctx, g.approval.ID, engine.DecideRequest{
		Decision: dag.ApprovalApprove, DecidedBy: "key_op", Source: store.ApprovalSourceHuman,
	}); err != nil {
		t.Fatalf("Decide approve while parked: %v", err)
	}
	if _, err := g.eng.Unpark(ctx, g.runID); err != nil {
		t.Fatalf("Unpark: %v", err)
	}
	waitRun(t, g.s, g.runID, store.RunStatusSucceeded)
}

// TestApprovalEarlyDecisionCancelsExpiry is DoD-1: a human decision before the
// timeout best-effort cancels the pending expiry (ZREM), so the delayed set
// empties and no expiry fires.
func TestApprovalEarlyDecisionCancelsExpiry(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	g := parkTimeoutGate(t, "approval_gate.json")

	// The expiry is scheduled (parkTimeoutGate asserted DelayedLen == 1).
	if _, err := g.eng.Decide(ctx, g.approval.ID, engine.DecideRequest{
		Decision: dag.ApprovalApprove, DecidedBy: "key_op", Source: store.ApprovalSourceHuman,
	}); err != nil {
		t.Fatalf("Decide approve: %v", err)
	}
	// The canceller ZREMed the pending expiry post-commit.
	if n := g.h.DelayedLen(ctx); n != 0 {
		t.Errorf("delayed set length after early decision = %d, want 0 (expiry cancelled)", n)
	}
	waitRun(t, g.s, g.runID, store.RunStatusSucceeded)
}

// TestApprovalStaleExpiryNoOps: when the early decision does NOT cancel the
// expiry (a Control without a canceller), the stale expiry still fires later,
// finds the approval already decided, and ack-drops — no second transition.
func TestApprovalStaleExpiryNoOps(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	g := parkTimeoutGate(t, "approval_gate.json")

	// Decide through a canceller-less control: the delayed expiry is left in
	// place (the ADR's "the CAS, not the ZREM, is the authority" path).
	ctl, err := engine.NewControl(g.s, engine.WithControlClock(g.clk.Now))
	if err != nil {
		t.Fatalf("NewControl: %v", err)
	}
	if _, err := ctl.Decide(ctx, g.approval.ID, engine.DecideRequest{
		Decision: dag.ApprovalApprove, DecidedBy: "key_op", Source: store.ApprovalSourceHuman,
	}); err != nil {
		t.Fatalf("Decide approve: %v", err)
	}
	waitRun(t, g.s, g.runID, store.RunStatusSucceeded)
	if n := g.h.DelayedLen(ctx); n != 1 {
		t.Fatalf("delayed expiry unexpectedly gone (len %d); the no-cancel path needs it present", n)
	}

	// Fire the stale expiry: it finds no pending approval and ack-drops.
	g.fireTimeout(t)
	// Give the consumer a moment to process the (no-op) delivery.
	waitDelayedLen(t, g.h, 0)

	// Exactly one human decision landed; no expiry event, no second transition.
	if n := countEvents(t, g.s, g.runID, store.EventApprovalDecided); n != 1 {
		t.Errorf("approval_decided events = %d, want exactly 1", n)
	}
	if n := countEvents(t, g.s, g.runID, store.EventApprovalExpired); n != 0 {
		t.Errorf("approval_expired events = %d, want 0 (stale expiry no-ops)", n)
	}
	ap, _ := g.s.Approvals().Get(ctx, g.approval.ID)
	if ap.Status != store.ApprovalStatusApproved {
		t.Errorf("approval status = %q, want approved (human decision, not expired)", ap.Status)
	}
}

// TestApprovalDecideVsTimeoutRace is DoD-2: a near-simultaneous human decision
// and timeout expiry both drive the arbiter CAS; exactly one wins and the step
// terminalizes once. Repeated to exercise the race.
func TestApprovalDecideVsTimeoutRace(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	for i := 0; i < 8; i++ {
		g := parkTimeoutGate(t, "approval_gate.json")
		// Past the deadline so the timeout handler applies its policy rather than
		// rescheduling.
		g.clk.Set(g.approval.TimeoutAt.Add(time.Second))

		var wg sync.WaitGroup
		wg.Add(2)
		var decideErr, timeoutErr atomic.Value
		go func() {
			defer wg.Done()
			_, err := g.eng.Decide(ctx, g.approval.ID, engine.DecideRequest{
				Decision: dag.ApprovalApprove, DecidedBy: "key_op", Source: store.ApprovalSourceHuman,
			})
			if err != nil {
				decideErr.Store(err.Error())
			}
		}()
		go func() {
			defer wg.Done()
			// Drive the timeout handler directly (bypass the delayed queue for a
			// tight race). It reject-fails or ack-drops depending on the CAS.
			if err := g.eng.Handle(ctx, g.timeoutDelivery()); err != nil {
				timeoutErr.Store(err.Error())
			}
		}()
		wg.Wait()

		// Exactly one settlement event: either the human approve or the timeout
		// reject won the single CAS.
		decided := countEvents(t, g.s, g.runID, store.EventApprovalDecided)
		expired := countEvents(t, g.s, g.runID, store.EventApprovalExpired)
		if decided+expired != 1 {
			t.Fatalf("iter %d: settlement events decided=%d expired=%d, want exactly one", i, decided, expired)
		}
		// The gate reached exactly one terminal state.
		gate, _ := g.s.Steps().Get(ctx, g.runID, "approve_publish")
		if gate.Status != store.StepStatusSucceeded && gate.Status != store.StepStatusDeadLettered {
			t.Fatalf("iter %d: gate status = %q, want a terminal state", i, gate.Status)
		}
		ap, _ := g.s.Approvals().Get(ctx, g.approval.ID)
		if ap.Status != store.ApprovalStatusApproved && ap.Status != store.ApprovalStatusExpired {
			t.Fatalf("iter %d: approval status = %q, want approved or expired", i, ap.Status)
		}
	}
}

// TestApprovalTimeoutReconcilerHeals: when the delayed expiry is never scheduled
// (a failing scheduler), the reconciler's overdue-approvals scan re-outboxes the
// expiry once its deadline is well past due, and the policy is applied.
func TestApprovalTimeoutReconcilerHeals(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s, h, runID := setupWithParams(t, readDef(t, "approval_gate.json"), json.RawMessage(`{"topic": "turtles"}`))
	clk := newFakeClock(testNow)
	d := startDispatcher(t, s, h.Queue())
	// A failing scheduler: the park's expiry schedule fails, so no delayed
	// member exists — only the reconciler can fire the timeout.
	eng, err := engine.New(s, approvalRegistry(t), "worker-a",
		engine.WithDispatchNudge(d.Nudge),
		engine.WithClock(clk.Now),
		engine.WithRetryScheduler(&failingScheduler{}))
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	h.Spawn("worker-a", eng.Handle, retryWorkerConfig())

	waitStep(t, s, runID, "approve_publish", "awaiting_human", func(st gen.RunStep) bool {
		return st.Status == store.StepStatusAwaitingHuman
	})
	if n := h.DelayedLen(ctx); n != 0 {
		t.Fatalf("delayed set length = %d, want 0 (failing scheduler)", n)
	}
	approvals, _ := s.Approvals().ListByRun(ctx, runID)
	deadline := *approvals[0].TimeoutAt

	// The reconciler sweeps at a time well past the deadline + its stale grace,
	// re-outboxing an approval_timeout expiry; the worker then applies reject.
	rec := reconcilerAt(t, s, deadline.Add(2*time.Minute))
	res := reconcileOnce(t, rec)
	if len(res.ApprovalsHealed) != 1 {
		t.Fatalf("reconciler healed %d approvals, want 1", len(res.ApprovalsHealed))
	}
	// Advance the engine's clock past the deadline so the handler fires (not
	// reschedules) when the dispatcher drains the healed outbox row.
	clk.Set(deadline.Add(2 * time.Minute))
	waitRun(t, s, runID, store.RunStatusFailed)

	ap, _ := s.Approvals().Get(ctx, approvals[0].ID)
	if ap.Status != store.ApprovalStatusExpired {
		t.Errorf("approval status = %q, want expired (reconciler-healed timeout)", ap.Status)
	}
}
