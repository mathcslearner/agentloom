//go:build integration

package engine_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mathcslearner/agentloom/internal/dag"
	"github.com/mathcslearner/agentloom/internal/engine"
	"github.com/mathcslearner/agentloom/internal/exec"
	"github.com/mathcslearner/agentloom/internal/exec/effects"
	"github.com/mathcslearner/agentloom/internal/queue"
	"github.com/mathcslearner/agentloom/internal/queue/queuetest"
	"github.com/mathcslearner/agentloom/internal/store"
)

// Ticket 5.5's headline suite: the side-effect journal end to end through
// the engine — one external fire across retries (short-circuit), one fire
// and a stable idempotency key across a reclaim/zombie takeover, and
// journal misuse loud in both modes.

// requireEffectFile asserts the external counter file holds exactly one
// line, from attempt 1, stamped with the step's derived idempotency key.
func requireEffectFile(t *testing.T, path string, runID uuid.UUID, stepID string) {
	t.Helper()
	data, err := os.ReadFile(path) // #nosec G304 -- t.TempDir path, test-only
	if err != nil {
		t.Fatalf("reading effect file: %v", err)
	}
	want := fmt.Sprintf("key=%s attempt=1\n", effects.Key(runID, stepID))
	if string(data) != want {
		t.Errorf("effect file = %q, want exactly %q — one fire, on attempt 1, under the stable key", data, want)
	}
}

// retryEffectDef: the effect fires and journals on attempt 1, then
// fail_times=2 fails attempts 1 and 2 after the journal — so the step
// takes three attempts while the external effect must fire once.
func retryEffectDef(path string) string {
	return fmt.Sprintf(`{
		"schema_version": 1,
		"name": "effects-retry",
		"steps": [
			{"id": "notify", "type": "effectful_echo",
			 "config": {"path": %q, "input": {"sent": true}, "fail_times": 2},
			 "retry": {"max_attempts": 3, "backoff": {"initial": "1s", "cap": "1m", "multiplier": 2}, "jitter": "none"}}
		],
		"edges": []
	}`, path)
}

// TestEffectfulEchoRetryShortCircuit: journaled results short-circuit
// re-execution on retry. Attempts 2 and 3 find the journaled result and
// never re-fire the external append; attempt 3 succeeds with the journaled
// echo as its output. Timing runs on the fake clock through the 5.2 retry
// machinery — promotion is driven by hand.
func TestEffectfulEchoRetryShortCircuit(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "notify.log")
	s, h, runID := setup(t, retryEffectDef(path))
	clk := newFakeClock(testNow)
	d := startDispatcher(t, s, h.Queue())
	eng := newWorker(t, s, "worker-a",
		engine.WithDispatchNudge(d.Nudge),
		engine.WithClock(clk.Now),
		engine.WithRetryScheduler(h.Delayed()))
	h.Spawn("worker-a", eng.Handle, retryWorkerConfig())

	// Attempt 1: the effect fires and journals, then the deliberate
	// post-journal failure schedules retry 1.
	due1 := testNow.Add(time.Second)
	waitRetryScheduled(t, s, h, runID, "notify", due1)
	clk.Set(due1)
	if res := h.PromoteDue(ctx, due1, 16); res.Promoted != 1 {
		t.Fatalf("promoted %d entries, want 1", res.Promoted)
	}

	// Attempt 2: short-circuited effect, still fails; retry 2 doubles.
	due2 := due1.Add(2 * time.Second)
	waitRetryScheduled(t, s, h, runID, "notify", due2)
	clk.Set(due2)
	if res := h.PromoteDue(ctx, due2, 16); res.Promoted != 1 {
		t.Fatalf("promoted %d entries, want 1", res.Promoted)
	}

	// Attempt 3: short-circuited effect, past fail_times — success.
	waitRun(t, s, runID, store.RunStatusSucceeded)
	h.WaitQuiescent(ctx)
	h.RequireHandledOncePerClaim()

	requireAttemptOutcomes(t, s, runID, "notify", []string{
		store.AttemptOutcomeTransient, store.AttemptOutcomeTransient, store.StepStatusSucceeded,
	})
	// The whole point: three attempts, one external fire.
	requireEffectFile(t, path, runID, "notify")

	step, err := s.Steps().Get(ctx, runID, "notify")
	if err != nil {
		t.Fatalf("reading step: %v", err)
	}
	if !jsonEqual(t, step.Output, json.RawMessage(`{"sent":true}`)) {
		t.Errorf("step output = %s, want the journaled echo", step.Output)
	}
	effs, err := s.SideEffects().ListByStep(ctx, runID, "notify")
	if err != nil {
		t.Fatalf("listing side effects: %v", err)
	}
	if len(effs) != 1 || effs[0].Status != store.SideEffectStatusDone || effs[0].Attempt != 1 {
		t.Errorf("journal rows = %+v, want one done row recorded by attempt 1", effs)
	}
	requireOutboxEmpty(t, s)
}

