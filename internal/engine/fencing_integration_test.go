//go:build integration

package engine_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mathcslearner/agentloom/internal/dag"
	"github.com/mathcslearner/agentloom/internal/engine"
	"github.com/mathcslearner/agentloom/internal/exec"
	"github.com/mathcslearner/agentloom/internal/queue"
	"github.com/mathcslearner/agentloom/internal/queue/queuetest"
	"github.com/mathcslearner/agentloom/internal/store"
)

// Ticket 4.5's fencing suite: the zombie write rejected end to end, and
// the lease-expiry takeover on the claim path (crash cell W3).

// countStepEvents counts the run's events of the given type whose payload
// step_id matches.
func countStepEvents(t *testing.T, s *store.Store, runID uuid.UUID, eventType, stepID string) int {
	t.Helper()
	events, err := s.Events().List(t.Context(), runID, 0, 1000)
	if err != nil {
		t.Fatalf("listing events: %v", err)
	}
	n := 0
	for _, ev := range events {
		if ev.Type != eventType {
			continue
		}
		var payload struct {
			StepID string `json:"step_id"`
		}
		if err := json.Unmarshal(ev.Payload, &payload); err != nil {
			t.Fatalf("decoding %s payload %s: %v", ev.Type, ev.Payload, err)
		}
		if payload.StepID == stepID {
			n++
		}
	}
	return n
}

// requireAttemptHistory asserts the step's attempt rows: one (claim,
// outcome) pair per attempt, in order.
func requireAttemptHistory(t *testing.T, s *store.Store, runID uuid.UUID, stepID string, want []struct {
	claim   uuid.UUID
	outcome string
},
) {
	t.Helper()
	attempts, err := s.Attempts().ListByStep(t.Context(), runID, stepID)
	if err != nil {
		t.Fatalf("listing attempts of %q: %v", stepID, err)
	}
	if len(attempts) != len(want) {
		t.Fatalf("step %q has %d attempts, want %d (%+v)", stepID, len(attempts), len(want), attempts)
	}
	for i, w := range want {
		got := attempts[i]
		if got.ClaimID != w.claim {
			t.Errorf("attempt %d claim = %s, want %s", i+1, got.ClaimID, w.claim)
		}
		if got.Outcome == nil || *got.Outcome != w.outcome {
			t.Errorf("attempt %d outcome = %v, want %q", i+1, got.Outcome, w.outcome)
		}
	}
}

// stallingEcho is worker A's echo-typed executor for the zombie test: it
// signals started, then blocks on release — deliberately ignoring ctx,
// because it simulates a stalled worker (GC pause, VM freeze), not a
// graceful shutdown — and finally returns its own output so the zombie's
// completion attempt carries data that must never land.
type stallingEcho struct {
	started chan struct{}
	release chan struct{}
}

func (*stallingEcho) Type() string { return string(dag.StepEcho) }

func (e *stallingEcho) Execute(context.Context, exec.StepContext) (exec.Output, error) {
	close(e.started)
	<-e.release
	return exec.Output{Data: json.RawMessage(`{"who":"zombie"}`)}, nil
}

const gateDef = `{
	"schema_version": 1,
	"name": "zombie-fencing",
	"steps": [
		{"id": "gate", "type": "echo", "config": {"input": {"who": "b-wins"}}},
		{"id": "next", "type": "noop"}
	],
	"edges": [
		{"from": "gate", "to": "next"}
	]
}`

