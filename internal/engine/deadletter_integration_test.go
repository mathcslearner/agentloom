//go:build integration

package engine_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/mathcslearner/agentloom/internal/dag"
	"github.com/mathcslearner/agentloom/internal/engine"
	"github.com/mathcslearner/agentloom/internal/exec"
	"github.com/mathcslearner/agentloom/internal/queue"
	"github.com/mathcslearner/agentloom/internal/store"
)

// Ticket 5.4's integration suite: dead-letter handling end to end — the
// continue_independent_branches write-off, the poison path (a handler
// crash loop reaches the DLQ instead of redelivering forever), and the
// requeue op re-executing a dead-lettered step to run completion with its
// budget re-armed from the baseline.

// continueDef: two independent branches under
// continue_independent_branches. doomed dead-letters on its first attempt
// (max_attempts 1); blocked is downstream of it; ok → done is the
// independent branch that must finish.
const continueDef = `{
	"schema_version": 1,
	"name": "continue-branches",
	"on_failure": "continue_independent_branches",
	"steps": [
		{"id": "doomed", "type": "fail_n_times", "config": {"n": 9}, "retry": {"max_attempts": 1}},
		{"id": "blocked", "type": "noop"},
		{"id": "ok", "type": "echo", "config": {"input": {"v": 1}}},
		{"id": "done", "type": "noop"}
	],
	"edges": [
		{"from": "doomed", "to": "blocked"},
		{"from": "ok", "to": "done"}
	]
}`

// TestContinueIndependentBranches is 5.4's write-off acceptance: the
// dead-letter cancels exactly the blocked descendants, the independent
// branch delivers its partial results, and the run terminalizes failed
// once every step is accounted for.
func TestContinueIndependentBranches(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s, h, runID := setup(t, continueDef)
	d := startDispatcher(t, s, h.Queue())
	spawnWorkers(t, s, h, d)

	run := waitRun(t, s, runID, store.RunStatusFailed)
	h.WaitQuiescent(ctx)
	h.RequireHandledOncePerClaim()

	requireStepStatuses(t, s, runID, map[string]string{
		"doomed": store.StepStatusDeadLettered, "blocked": store.StepStatusCancelled,
		"ok": store.StepStatusSucceeded, "done": store.StepStatusSucceeded,
	})
	if run.StepsSucceeded != 2 || run.StepsFailed != 1 || run.StepsCancelled != 1 {
		t.Errorf("run counters = %d succeeded / %d failed / %d cancelled, want 2/1/1",
			run.StepsSucceeded, run.StepsFailed, run.StepsCancelled)
	}
	// The independent branch's output survived — partial results.
	done, err := s.Steps().Get(ctx, runID, "ok")
	if err != nil {
		t.Fatalf("reading step ok: %v", err)
	}
	if len(done.Output) == 0 {
		t.Error("independent branch has no output — partial results were lost")
	}
	// The write-off is recorded with its reason.
	events, err := s.Events().List(ctx, runID, 0, 1000)
	if err != nil {
		t.Fatalf("listing events: %v", err)
	}
	cancelledEvents := 0
	for _, ev := range events {
		if ev.Type != store.EventStepCancelled {
			continue
		}
		cancelledEvents++
		var payload struct {
			StepID string `json:"step_id"`
			Reason string `json:"reason"`
		}
		if err := json.Unmarshal(ev.Payload, &payload); err != nil {
			t.Fatalf("decoding step_cancelled payload: %v", err)
		}
		if payload.StepID != "blocked" || payload.Reason != store.CancelReasonUpstreamDeadLettered {
			t.Errorf("step_cancelled payload = %+v, want blocked/upstream_dead_lettered", payload)
		}
	}
	if cancelledEvents != 1 {
		t.Errorf("step_cancelled events = %d, want 1", cancelledEvents)
	}
	if got := countEvents(t, s, runID, store.EventRunFailed); got != 1 {
		t.Errorf("run_failed events = %d, want exactly 1", got)
	}
	requireOutboxEmpty(t, s)
}

// panickingNoop is a noop-typed executor that panics every invocation —
// the deterministic handler crash loop of ADR-006 taxonomy row 9.
type panickingNoop struct{}

func (panickingNoop) Type() string { return string(dag.StepNoop) }

func (panickingNoop) Execute(context.Context, exec.StepContext) (exec.Output, error) {
	panic("deliberate executor panic")
}

// poisonDef: boom crash-loops; never is downstream and must stay blocked.
const poisonDef = `{
	"schema_version": 1,
	"name": "poison-loop",
	"steps": [
		{"id": "boom", "type": "noop"},
		{"id": "never", "type": "noop"}
	],
	"edges": [{"from": "boom", "to": "never"}]
}`