// stallAfterJournal is worker A's effectful_echo-typed executor for the
// zombie test: it runs the real executor — journaling the effect and its
// result — then signals started and stalls, deliberately ignoring ctx (a
// stalled worker, not a graceful shutdown), holding its claim past the
// lease TTL with the completion never committed.
type stallAfterJournal struct {
	started chan struct{}
	release chan struct{}
}

func (*stallAfterJournal) Type() string { return string(dag.StepEffectfulEcho) }

func (e *stallAfterJournal) Execute(ctx context.Context, sc exec.StepContext) (exec.Output, error) {
	out, err := exec.EffectfulEchoExecutor{}.Execute(ctx, sc)
	close(e.started)
	<-e.release
	return out, err
}

func reclaimEffectDef(path string) string {
	return fmt.Sprintf(`{
		"schema_version": 1,
		"name": "effects-reclaim",
		"steps": [
			{"id": "notify", "type": "effectful_echo",
			 "config": {"path": %q, "input": {"sent": true}}},
			{"id": "next", "type": "noop"}
		],
		"edges": [{"from": "notify", "to": "next"}]
	}`, path)
}

// TestEffectJournalSurvivesReclaim is the kill/reclaim acceptance: worker A
// journals the effect and stalls before completing; worker B reclaims,
// takes over, and re-executes — the journal short-circuits, so the
// external effect fired exactly once and the file carries the one stable
// idempotency key both workers derived. The resumed zombie's completion is
// fenced as in 4.5.
func TestEffectJournalSurvivesReclaim(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "notify.log")
	s, h, runID := setup(t, reclaimEffectDef(path))

	// Hand-place the entry so worker A reads it before anything else runs.
	if n := loseDispatch(t, s); n != 1 {
		t.Fatalf("lost drain dispatched %d rows, want 1", n)
	}
	h.Enqueue(ctx, stepEnvelope(runID, "notify"))
	msg := h.ReadOne(ctx, "worker-a")

	stall := &stallAfterJournal{started: make(chan struct{}), release: make(chan struct{})}
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
			ID: msg.ID, Envelope: stepEnvelope(runID, "notify"), DeliveryCount: 1,
		})
	}()
	<-stall.started

	// A journaled the effect and stalls holding its claim.
	notify, err := s.Steps().Get(ctx, runID, "notify")
	if err != nil {
		t.Fatalf("reading notify: %v", err)
	}
	if notify.Status != store.StepStatusRunning || notify.ClaimID == nil {
		t.Fatalf("notify = %s claim=%v, want running under worker A", notify.Status, notify.ClaimID)
	}
	claimA := *notify.ClaimID

	// Worker B reclaims, takes over, re-executes (short-circuited), and
	// completes the run.
	d := startDispatcher(t, s, h.Queue())
	h.Spawn("worker-b", newWorker(t, s, "worker-b", engine.WithDispatchNudge(d.Nudge)).Handle,
		queuetest.LeaseConfig(400*time.Millisecond))
	waitRun(t, s, runID, store.RunStatusSucceeded)

	notify, err = s.Steps().Get(ctx, runID, "notify")
	if err != nil {
		t.Fatalf("reading notify: %v", err)
	}
	if notify.ClaimID == nil || *notify.ClaimID == claimA {
		t.Fatal("notify still carries worker A's claim after the takeover")
	}
	claimB := *notify.ClaimID

	// The zombie resumes; its completion must be fenced (no ACK).
	close(stall.release)
	select {
	case err = <-handleErr:
	case <-time.After(15 * time.Second):
		t.Fatal("worker A's Handle did not return after release")
	}
	if err == nil || !errors.Is(err, store.ErrConflict) {
		t.Fatalf("zombie completion error = %v, want a transition conflict", err)
	}

	// One external fire, under the one stable key, despite two executions
	// of the step (A's real one, B's short-circuited one).
	requireEffectFile(t, path, runID, "notify")
	if !jsonEqual(t, notify.Output, json.RawMessage(`{"sent":true}`)) {
		t.Errorf("notify output = %s, want the journaled echo (B's short-circuit result)", notify.Output)
	}
	requireAttemptHistory(t, s, runID, "notify", []struct {
		claim   uuid.UUID
		outcome string
	}{
		{claim: claimA, outcome: store.AttemptOutcomeLost},
		{claim: claimB, outcome: store.StepStatusSucceeded},
	})
	if got := countStepEvents(t, s, runID, store.EventStepReclaimed, "notify"); got != 1 {
		t.Errorf("step_reclaimed events = %d, want 1", got)
	}
	requireStepStatuses(t, s, runID, map[string]string{
		"notify": store.StepStatusSucceeded, "next": store.StepStatusSucceeded,
	})
	h.WaitQuiescent(ctx)
	h.RequireHandledOncePerClaim()
	requireOutboxEmpty(t, s)
}