// TestZombieCompletionFenced is the ticket's headline acceptance: worker A
// claims the gate step and stalls past the lease TTL (its Handle is driven
// directly over a PEL entry it never heartbeats — a perfectly silent
// holder); worker B reclaims the entry, performs the lease-expiry takeover,
// executes, and completes the run. When A resumes, its completion is
// rejected by the fence: the state reflects B's result only, the successor
// was dispatched exactly once, and A's rejection carries both claim IDs.
func TestZombieCompletionFenced(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s, h, runID := setup(t, gateDef)

	// Clear the instantiation dispatch; this test hand-places the entry so
	// worker A can read it before anything else runs.
	if n := loseDispatch(t, s); n != 1 {
		t.Fatalf("lost drain dispatched %d rows, want 1", n)
	}
	h.Enqueue(ctx, stepEnvelope(runID, "gate"))
	msg := h.ReadOne(ctx, "worker-a")

	// Worker A: an engine whose echo executor stalls. Its Handle runs
	// directly (no consumer loop, so no heartbeats — the stall).
	stall := &stallingEcho{started: make(chan struct{}), release: make(chan struct{})}
	regA, err := exec.NewRegistry(stall, exec.NoopExecutor{})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	engineA, err := engine.New(s, regA, "worker-a")
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	handleErr := make(chan error, 1)
	go func() {
		handleErr <- engineA.Handle(ctx, queue.Delivery{
			ID: msg.ID, Envelope: stepEnvelope(runID, "gate"), DeliveryCount: 1,
		})
	}()
	<-stall.started

	// A holds the claim and is "executing"; capture its fencing token.
	gate, err := s.Steps().Get(ctx, runID, "gate")
	if err != nil {
		t.Fatalf("reading gate: %v", err)
	}
	if gate.Status != store.StepStatusRunning || gate.ClaimID == nil {
		t.Fatalf("gate = %s claim=%v, want running under worker A", gate.Status, gate.ClaimID)
	}
	claimA := *gate.ClaimID

	// Worker B: a real consumer with a short lease TTL. Its reclaimer
	// takes the silent entry, performs the takeover, and completes the run
	// (the dispatcher carries the successor's outbox row).
	d := startDispatcher(t, s, h.Queue())
	h.Spawn("worker-b", newWorker(t, s, "worker-b", engine.WithDispatchNudge(d.Nudge)).Handle,
		queuetest.LeaseConfig(400*time.Millisecond))
	waitRun(t, s, runID, store.RunStatusSucceeded)

	gate, err = s.Steps().Get(ctx, runID, "gate")
	if err != nil {
		t.Fatalf("reading gate: %v", err)
	}
	if gate.ClaimID == nil || *gate.ClaimID == claimA {
		t.Fatal("gate still carries worker A's claim after the takeover")
	}
	claimB := *gate.ClaimID

	// Resume the zombie: its completion must be rejected — an error return
	// (the Handle/ACK contract: no ACK) naming both claims.
	close(stall.release)
	select {
	case err = <-handleErr:
	case <-time.After(15 * time.Second):
		t.Fatal("worker A's Handle did not return after release")
	}
	if err == nil {
		t.Fatal("zombie completion returned nil — it would have ACKed")
	}
	if !errors.Is(err, store.ErrConflict) {
		t.Errorf("zombie completion error = %v, want a transition conflict", err)
	}
	if !strings.Contains(err.Error(), claimA.String()) || !strings.Contains(err.Error(), claimB.String()) {
		t.Errorf("fencing rejection %q does not carry both claim IDs (%s, %s)", err, claimA, claimB)
	}

	// State reflects B's result only.
	if !jsonEqual(t, gate.Output, json.RawMessage(`{"who":"b-wins"}`)) {
		t.Errorf("gate output = %s, want worker B's echo (the zombie's output must never land)", gate.Output)
	}
	requireStepStatuses(t, s, runID, map[string]string{
		"gate": store.StepStatusSucceeded, "next": store.StepStatusSucceeded,
	})
	requireAttemptHistory(t, s, runID, "gate", []struct {
		claim   uuid.UUID
		outcome string
	}{
		{claim: claimA, outcome: store.AttemptOutcomeLost},
		{claim: claimB, outcome: store.StepStatusSucceeded},
	})

	// Successors dispatched exactly once (event log asserted), one
	// takeover on record, one completion.
	if got := countStepEvents(t, s, runID, store.EventStepReady, "next"); got != 1 {
		t.Errorf("step_ready(next) events = %d, want exactly 1", got)
	}
	if got := countStepEvents(t, s, runID, store.EventStepReclaimed, "gate"); got != 1 {
		t.Errorf("step_reclaimed(gate) events = %d, want exactly 1", got)
	}
	if got := countStepEvents(t, s, runID, store.EventStepSucceeded, "gate"); got != 1 {
		t.Errorf("step_succeeded(gate) events = %d, want exactly 1", got)
	}
	if got := countEvents(t, s, runID, store.EventRunSucceeded); got != 1 {
		t.Errorf("run_succeeded events = %d, want exactly 1", got)
	}

	// B ACKed the reclaimed entry; nothing dangles.
	h.WaitQuiescent(ctx)
	h.RequireHandledOncePerClaim()
	requireOutboxEmpty(t, s)
}

// TestReclaimedDeliveryTakesOver is crash cell W3 on the claim path: a
// worker claims a step and dies silently (its PEL entry never heartbeats,
// its claim never completes). The reclaimed delivery finds the step
// running, performs the takeover in the claim transaction, re-claims, and
// executes — the attempt history proving the reclaim (4.7's precursor).
func TestReclaimedDeliveryTakesOver(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s, h, runID := setup(t, singleNoop)

	if n := loseDispatch(t, s); n != 1 {
		t.Fatalf("lost drain dispatched %d rows, want 1", n)
	}
	h.Enqueue(ctx, stepEnvelope(runID, "only"))
	h.ReadOne(ctx, "worker-dead")

	// The dead worker got exactly as far as its claim transaction.
	var claimA uuid.UUID
	err := s.WithTx(ctx, func(ctx context.Context, q store.Querier) error {
		step, err := store.ClaimStep(ctx, q, store.ClaimStepArgs{RunID: runID, StepID: "only", Now: testNow})
		if err != nil {
			return err
		}
		claimA = *step.ClaimID
		return nil
	})
	if err != nil {
		t.Fatalf("claiming as the dead worker: %v", err)
	}

	// Worker B's reclaimer picks the entry up (delivery count 2) and the
	// takeover path runs it to completion.
	h.Spawn("worker-b", newWorker(t, s, "worker-b").Handle, queuetest.LeaseConfig(400*time.Millisecond))
	waitRun(t, s, runID, store.RunStatusSucceeded)
	h.WaitQuiescent(ctx)
	h.RequireHandledOncePerClaim()

	step, err := s.Steps().Get(ctx, runID, "only")
	if err != nil {
		t.Fatalf("reading step: %v", err)
	}
	if step.AttemptCount != 2 || step.ClaimID == nil || *step.ClaimID == claimA {
		t.Errorf("step after takeover = attempts %d claim %v, want 2 attempts under a fresh claim", step.AttemptCount, step.ClaimID)
	}
	requireAttemptHistory(t, s, runID, "only", []struct {
		claim   uuid.UUID
		outcome string
	}{
		{claim: claimA, outcome: store.AttemptOutcomeLost},
		{claim: *step.ClaimID, outcome: store.StepStatusSucceeded},
	})
	if got := countStepEvents(t, s, runID, store.EventStepReclaimed, "only"); got != 1 {
		t.Errorf("step_reclaimed events = %d, want exactly 1", got)
	}
	if got := countEvents(t, s, runID, store.EventStepClaimed); got != 2 {
		t.Errorf("step_claimed events = %d, want 2 (the dead worker's and the takeover's)", got)
	}
}
