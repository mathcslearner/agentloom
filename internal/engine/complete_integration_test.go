//go:build integration

package engine_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	"github.com/mathcslearner/agentloom/internal/store/gen"
	"github.com/mathcslearner/agentloom/internal/store/storetest"
)

// Ticket 4.3's integration suite: full execute-and-complete runs across
// two workers. The outbox is drained by a test-local loop (List → Enqueue
// → Delete) standing in for ticket 4.4's dispatcher; deleting only after
// a successful enqueue makes it at-least-once, and the claim CAS
// ACK-drops any duplicates that produces — the system's own contract.

// mustDecode decodes a definition fixture, failing the test on error.
func mustDecode(t *testing.T, defJSON string) *dag.Definition {
	t.Helper()
	def, err := dag.Decode([]byte(defJSON))
	if err != nil {
		t.Fatalf("decoding definition: %v", err)
	}
	return def
}

// workerConfig spreads entries across the two spawned workers (Batch 1)
// with a short block for fast shutdown.
func workerConfig() queue.ConsumerConfig {
	return queue.ConsumerConfig{Block: 500 * time.Millisecond, Batch: 1}
}

// newWorker builds an engine over the full builtin registry.
func newWorker(t *testing.T, s *store.Store, workerID string) *engine.Engine {
	t.Helper()
	e, err := engine.New(s, exec.Builtins(), workerID)
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	return e
}

// spawnWorkers runs two builtin-registry workers against the harness.
func spawnWorkers(t *testing.T, s *store.Store, h *queuetest.Harness) {
	t.Helper()
	h.Spawn("worker-a", newWorker(t, s, "worker-a").Handle, workerConfig())
	h.Spawn("worker-b", newWorker(t, s, "worker-b").Handle, workerConfig())
}

// drainOutbox stands in for the 4.4 dispatcher: poll task_outbox, enqueue
// each row, delete what was enqueued. Runs until test cleanup; errors are
// swallowed (retried next tick) so shutdown races stay quiet.
func drainOutbox(t *testing.T, s *store.Store, h *queuetest.Harness) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	t.Cleanup(func() { cancel(); <-done })
	go func() {
		defer close(done)
		ticker := time.NewTicker(10 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
			tasks, err := s.Outbox().List(ctx, 100)
			if err != nil || len(tasks) == 0 {
				continue
			}
			ids := make([]int64, 0, len(tasks))
			for _, task := range tasks {
				_, err := h.Queue().Enqueue(ctx, queue.Envelope{
					RunID: task.RunID, StepID: task.StepID, Reason: task.Reason,
				})
				if err != nil {
					continue
				}
				ids = append(ids, task.ID)
			}
			if len(ids) > 0 {
				s.Outbox().Delete(ctx, ids) //nolint:errcheck // re-drained rows dedupe at claim
			}
		}
	}()
}

// waitRun polls until the run reaches want, failing on timeout or on a
// different terminal status, and returns the final run row.
func waitRun(t *testing.T, s *store.Store, runID uuid.UUID, want string) gen.Run {
	t.Helper()
	ctx := t.Context()
	deadline := time.Now().Add(15 * time.Second)
	var run gen.Run
	for time.Now().Before(deadline) {
		var err error
		run, err = s.Runs().Get(ctx, runID)
		if err != nil {
			t.Fatalf("reading run: %v", err)
		}
		if run.Status == want {
			return run
		}
		if run.Status != store.RunStatusRunning {
			t.Fatalf("run reached %q, want %q", run.Status, want)
		}
		time.Sleep(15 * time.Millisecond)
	}
	t.Fatalf("run never reached %q (stuck at %q):\n%s", want, run.Status, dumpSteps(t, s, runID))
	return gen.Run{}
}

// dumpSteps renders the run's step states for stuck-run diagnostics.
func dumpSteps(t *testing.T, s *store.Store, runID uuid.UUID) string {
	t.Helper()
	steps, err := s.Steps().ListByRun(context.Background(), runID)
	if err != nil {
		return fmt.Sprintf("(listing steps: %v)", err)
	}
	var b strings.Builder
	for _, st := range steps {
		fmt.Fprintf(&b, "  %s [%s] status=%s remaining=%d fired=%d\n",
			st.StepID, st.StepType, st.Status, st.RemainingDeps, st.FiredDeps)
	}
	return b.String()
}