// misusingNoop is a noop-typed executor that violates the journal
// protocol: it records a result with no Begin token — the "execute
// without intent" bug the misuse detection exists to catch.
type misusingNoop struct{}

func (misusingNoop) Type() string { return string(dag.StepNoop) }

func (misusingNoop) Execute(ctx context.Context, sc exec.StepContext) (exec.Output, error) {
	sj, ok := sc.Effects.(*effects.StepJournal)
	if !ok {
		return exec.Output{}, fmt.Errorf("effects handle is %T, want *effects.StepJournal", sc.Effects)
	}
	if _, err := sj.Complete(ctx, nil, json.RawMessage(`{}`)); err != nil {
		return exec.Output{}, err
	}
	return exec.Output{}, errors.New("misuse was tolerated silently")
}

// TestEffectsMisuseDeadLettersNonStrict: with strict off, misuse is a
// permanent-classified error — the step dead-letters on the first attempt
// with the misuse spelled out, never retried.
func TestEffectsMisuseDeadLettersNonStrict(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s, h, runID := setup(t, soloDef)
	d := startDispatcher(t, s, h.Queue())

	reg, err := exec.NewRegistry(misusingNoop{})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	eng, err := engine.New(s, reg, "worker-a", engine.WithDispatchNudge(d.Nudge))
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	h.Spawn("worker-a", eng.Handle, workerConfig())

	waitRun(t, s, runID, store.RunStatusFailed)
	h.WaitQuiescent(ctx)
	h.RequireHandledOncePerClaim()

	requireAttemptOutcomes(t, s, runID, "solo", []string{store.AttemptOutcomePermanent})
	dls, err := s.DeadLetters().ListByStep(ctx, runID, "solo")
	if err != nil {
		t.Fatalf("listing dead letters: %v", err)
	}
	if len(dls) != 1 || dls[0].Source != store.DeadLetterSourcePermanent {
		t.Fatalf("dead letters = %+v, want one row with source permanent", dls)
	}
	if !strings.Contains(string(dls[0].Error), "journal misuse") {
		t.Errorf("dead-letter error = %s, want the misuse named", dls[0].Error)
	}
	requireOutboxEmpty(t, s)
}

// TestEffectsMisusePanicsStrict: strict mode (the dev/test default in
// config) panics on misuse. The consumer contains the panic without an
// ACK, so the entry crash-loops to the poison threshold and dead-letters
// with source poison — loud in the logs, bounded, durable in the DLQ.
func TestEffectsMisusePanicsStrict(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s, h, runID := setup(t, soloDef)
	d := startDispatcher(t, s, h.Queue())

	reg, err := exec.NewRegistry(misusingNoop{})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	eng, err := engine.New(s, reg, "worker-a",
		engine.WithDispatchNudge(d.Nudge),
		engine.WithStrictEffects(true))
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	// Real-time queue timings, as in the 5.4 poison test: a short lease
	// walks the panicking entry through reclaims to threshold 2.
	h.Spawn("worker-a", eng.Handle, queue.ConsumerConfig{
		Block:             100 * time.Millisecond,
		Batch:             1,
		LeaseTTL:          300 * time.Millisecond,
		HeartbeatInterval: 100 * time.Millisecond,
		ReclaimInterval:   150 * time.Millisecond,
		PoisonThreshold:   2,
		PoisonHandler:     eng.HandlePoison,
		PromoterTick:      time.Hour,
	})

	waitRun(t, s, runID, store.RunStatusFailed)
	h.WaitQuiescent(ctx)

	requireStepStatuses(t, s, runID, map[string]string{"solo": store.StepStatusDeadLettered})
	dls, err := s.DeadLetters().ListByStep(ctx, runID, "solo")
	if err != nil {
		t.Fatalf("listing dead letters: %v", err)
	}
	if len(dls) != 1 || dls[0].Source != store.DeadLetterSourcePoison || dls[0].Class != nil {
		t.Errorf("dead letters = %+v, want one poison row with NULL class", dls)
	}
	requireOutboxEmpty(t, s)
}