// TestPoisonMessageReachesDLQ is 5.4's poison acceptance: a handler that
// panics every delivery walks the entry's delivery count to the poison
// threshold, at which point the poison handler dead-letters the step
// (source poison, raw envelope preserved, dangling attempt closed lost),
// applies the run disposition, and acks — the queue quiesces instead of
// redelivering forever.
func TestPoisonMessageReachesDLQ(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s, h, runID := setup(t, poisonDef)
	d := startDispatcher(t, s, h.Queue())

	reg, err := exec.NewRegistry(panickingNoop{})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	eng, err := engine.New(s, reg, "worker-a", engine.WithDispatchNudge(d.Nudge))
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	// Real-time queue timings (the queue package convention): a short
	// lease walks the panicking entry through reclaims to the threshold
	// fast. Threshold 2 → two executions, then diversion.
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

	run := waitRun(t, s, runID, store.RunStatusFailed)
	h.WaitQuiescent(ctx)

	requireStepStatuses(t, s, runID, map[string]string{
		"boom": store.StepStatusDeadLettered, "never": store.StepStatusPending,
	})
	if run.StepsFailed != 1 {
		t.Errorf("steps_failed = %d, want 1", run.StepsFailed)
	}
	// Every judged-nothing execution closed lost: the first by takeover,
	// the last by the poison dead-letter itself.
	attempts, err := s.Attempts().ListByStep(ctx, runID, "boom")
	if err != nil {
		t.Fatalf("listing attempts: %v", err)
	}
	if len(attempts) == 0 || len(attempts) > 2 {
		t.Fatalf("attempts = %d, want 1..2 (threshold bounds the crash loop)", len(attempts))
	}
	for i, a := range attempts {
		if a.Outcome == nil || *a.Outcome != store.AttemptOutcomeLost {
			t.Errorf("attempt %d outcome = %v, want lost — a crash loop records no judgment", i+1, a.Outcome)
		}
	}
	dls, err := s.DeadLetters().ListByStep(ctx, runID, "boom")
	if err != nil {
		t.Fatalf("listing dead letters: %v", err)
	}
	if len(dls) != 1 {
		t.Fatalf("dead_letters rows = %d, want 1", len(dls))
	}
	dl := dls[0]
	if dl.Source != store.DeadLetterSourcePoison || dl.Class != nil {
		t.Errorf("dead letter = source %q class %v, want poison with NULL class", dl.Source, dl.Class)
	}
	if !strings.Contains(string(dl.Payload), runID.String()) || !strings.Contains(string(dl.Payload), "boom") {
		t.Errorf("payload = %s, want the raw envelope naming the run and step", dl.Payload)
	}
	if got := countEvents(t, s, runID, store.EventStepDeadLettered); got != 1 {
		t.Errorf("step_dead_lettered events = %d, want 1", got)
	}
	requireOutboxEmpty(t, s)
}

// requeueDef: flaky fails its first three attempts under a two-attempt
// budget with deterministic 1s backoff — it exhausts on attempt 2, and
// after a requeue needs the re-armed budget (attempt 3 fails again) to
// reach the succeeding attempt 4.
const requeueDef = `{
	"schema_version": 1,
	"name": "requeue-cures",
	"steps": [
		{"id": "flaky", "type": "fail_n_times", "config": {"n": 3},
		 "retry": {"max_attempts": 2, "backoff": {"initial": "1s", "cap": "1m", "multiplier": 2}, "jitter": "none"}},
		{"id": "after", "type": "noop"}
	],
	"edges": [{"from": "flaky", "to": "after"}]
}`

