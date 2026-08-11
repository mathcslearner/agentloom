//go:build integration

package engine_test

import (
	"context"
	"encoding/json"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mathcslearner/agentloom/internal/dag"
	"github.com/mathcslearner/agentloom/internal/engine"
	"github.com/mathcslearner/agentloom/internal/exec"
	"github.com/mathcslearner/agentloom/internal/queue"
	"github.com/mathcslearner/agentloom/internal/queue/queuetest"
	"github.com/mathcslearner/agentloom/internal/store"
	"github.com/mathcslearner/agentloom/internal/store/storetest"
)

// testNow is the injected clock for run instantiation and store-side
// transitions. The engine under test keeps its default clock: claim
// timestamps are real time, which nothing here asserts on.
var testNow = time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)

// singleNoop is the minimal fixture: one noop entry step, so a claim race
// has exactly one contested step.
const singleNoop = `{
	"schema_version": 1,
	"name": "claim-race",
	"steps": [{"id": "only", "type": "noop"}],
	"edges": []
}`

// countingNoop is a noop-typed executor that counts Execute calls — the
// side-effect probe proving "exactly one executes".
type countingNoop struct {
	calls atomic.Int64
}

func (*countingNoop) Type() string { return string(dag.StepNoop) }

func (c *countingNoop) Execute(context.Context, exec.StepContext) (exec.Output, error) {
	c.calls.Add(1)
	return exec.Output{}, nil
}

// setup builds one test's isolated world: a migrated Postgres database, an
// isolated queue harness, and an instantiated run of the given definition.
func setup(t *testing.T, defJSON string) (*store.Store, *queuetest.Harness, uuid.UUID) {
	t.Helper()
	s := store.NewFromPool(storetest.NewDB(t))
	h := queuetest.New(t)
	def, err := dag.Decode([]byte(defJSON))
	if err != nil {
		t.Fatalf("decoding definition: %v", err)
	}
	res, err := s.CreateRun(t.Context(), store.CreateRunArgs{Definition: def, Now: testNow})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	// Create the group up front so quiescence probes never race the
	// spawned consumer's own EnsureGroup.
	h.EnsureGroup(t.Context())
	return s, h, res.Run.ID
}

// newEngine builds an engine whose registry holds only the counting noop.
func newEngine(t *testing.T, s *store.Store, c *countingNoop, workerID string) *engine.Engine {
	t.Helper()
	reg, err := exec.NewRegistry(c)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	e, err := engine.New(s, reg, workerID)
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	return e
}

// stepEnvelope points at a step of the given run, the way the outbox
// drainer (4.4) will.
func stepEnvelope(runID uuid.UUID, stepID string) queue.Envelope {
	return queue.Envelope{RunID: runID, StepID: stepID, Reason: queue.ReasonStepReady}
}

// jsonEqual compares two JSON documents semantically — Postgres jsonb
// does not preserve the input's whitespace.
func jsonEqual(t *testing.T, a, b json.RawMessage) bool {
	t.Helper()
	var av, bv any
	if err := json.Unmarshal(a, &av); err != nil {
		t.Fatalf("unmarshaling %s: %v", a, err)
	}
	if err := json.Unmarshal(b, &bv); err != nil {
		t.Fatalf("unmarshaling %s: %v", b, err)
	}
	return reflect.DeepEqual(av, bv)
}

// countEvents returns how many events of the given type the run has.
func countEvents(t *testing.T, s *store.Store, runID uuid.UUID, eventType string) int {
	t.Helper()
	events, err := s.Events().List(t.Context(), runID, 0, 1000)
	if err != nil {
		t.Fatalf("listing events: %v", err)
	}
	n := 0
	for _, ev := range events {
		if ev.Type == eventType {
			n++
		}
	}
	return n
}