// requireStepStatuses asserts every step's status matches want exactly.
func requireStepStatuses(t *testing.T, s *store.Store, runID uuid.UUID, want map[string]string) {
	t.Helper()
	steps, err := s.Steps().ListByRun(t.Context(), runID)
	if err != nil {
		t.Fatalf("listing steps: %v", err)
	}
	if len(steps) != len(want) {
		t.Errorf("run has %d steps, want %d", len(steps), len(want))
	}
	for _, st := range steps {
		if w, ok := want[st.StepID]; !ok {
			t.Errorf("unexpected step %q", st.StepID)
		} else if st.Status != w {
			t.Errorf("step %q status = %q, want %q", st.StepID, st.Status, w)
		}
	}
}

// countStepReadyEvents counts the step_ready events whose payload step_id
// matches — the per-step dispatch counter (exactly-once readiness).
func countStepReadyEvents(t *testing.T, s *store.Store, runID uuid.UUID, stepID string) int {
	t.Helper()
	events, err := s.Events().List(t.Context(), runID, 0, 1000)
	if err != nil {
		t.Fatalf("listing events: %v", err)
	}
	n := 0
	for _, ev := range events {
		if ev.Type != store.EventStepReady {
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

// requireOutboxEmpty asserts every outbox row was drained.
func requireOutboxEmpty(t *testing.T, s *store.Store) {
	t.Helper()
	tasks, err := s.Outbox().List(t.Context(), 100)
	if err != nil {
		t.Fatalf("listing outbox: %v", err)
	}
	if len(tasks) != 0 {
		t.Errorf("outbox not drained: %d rows remain (%+v)", len(tasks), tasks)
	}
}

// requireSingleAttempts asserts each step ran exactly once (or never).
func requireSingleAttempts(t *testing.T, s *store.Store, runID uuid.UUID, executed []string) {
	t.Helper()
	for _, stepID := range executed {
		attempts, err := s.Attempts().ListByStep(t.Context(), runID, stepID)
		if err != nil {
			t.Fatalf("listing attempts of %q: %v", stepID, err)
		}
		if len(attempts) != 1 {
			t.Errorf("step %q has %d attempt rows, want exactly 1", stepID, len(attempts))
		}
	}
}

const linearDef = `{
	"schema_version": 1,
	"name": "linear",
	"steps": [
		{"id": "a", "type": "echo", "config": {"input": {"v": 1}}},
		{"id": "b", "type": "noop"},
		{"id": "c", "type": "echo", "config": {"input": {"done": true}}}
	],
	"edges": [
		{"from": "a", "to": "b"},
		{"from": "b", "to": "c"}
	]
}`

// TestLinearRunCompletes: the first end-to-end life — instantiate,
// dispatch, claim, execute, complete, fan out, repeat until the run
// succeeds — across two workers.
func TestLinearRunCompletes(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s, h, runID := setup(t, linearDef)
	spawnWorkers(t, s, h)
	drainOutbox(t, s, h)

	run := waitRun(t, s, runID, store.RunStatusSucceeded)
	h.WaitQuiescent(ctx)
	h.RequireHandledOncePerClaim()

	requireStepStatuses(t, s, runID, map[string]string{
		"a": store.StepStatusSucceeded, "b": store.StepStatusSucceeded, "c": store.StepStatusSucceeded,
	})
	requireSingleAttempts(t, s, runID, []string{"a", "b", "c"})
	requireOutboxEmpty(t, s)
	if run.StepsSucceeded != 3 || run.StepsFailed != 0 || run.StepsSkipped != 0 {
		t.Errorf("run counters = %d/%d/%d (succeeded/failed/skipped), want 3/0/0",
			run.StepsSucceeded, run.StepsFailed, run.StepsSkipped)
	}
	stepA, err := s.Steps().Get(ctx, runID, "a")
	if err != nil {
		t.Fatalf("reading step a: %v", err)
	}
	if !jsonEqual(t, stepA.Output, json.RawMessage(`{"v": 1}`)) {
		t.Errorf("step a output = %s, want the echoed config input", stepA.Output)
	}
	if got := countEvents(t, s, runID, store.EventRunSucceeded); got != 1 {
		t.Errorf("run_succeeded events = %d, want exactly 1", got)
	}
	edges, err := s.Steps().ListEdgesByRun(ctx, runID)
	if err != nil {
		t.Fatalf("listing edges: %v", err)
	}
	for _, e := range edges {
		if e.Resolution != store.EdgeResolutionFired {
			t.Errorf("edge %d (%s→%s) resolution = %q, want fired", e.Ordinal, e.FromStep, e.ToStep, e.Resolution)
		}
	}
}

const fanoutDef = `{
	"schema_version": 1,
	"name": "fanout-fanin",
	"steps": [
		{"id": "start", "type": "noop"},
		{"id": "s1", "type": "sleep", "config": {"duration": "10ms"}},
		{"id": "s2", "type": "sleep", "config": {"duration": "20ms"}},
		{"id": "s3", "type": "sleep", "config": {"duration": "30ms"}},
		{"id": "merge", "type": "join", "config": {"mode": "all"}},
		{"id": "tail", "type": "noop"}
	],
	"edges": [
		{"from": "start", "to": "s1"},
		{"from": "start", "to": "s2"},
		{"from": "start", "to": "s3"},
		{"from": "s1", "to": "merge"},
		{"from": "s2", "to": "merge"},
		{"from": "s3", "to": "merge"},
		{"from": "merge", "to": "tail"}
	]
}`

// TestFanOutFanInCompletes: parallel branches converge on a `join all`;
// the join is dispatched exactly once, only after every parent fired.
func TestFanOutFanInCompletes(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s, h, runID := setup(t, fanoutDef)
	spawnWorkers(t, s, h)
	drainOutbox(t, s, h)

	run := waitRun(t, s, runID, store.RunStatusSucceeded)
	h.WaitQuiescent(ctx)
	h.RequireHandledOncePerClaim()

	requireStepStatuses(t, s, runID, map[string]string{
		"start": store.StepStatusSucceeded, "s1": store.StepStatusSucceeded,
		"s2": store.StepStatusSucceeded, "s3": store.StepStatusSucceeded,
		"merge": store.StepStatusSucceeded, "tail": store.StepStatusSucceeded,
	})
	requireSingleAttempts(t, s, runID, []string{"start", "s1", "s2", "s3", "merge", "tail"})
	requireOutboxEmpty(t, s)
	if run.StepsSucceeded != 6 {
		t.Errorf("steps_succeeded = %d, want 6", run.StepsSucceeded)
	}
	// Exactly one readiness dispatch for the join — the completion
	// transaction of whichever parent fired last.
	if got := countStepReadyEvents(t, s, runID, "merge"); got != 1 {
		t.Errorf("step_ready(merge) events = %d, want exactly 1", got)
	}
	// The join readied only after all three parents succeeded: its
	// step_ready seq is after every parent's step_succeeded seq.
	events, err := s.Events().List(ctx, runID, 0, 1000)
	if err != nil {
		t.Fatalf("listing events: %v", err)
	}
	var mergeReadySeq int64
	parentDone := map[string]int64{}
	for _, ev := range events {
		var payload struct {
			StepID string `json:"step_id"`
		}
		_ = json.Unmarshal(ev.Payload, &payload)
		if ev.Type == store.EventStepReady && payload.StepID == "merge" {
			mergeReadySeq = ev.Seq
		}
		if ev.Type == store.EventStepSucceeded && strings.HasPrefix(payload.StepID, "s") && payload.StepID != "start" {
			parentDone[payload.StepID] = ev.Seq
		}
	}
	for parent, seq := range parentDone {
		if mergeReadySeq < seq {
			t.Errorf("merge readied (seq %d) before parent %s succeeded (seq %d)", mergeReadySeq, parent, seq)
		}
	}
}

const branchDef = `{
	"schema_version": 1,
	"name": "conditional-branch",
	"steps": [
		{"id": "route", "type": "branch", "config": {"input": {"category": "refund"}}},
		{"id": "refund", "type": "echo", "config": {"input": {"arm": "refund"}}},
		{"id": "complaint", "type": "echo", "config": {"input": {"arm": "complaint"}}},
		{"id": "complaint_followup", "type": "noop"},
		{"id": "generic", "type": "echo", "config": {"input": {"arm": "generic"}}},
		{"id": "merge", "type": "join", "config": {"mode": "all"}},
		{"id": "done", "type": "noop"}
	],
	"edges": [
		{"from": "route", "to": "refund", "when": "output.category == 'refund'"},
		{"from": "route", "to": "complaint", "when": "output.category == 'complaint'"},
		{"from": "route", "to": "generic"},
		{"from": "complaint", "to": "complaint_followup"},
		{"from": "refund", "to": "merge"},
		{"from": "complaint_followup", "to": "merge"},
		{"from": "generic", "to": "merge"},
		{"from": "merge", "to": "done"}
	]
}`

// TestConditionalBranchWithSkipPropagation: branch first-match routing —
// the taken arm executes; the untaken arms skip, the skip propagates
// through their descendants, and the mixed fired/skipped fan-in readies
// the `join all` (ADR-003 skip-propagation + join semantics, end to end).
func TestConditionalBranchWithSkipPropagation(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s, h, runID := setup(t, branchDef)
	spawnWorkers(t, s, h)
	drainOutbox(t, s, h)

	run := waitRun(t, s, runID, store.RunStatusSucceeded)
	h.WaitQuiescent(ctx)
	h.RequireHandledOncePerClaim()

	requireStepStatuses(t, s, runID, map[string]string{
		"route":              store.StepStatusSucceeded,
		"refund":             store.StepStatusSucceeded,
		"complaint":          store.StepStatusSkipped,
		"complaint_followup": store.StepStatusSkipped,
		"generic":            store.StepStatusSkipped,
		"merge":              store.StepStatusSucceeded,
		"done":               store.StepStatusSucceeded,
	})
	requireSingleAttempts(t, s, runID, []string{"route", "refund", "merge", "done"})
	requireOutboxEmpty(t, s)
	if run.StepsSucceeded != 4 || run.StepsSkipped != 3 || run.StepsFailed != 0 {
		t.Errorf("run counters = %d/%d/%d (succeeded/failed/skipped), want 4/0/3",
			run.StepsSucceeded, run.StepsFailed, run.StepsSkipped)
	}
	// Skipped steps never execute: no attempt rows, no dispatch.
	for _, stepID := range []string{"complaint", "complaint_followup", "generic"} {
		attempts, err := s.Attempts().ListByStep(ctx, runID, stepID)
		if err != nil {
			t.Fatalf("listing attempts of %q: %v", stepID, err)
		}
		if len(attempts) != 0 {
			t.Errorf("skipped step %q has %d attempt rows, want 0", stepID, len(attempts))
		}
		if got := countStepReadyEvents(t, s, runID, stepID); got != 0 {
			t.Errorf("skipped step %q has %d step_ready events, want 0", stepID, got)
		}
	}
	// Edge resolutions: the taken path fired, everything else skipped.
	wantResolutions := map[int32]string{
		0: store.EdgeResolutionFired,   // route→refund (first match)
		1: store.EdgeResolutionSkipped, // route→complaint
		2: store.EdgeResolutionSkipped, // route→generic (default, passed over)
		3: store.EdgeResolutionSkipped, // complaint→complaint_followup (propagated)
		4: store.EdgeResolutionFired,   // refund→merge
		5: store.EdgeResolutionSkipped, // complaint_followup→merge (propagated)
		6: store.EdgeResolutionSkipped, // generic→merge (propagated)
		7: store.EdgeResolutionFired,   // merge→done
	}
	edges, err := s.Steps().ListEdgesByRun(ctx, runID)
	if err != nil {
		t.Fatalf("listing edges: %v", err)
	}
	for _, e := range edges {
		if want := wantResolutions[e.Ordinal]; e.Resolution != want {
			t.Errorf("edge %d (%s→%s) resolution = %q, want %q", e.Ordinal, e.FromStep, e.ToStep, e.Resolution, want)
		}
	}
}

const joinAnyDef = `{
	"schema_version": 1,
	"name": "join-any",
	"steps": [
		{"id": "fast", "type": "sleep", "config": {"duration": "10ms"}},
		{"id": "slow", "type": "sleep", "config": {"duration": "500ms"}},
		{"id": "race", "type": "join", "config": {"mode": "any"}},
		{"id": "after", "type": "noop"}
	],
	"edges": [
		{"from": "fast", "to": "race"},
		{"from": "slow", "to": "race"},
		{"from": "race", "to": "after"}
	]
}`

// TestJoinAnyAbsorbsLateFiring: a `join any` readies on the first fired
// edge; the slower parent's later firing still resolves its edge but is
// absorbed — exactly one readiness dispatch, no duplicate execution.
func TestJoinAnyAbsorbsLateFiring(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s, h, runID := setup(t, joinAnyDef)
	spawnWorkers(t, s, h)
	drainOutbox(t, s, h)

	waitRun(t, s, runID, store.RunStatusSucceeded)
	h.WaitQuiescent(ctx)
	h.RequireHandledOncePerClaim()

	requireStepStatuses(t, s, runID, map[string]string{
		"fast": store.StepStatusSucceeded, "slow": store.StepStatusSucceeded,
		"race": store.StepStatusSucceeded, "after": store.StepStatusSucceeded,
	})
	requireSingleAttempts(t, s, runID, []string{"fast", "slow", "race", "after"})
	requireOutboxEmpty(t, s)
	if got := countStepReadyEvents(t, s, runID, "race"); got != 1 {
		t.Errorf("step_ready(race) events = %d, want exactly 1 (late firing must be absorbed)", got)
	}
	// Both edges into the join resolved fired even though only the first
	// readied it — the absorption is in dispatch, not bookkeeping.
	edges, err := s.Steps().ListEdgesByRun(ctx, runID)
	if err != nil {
		t.Fatalf("listing edges: %v", err)
	}
	for _, e := range edges {
		if e.Resolution != store.EdgeResolutionFired {
			t.Errorf("edge %d (%s→%s) resolution = %q, want fired", e.Ordinal, e.FromStep, e.ToStep, e.Resolution)
		}
	}
}

const failDef = `{
	"schema_version": 1,
	"name": "executor-failure",
	"steps": [
		{"id": "boom", "type": "fail_n_times", "config": {"n": 99}},
		{"id": "never", "type": "noop"}
	],
	"edges": [
		{"from": "boom", "to": "never"}
	]
}`

// TestExecutorFailureFailsStepAndRun: an executor error lands the failure
// completion — step failed with the error recorded, run failed (the v1
// rollup; ADR-006 refines policy in M5), dependents never dispatched, and
// the delivery ACKed (queue quiescent).
func TestExecutorFailureFailsStepAndRun(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s, h, runID := setup(t, failDef)
	spawnWorkers(t, s, h)
	drainOutbox(t, s, h)

	run := waitRun(t, s, runID, store.RunStatusFailed)
	h.WaitQuiescent(ctx)
	h.RequireHandledOncePerClaim()

	requireStepStatuses(t, s, runID, map[string]string{
		"boom": store.StepStatusFailed, "never": store.StepStatusPending,
	})
	requireOutboxEmpty(t, s)
	if run.StepsFailed != 1 || run.StepsSucceeded != 0 {
		t.Errorf("run counters = %d/%d (succeeded/failed), want 0/1", run.StepsSucceeded, run.StepsFailed)
	}
	boom, err := s.Steps().Get(ctx, runID, "boom")
	if err != nil {
		t.Fatalf("reading step: %v", err)
	}
	if !strings.Contains(string(boom.Error), "deliberate failure") {
		t.Errorf("step error = %s, want the executor's message recorded", boom.Error)
	}
	attempts, err := s.Attempts().ListByStep(ctx, runID, "boom")
	if err != nil {
		t.Fatalf("listing attempts: %v", err)
	}
	if len(attempts) != 1 || attempts[0].Outcome == nil || *attempts[0].Outcome != store.StepStatusFailed {
		t.Errorf("attempts = %+v, want one closed with outcome failed", attempts)
	}
	// The failed step's out-edges stay unresolved (ADR-004): the
	// dependent is permanently blocked, never ready, never skipped.
	if got := countStepReadyEvents(t, s, runID, "never"); got != 0 {
		t.Errorf("step_ready(never) events = %d, want 0", got)
	}
	if got := countEvents(t, s, runID, store.EventRunFailed); got != 1 {
		t.Errorf("run_failed events = %d, want exactly 1", got)
	}
}

// TestCompletionIsSingleTransaction: abort the completion transaction at
// every failpoint stage and assert the database still shows the exact
// pre-completion state — nothing (output, transition, edges, counters,
// events, outbox) leaks from an uncommitted completion. Handle must
// return an error (no ACK → redelivery). Not parallel: the failpoint is
// a package global.
func TestCompletionIsSingleTransaction(t *testing.T) {
	stages := []string{
		engine.StageAfterStepTransition,
		engine.StageAfterFanOut,
		engine.StageAfterOutbox,
	}
	for _, stage := range stages {
		t.Run(stage, func(t *testing.T) {
			ctx := t.Context()
			s := store.NewFromPool(storetest.NewDB(t))
			def := mustDecode(t, linearDef)
			res, err := s.CreateRun(ctx, store.CreateRunArgs{Definition: def, Now: testNow})
			if err != nil {
				t.Fatalf("CreateRun: %v", err)
			}
			runID := res.Run.ID

			boom := errors.New("injected completion failure")
			engine.SetCompleteFailpoint(t, func(got string) error {
				if got == stage {
					return boom
				}
				return nil
			})

			// Drive the delivery by hand — no queue involved; the claim
			// transaction commits, then the completion transaction aborts.
			e := newWorker(t, s, "worker-a")
			err = e.Handle(ctx, queue.Delivery{
				ID:            "0-1",
				Envelope:      stepEnvelope(runID, "a"),
				DeliveryCount: 1,
			})
			if !errors.Is(err, boom) {
				t.Fatalf("Handle error = %v, want the injected failure (no ACK)", err)
			}

			// Pre-completion state, exactly: the claim stuck, nothing else.
			stepA, err := s.Steps().Get(ctx, runID, "a")
			if err != nil {
				t.Fatalf("reading step a: %v", err)
			}
			if stepA.Status != store.StepStatusRunning {
				t.Errorf("step a status = %q, want running (claim committed, completion rolled back)", stepA.Status)
			}
			if stepA.Output != nil {
				t.Errorf("step a output = %s, want none", stepA.Output)
			}
			if stepA.FinishedAt != nil {
				t.Error("step a has finished_at, want none")
			}
			stepB, err := s.Steps().Get(ctx, runID, "b")
			if err != nil {
				t.Fatalf("reading step b: %v", err)
			}
			if stepB.Status != store.StepStatusPending || stepB.RemainingDeps != 1 || stepB.FiredDeps != 0 {
				t.Errorf("step b = %s remaining=%d fired=%d, want untouched pending/1/0",
					stepB.Status, stepB.RemainingDeps, stepB.FiredDeps)
			}
			edges, err := s.Steps().ListEdgesByRun(ctx, runID)
			if err != nil {
				t.Fatalf("listing edges: %v", err)
			}
			for _, edge := range edges {
				if edge.Resolution != store.EdgeResolutionUnresolved {
					t.Errorf("edge %d resolution = %q, want unresolved", edge.Ordinal, edge.Resolution)
				}
			}
			if got := countEvents(t, s, runID, store.EventStepSucceeded); got != 0 {
				t.Errorf("step_succeeded events = %d, want 0", got)
			}
			run, err := s.Runs().Get(ctx, runID)
			if err != nil {
				t.Fatalf("reading run: %v", err)
			}
			if run.Status != store.RunStatusRunning || run.StepsSucceeded != 0 {
				t.Errorf("run = %s with %d succeeded, want running with 0", run.Status, run.StepsSucceeded)
			}
			tasks, err := s.Outbox().List(ctx, 100)
			if err != nil {
				t.Fatalf("listing outbox: %v", err)
			}
			if len(tasks) != 1 || tasks[0].StepID != "a" {
				t.Errorf("outbox = %+v, want only the instantiation row for step a", tasks)
			}
		})
	}
}