// TestRequeueReExecutesAndCompletesRun is 5.4's requeue acceptance: the
// exhausted step lands in the DLQ and fails the run (fail_fast); the
// internal requeue op re-opens the run, re-dispatches the step, and the
// re-armed budget carries it through one more transient failure to
// success — the run completes.
func TestRequeueReExecutesAndCompletesRun(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s, h, runID := setup(t, requeueDef)
	clk := newFakeClock(testNow)
	d := startDispatcher(t, s, h.Queue())
	eng := newWorker(t, s, "worker-a",
		engine.WithDispatchNudge(d.Nudge),
		engine.WithClock(clk.Now),
		engine.WithRetryScheduler(h.Delayed()))
	h.Spawn("worker-a", eng.Handle, retryWorkerConfig())

	// Attempt 1 fails; retry 1 due at t0+1s. Fire it: attempt 2 fails and
	// exhausts the budget — dead-lettered, run failed (fail_fast).
	due1 := testNow.Add(time.Second)
	waitRetryScheduled(t, s, h, runID, "flaky", due1)
	clk.Set(due1)
	if res := h.PromoteDue(ctx, due1, 16); res.Promoted != 1 {
		t.Fatalf("promoted %d entries at due time, want 1", res.Promoted)
	}
	waitRun(t, s, runID, store.RunStatusFailed)
	h.WaitQuiescent(ctx)
	requireStepStatuses(t, s, runID, map[string]string{
		"flaky": store.StepStatusDeadLettered, "after": store.StepStatusPending,
	})

	// Requeue: the run re-opens and the step re-dispatches.
	res, err := eng.Requeue(ctx, runID, "flaky")
	if err != nil {
		t.Fatalf("Requeue: %v", err)
	}
	if !res.RunResumed || len(res.Revived) != 0 {
		t.Errorf("RequeueResult = resumed %v revived %v, want resumed with nothing to revive", res.RunResumed, res.Revived)
	}
	if len(res.Dispatched) != 1 || res.Dispatched[0] != "flaky" {
		t.Errorf("Dispatched = %v, want [flaky]", res.Dispatched)
	}

	// Attempt 3 fails (n=3) — but the baseline re-armed the budget, so it
	// retries instead of re-dead-lettering. Fire retry: attempt 4
	// succeeds and the run completes.
	due2 := due1.Add(time.Second)
	waitRetryScheduled(t, s, h, runID, "flaky", due2)
	clk.Set(due2)
	if res := h.PromoteDue(ctx, due2, 16); res.Promoted != 1 {
		t.Fatalf("promoted %d entries at due time, want 1", res.Promoted)
	}
	run := waitRun(t, s, runID, store.RunStatusSucceeded)
	h.WaitQuiescent(ctx)
	h.RequireHandledOncePerClaim()

	requireStepStatuses(t, s, runID, map[string]string{
		"flaky": store.StepStatusSucceeded, "after": store.StepStatusSucceeded,
	})
	requireAttemptOutcomes(t, s, runID, "flaky", []string{
		store.AttemptOutcomeTransient, store.AttemptOutcomeTransient,
		store.AttemptOutcomeTransient, store.StepStatusSucceeded,
	})
	if run.StepsFailed != 0 || run.StepsSucceeded != 2 {
		t.Errorf("run counters = %d/%d (succeeded/failed), want 2/0 — the requeue un-bumped the failure",
			run.StepsSucceeded, run.StepsFailed)
	}
	dls, err := s.DeadLetters().ListByStep(ctx, runID, "flaky")
	if err != nil || len(dls) != 1 {
		t.Fatalf("dead letters = %v (err %v), want the immutable seq-1 record", dls, err)
	}
	for _, typ := range []string{store.EventStepRequeued, store.EventRunResumed} {
		if got := countEvents(t, s, runID, typ); got != 1 {
			t.Errorf("%s events = %d, want 1", typ, got)
		}
	}
	requireOutboxEmpty(t, s)
}

// continueRequeueDef: doomed dead-letters immediately; blocked sits
// downstream; ok is independent. After a requeue, doomed succeeds on
// attempt 2 (n=1) and blocked must be revived and run.
const continueRequeueDef = `{
	"schema_version": 1,
	"name": "continue-requeue",
	"on_failure": "continue_independent_branches",
	"steps": [
		{"id": "doomed", "type": "fail_n_times", "config": {"n": 1}, "retry": {"max_attempts": 1}},
		{"id": "blocked", "type": "noop"},
		{"id": "ok", "type": "noop"}
	],
	"edges": [{"from": "doomed", "to": "blocked"}]
}`

// TestRequeueRevivesWrittenOffDescendants: under
// continue_independent_branches the write-off is undone by the requeue —
// cancelled descendants return to pending, execute once the requeued step
// succeeds, and the run lands succeeded with clean counters.
func TestRequeueRevivesWrittenOffDescendants(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s, h, runID := setup(t, continueRequeueDef)
	d := startDispatcher(t, s, h.Queue())
	spawnWorkers(t, s, h, d)

	// doomed dies on attempt 1; blocked is written off; ok finishes; the
	// run terminalizes failed.
	waitRun(t, s, runID, store.RunStatusFailed)
	h.WaitQuiescent(ctx)
	requireStepStatuses(t, s, runID, map[string]string{
		"doomed": store.StepStatusDeadLettered, "blocked": store.StepStatusCancelled,
		"ok": store.StepStatusSucceeded,
	})

	// Requeue revives the run AND the written-off descendant.
	eng := newWorker(t, s, "requeuer", engine.WithDispatchNudge(d.Nudge))
	res, err := eng.Requeue(ctx, runID, "doomed")
	if err != nil {
		t.Fatalf("Requeue: %v", err)
	}
	if !res.RunResumed {
		t.Error("RunResumed = false, want the failed run re-opened")
	}
	if len(res.Revived) != 1 || res.Revived[0] != "blocked" {
		t.Errorf("Revived = %v, want [blocked]", res.Revived)
	}

	// doomed succeeds on attempt 2, blocked runs, the run completes.
	run := waitRun(t, s, runID, store.RunStatusSucceeded)
	h.WaitQuiescent(ctx)
	h.RequireHandledOncePerClaim()
	requireStepStatuses(t, s, runID, map[string]string{
		"doomed": store.StepStatusSucceeded, "blocked": store.StepStatusSucceeded,
		"ok": store.StepStatusSucceeded,
	})
	if run.StepsFailed != 0 || run.StepsCancelled != 0 || run.StepsSucceeded != 3 {
		t.Errorf("run counters = %d succeeded / %d failed / %d cancelled, want 3/0/0",
			run.StepsSucceeded, run.StepsFailed, run.StepsCancelled)
	}
	if got := countEvents(t, s, runID, store.EventStepRevived); got != 1 {
		t.Errorf("step_revived events = %d, want 1", got)
	}
	requireOutboxEmpty(t, s)
}