// TestClaimRaceExactlyOneExecutes is 4.2's headline acceptance: many
// duplicate deliveries of one ready step, consumed by two workers, must
// produce exactly one claim, one attempt row, and one executor invocation
// — every duplicate ACK-and-dropped by the claim CAS.
func TestClaimRaceExactlyOneExecutes(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s, h, runID := setup(t, singleNoop)

	// Enqueue the duplicates before any consumer starts, so both workers
	// wake up to a backlog and race entry by entry (Batch: 1 spreads the
	// entries across them instead of letting one drain the lot).
	const duplicates = 8
	for range duplicates {
		h.Enqueue(ctx, stepEnvelope(runID, "only"))
	}

	counting := &countingNoop{}
	cfg := queue.ConsumerConfig{Block: 500 * time.Millisecond, Batch: 1}
	h.Spawn("worker-a", newEngine(t, s, counting, "worker-a").Handle, cfg)
	h.Spawn("worker-b", newEngine(t, s, counting, "worker-b").Handle, cfg)

	// Quiescence = every duplicate delivered and acked (the winner after
	// executing, the losers as ACK-and-drop).
	h.WaitQuiescent(ctx)
	h.RequireHandledOncePerClaim()

	if got := counting.calls.Load(); got != 1 {
		t.Errorf("executor ran %d times, want exactly 1", got)
	}
	step, err := s.Steps().Get(ctx, runID, "only")
	if err != nil {
		t.Fatalf("reading step: %v", err)
	}
	if step.Status != store.StepStatusSucceeded {
		t.Errorf("step status = %q, want %q (the winner's completion transaction, ticket 4.3)", step.Status, store.StepStatusSucceeded)
	}
	if step.ClaimID == nil {
		t.Error("step has no claim_id after a won claim")
	}
	if step.AttemptCount != 1 {
		t.Errorf("attempt_count = %d, want 1", step.AttemptCount)
	}
	attempts, err := s.Attempts().ListByStep(ctx, runID, "only")
	if err != nil {
		t.Fatalf("listing attempts: %v", err)
	}
	if len(attempts) != 1 {
		t.Errorf("attempt rows = %d, want exactly 1", len(attempts))
	}
	if got := countEvents(t, s, runID, store.EventStepClaimed); got != 1 {
		t.Errorf("step_claimed events = %d, want exactly 1", got)
	}
	// The single-step run rolled up in the winner's completion transaction.
	run, err := s.Runs().Get(ctx, runID)
	if err != nil {
		t.Fatalf("reading run: %v", err)
	}
	if run.Status != store.RunStatusSucceeded {
		t.Errorf("run status = %q, want %q", run.Status, store.RunStatusSucceeded)
	}
}

// TestDuplicateOfCompletedStepDropped is crash cell W4's consumer-side
// half: a delivery pointing at an already-succeeded step is ACKed and
// dropped with zero side effects — no executor run, no attempt row, no
// event, output untouched.
func TestDuplicateOfCompletedStepDropped(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s, h, runID := setup(t, singleNoop)

	// Complete the step out-of-band, the way a prior worker's completion
	// transaction (4.3) would have.
	wantOutput := json.RawMessage(`{"done":true}`)
	err := s.WithTx(ctx, func(ctx context.Context, q store.Querier) error {
		step, err := store.ClaimStep(ctx, q, store.ClaimStepArgs{RunID: runID, StepID: "only", Now: testNow})
		if err != nil {
			return err
		}
		_, err = store.SucceedStep(ctx, q, store.SucceedStepArgs{
			RunID: runID, StepID: "only", ClaimID: *step.ClaimID, Output: wantOutput, Now: testNow,
		})
		return err
	})
	if err != nil {
		t.Fatalf("completing step out-of-band: %v", err)
	}

	counting := &countingNoop{}
	h.Spawn("worker-a", newEngine(t, s, counting, "worker-a").Handle, queuetest.FastConfig())
	h.Enqueue(ctx, stepEnvelope(runID, "only"))
	h.WaitQuiescent(ctx)

	if got := counting.calls.Load(); got != 0 {
		t.Errorf("executor ran %d times on a duplicate of finished work, want 0", got)
	}
	step, err := s.Steps().Get(ctx, runID, "only")
	if err != nil {
		t.Fatalf("reading step: %v", err)
	}
	if step.Status != store.StepStatusSucceeded {
		t.Errorf("step status = %q, want %q", step.Status, store.StepStatusSucceeded)
	}
	if !jsonEqual(t, step.Output, wantOutput) {
		t.Errorf("step output = %s, want %s (must be untouched)", step.Output, wantOutput)
	}
	if step.AttemptCount != 1 {
		t.Errorf("attempt_count = %d, want 1 (duplicate must not re-claim)", step.AttemptCount)
	}
	attempts, err := s.Attempts().ListByStep(ctx, runID, "only")
	if err != nil {
		t.Fatalf("listing attempts: %v", err)
	}
	if len(attempts) != 1 {
		t.Errorf("attempt rows = %d, want 1", len(attempts))
	}
	if got := countEvents(t, s, runID, store.EventStepClaimed); got != 1 {
		t.Errorf("step_claimed events = %d, want 1", got)
	}
}

// TestDanglingReferenceDropped: a delivery pointing at a run that does not
// exist is ACKed and dropped, and the consumer loop stays healthy.
func TestDanglingReferenceDropped(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s := store.NewFromPool(storetest.NewDB(t))
	h := queuetest.New(t)
	h.EnsureGroup(ctx)

	counting := &countingNoop{}
	h.Spawn("worker-a", newEngine(t, s, counting, "worker-a").Handle, queuetest.FastConfig())
	h.Enqueue(ctx, stepEnvelope(uuid.New(), "ghost"))
	h.WaitQuiescent(ctx)

	if got := counting.calls.Load(); got != 0 {
		t.Errorf("executor ran %d times on a dangling reference, want 0", got)
	}
}
