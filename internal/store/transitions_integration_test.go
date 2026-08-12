//go:build integration

package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/mathcslearner/agentloom/internal/dag"
	"github.com/mathcslearner/agentloom/internal/store"
	"github.com/mathcslearner/agentloom/internal/store/gen"
)

// decodeDef decodes an inline definition literal (tight-control graphs the
// canonical fixtures don't cover).
func decodeDef(t *testing.T, doc string) *dag.Definition {
	t.Helper()
	def, err := dag.Decode([]byte(doc))
	if err != nil {
		t.Fatalf("decoding inline definition: %v", err)
	}
	return def
}

// instantiate creates a run of def and returns it.
func instantiate(t *testing.T, s *store.Store, def *dag.Definition) gen.Run {
	t.Helper()
	res, err := s.CreateRun(t.Context(), store.CreateRunArgs{Definition: def, Now: testNow})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	return res.Run
}

// Single-transition helpers: each runs one transition in its own WithTx,
// the way the M4 worker will for claims.

func claimStep(t *testing.T, s *store.Store, runID uuid.UUID, stepID string) (gen.RunStep, error) {
	t.Helper()
	var step gen.RunStep
	err := s.WithTx(t.Context(), func(ctx context.Context, q store.Querier) error {
		var err error
		step, err = store.ClaimStep(ctx, q, store.ClaimStepArgs{RunID: runID, StepID: stepID, Now: testNow})
		return err
	})
	return step, err
}

func succeedStep(t *testing.T, s *store.Store, runID uuid.UUID, stepID string, claim uuid.UUID, output json.RawMessage) (gen.RunStep, error) {
	t.Helper()
	var step gen.RunStep
	err := s.WithTx(t.Context(), func(ctx context.Context, q store.Querier) error {
		var err error
		step, err = store.SucceedStep(ctx, q, store.SucceedStepArgs{
			RunID: runID, StepID: stepID, ClaimID: claim, Output: output, Now: testNow,
		})
		return err
	})
	return step, err
}

// failStep is the terminal failure path (dead-lettering with source
// permanent since ticket 5.4 — the pre-5.4 FailStep is retired).
func failStep(t *testing.T, s *store.Store, runID uuid.UUID, stepID string, claim uuid.UUID, errJSON json.RawMessage) error {
	t.Helper()
	return s.WithTx(t.Context(), func(ctx context.Context, q store.Querier) error {
		_, err := store.DeadLetterStep(ctx, q, store.DeadLetterStepArgs{
			RunID: runID, StepID: stepID, ClaimID: claim,
			Source:  store.DeadLetterSourcePermanent,
			Outcome: store.AttemptOutcomePermanent, Error: errJSON, Now: testNow,
		})
		return err
	})
}

func cancelStep(t *testing.T, s *store.Store, runID uuid.UUID, stepID string) error {
	t.Helper()
	return s.WithTx(t.Context(), func(ctx context.Context, q store.Querier) error {
		_, err := store.CancelStep(ctx, q, store.CancelStepArgs{
			RunID: runID, StepID: stepID,
			Reason: store.CancelReasonUpstreamDeadLettered, Now: testNow,
		})
		return err
	})
}

func resolveEdge(t *testing.T, s *store.Store, runID uuid.UUID, ordinal int32, fired bool) (store.ResolveEdgeResult, error) {
	t.Helper()
	var res store.ResolveEdgeResult
	err := s.WithTx(t.Context(), func(ctx context.Context, q store.Querier) error {
		var err error
		res, err = store.ResolveEdge(ctx, q, store.ResolveEdgeArgs{
			RunID: runID, Ordinal: ordinal, Fired: fired, Now: testNow,
		})
		return err
	})
	return res, err
}

func readyStep(t *testing.T, s *store.Store, runID uuid.UUID, stepID string, joinAny bool) error {
	t.Helper()
	return s.WithTx(t.Context(), func(ctx context.Context, q store.Querier) error {
		_, err := store.ReadyStep(ctx, q, store.ReadyStepArgs{
			RunID: runID, StepID: stepID, JoinAny: joinAny, Now: testNow,
		})
		return err
	})
}

func skipStep(t *testing.T, s *store.Store, runID uuid.UUID, stepID string) (gen.RunStep, error) {
	t.Helper()
	var step gen.RunStep
	err := s.WithTx(t.Context(), func(ctx context.Context, q store.Querier) error {
		var err error
		step, err = store.SkipStep(ctx, q, store.SkipStepArgs{RunID: runID, StepID: stepID, Now: testNow})
		return err
	})
	return step, err
}

func succeedRun(t *testing.T, s *store.Store, runID uuid.UUID) (gen.Run, error) {
	t.Helper()
	var run gen.Run
	err := s.WithTx(t.Context(), func(ctx context.Context, q store.Querier) error {
		var err error
		run, err = store.SucceedRun(ctx, q, store.SucceedRunArgs{RunID: runID, Now: testNow})
		return err
	})
	return run, err
}

func failRun(t *testing.T, s *store.Store, runID uuid.UUID) (gen.Run, error) {
	t.Helper()
	var run gen.Run
	err := s.WithTx(t.Context(), func(ctx context.Context, q store.Querier) error {
		var err error
		run, err = store.FailRun(ctx, q, store.FailRunArgs{RunID: runID, Now: testNow})
		return err
	})
	return run, err
}

// mustClaim / mustSucceed / mustResolve fail the test on error.
func mustClaim(t *testing.T, s *store.Store, runID uuid.UUID, stepID string) gen.RunStep {
	t.Helper()
	step, err := claimStep(t, s, runID, stepID)
	if err != nil {
		t.Fatalf("claiming %q: %v", stepID, err)
	}
	return step
}

func mustSucceed(t *testing.T, s *store.Store, runID uuid.UUID, stepID string, claim uuid.UUID) gen.RunStep {
	t.Helper()
	step, err := succeedStep(t, s, runID, stepID, claim, json.RawMessage(`{"ok":true}`))
	if err != nil {
		t.Fatalf("succeeding %q: %v", stepID, err)
	}
	return step
}

func mustResolve(t *testing.T, s *store.Store, runID uuid.UUID, ordinal int32, fired bool) store.ResolveEdgeResult {
	t.Helper()
	res, err := resolveEdge(t, s, runID, ordinal, fired)
	if err != nil {
		t.Fatalf("resolving edge %d: %v", ordinal, err)
	}
	if !res.Resolved {
		t.Fatalf("edge %d unexpectedly already resolved", ordinal)
	}
	return res
}

// conflictError asserts err is a *TransitionError with the given reason
// and returns it for field assertions.
func conflictError(t *testing.T, err error, reason store.ConflictReason) *store.TransitionError {
	t.Helper()
	if !errors.Is(err, store.ErrConflict) {
		t.Fatalf("error %v does not match ErrConflict", err)
	}
	var te *store.TransitionError
	if !errors.As(err, &te) {
		t.Fatalf("error %v carries no *TransitionError", err)
	}
	if te.Reason != reason {
		t.Fatalf("conflict reason = %q, want %q (error: %v)", te.Reason, reason, err)
	}
	return te
}

// wantConflict is conflictError for call sites that only care about the
// classification.
func wantConflict(t *testing.T, err error, reason store.ConflictReason) {
	t.Helper()
	_ = conflictError(t, err, reason)
}

// eventTypes returns the run's event log types in seq order, asserting the
// sequence is contiguous from 1.
func eventTypes(t *testing.T, s *store.Store, runID uuid.UUID) []string {
	t.Helper()
	evs, err := s.Events().List(t.Context(), runID, 0, 500)
	if err != nil {
		t.Fatalf("listing events: %v", err)
	}
	types := make([]string, len(evs))
	for i, ev := range evs {
		if ev.Seq != int64(i+1) {
			t.Fatalf("event seq gap: event %d has seq %d", i, ev.Seq)
		}
		types[i] = ev.Type
	}
	return types
}

// assertCountersMatchEdges verifies the two ADR-004 bookkeeping
// representations agree: every step's counters equal what its incoming
// normal edges' resolutions imply.
func assertCountersMatchEdges(t *testing.T, s *store.Store, runID uuid.UUID) {
	t.Helper()
	edges, err := s.Steps().ListEdgesByRun(t.Context(), runID)
	if err != nil {
		t.Fatalf("listing edges: %v", err)
	}
	remaining := map[string]int32{}
	fired := map[string]int32{}
	for _, e := range edges {
		if e.EdgeType != store.EdgeTypeNormal {
			continue
		}
		switch e.Resolution {
		case store.EdgeResolutionUnresolved:
			remaining[e.ToStep]++
		case store.EdgeResolutionFired:
			fired[e.ToStep]++
		}
	}
	for id, step := range stepsByID(t, s, runID) {
		if step.RemainingDeps != remaining[id] || step.FiredDeps != fired[id] {
			t.Errorf("step %q counters (remaining %d, fired %d) disagree with edge resolutions (remaining %d, fired %d)",
				id, step.RemainingDeps, step.FiredDeps, remaining[id], fired[id])
		}
	}
}

// TestClaimStepRace is the ticket's headline acceptance criterion: N
// goroutines race to claim one ready step; exactly one wins, the losers
// get typed ErrConflict, and exactly one attempt/event/claim exists.
func TestClaimStepRace(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	run := instantiate(t, s, loadFixture(t, "fanout.json"))

	const racers = 16
	errs := make([]error, racers)
	var wg sync.WaitGroup
	for i := range racers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = claimStep(t, s, run.ID, "start")
		}()
	}
	wg.Wait()

	winners := 0
	for _, err := range errs {
		if err == nil {
			winners++
			continue
		}
		te := conflictError(t, err, store.ConflictWrongStatus)
		if te.From != store.StepStatusRunning {
			t.Errorf("loser saw status %q, want running", te.From)
		}
	}
	if winners != 1 {
		t.Fatalf("winners = %d, want exactly 1", winners)
	}

	step, err := s.Steps().Get(t.Context(), run.ID, "start")
	if err != nil {
		t.Fatalf("reading step: %v", err)
	}
	if step.Status != store.StepStatusRunning || step.ClaimID == nil || step.AttemptCount != 1 {
		t.Errorf("step after race: status %q, claim %v, attempts %d — want running, non-nil, 1",
			step.Status, step.ClaimID, step.AttemptCount)
	}
	if step.StartedAt == nil || !step.StartedAt.Equal(testNow) {
		t.Errorf("started_at = %v, want %v", step.StartedAt, testNow)
	}
	attempts, err := s.Attempts().ListByStep(t.Context(), run.ID, "start")
	if err != nil {
		t.Fatalf("listing attempts: %v", err)
	}
	if len(attempts) != 1 || attempts[0].ClaimID != *step.ClaimID {
		t.Errorf("attempts = %+v, want one row carrying the step's claim", attempts)
	}
	types := eventTypes(t, s, run.ID)
	want := []string{store.EventRunCreated, store.EventStepReady, store.EventStepClaimed}
	if len(types) != len(want) {
		t.Fatalf("event types = %v, want %v (losers must append nothing)", types, want)
	}
}

// TestStepTransitionMatrix drives every step transition against every
// current status: only the matrix-legal from-status succeeds, everything
// else is a typed wrong_status conflict that leaves no trace.
func TestStepTransitionMatrix(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	ctx := t.Context()

	type callFn func(ctx context.Context, q store.Querier, runID uuid.UUID, stepID string, claim uuid.UUID) error
	transitions := []struct {
		name      string
		legalFrom string
		// alsoLegalFrom extends legalFrom for the multi-from transitions
		// (CancelStep since 5.6 accepts pending, ready, and retrying).
		alsoLegalFrom []string
		// counters seeded for pending steps: chosen so the pending cell
		// exercises the transition's guard-satisfied path.
		pendingRemaining, pendingFired int32
		call                           callFn
	}{
		{
			name: "ClaimStep", legalFrom: store.StepStatusReady,
			call: func(ctx context.Context, q store.Querier, runID uuid.UUID, stepID string, _ uuid.UUID) error {
				_, err := store.ClaimStep(ctx, q, store.ClaimStepArgs{RunID: runID, StepID: stepID, Now: testNow})
				return err
			},
		},
		{
			name: "SucceedStep", legalFrom: store.StepStatusRunning,
			call: func(ctx context.Context, q store.Querier, runID uuid.UUID, stepID string, claim uuid.UUID) error {
				_, err := store.SucceedStep(ctx, q, store.SucceedStepArgs{
					RunID: runID, StepID: stepID, ClaimID: claim, Now: testNow,
				})
				return err
			},
		},
		{
			name: "DeadLetterStep", legalFrom: store.StepStatusRunning,
			call: func(ctx context.Context, q store.Querier, runID uuid.UUID, stepID string, claim uuid.UUID) error {
				_, err := store.DeadLetterStep(ctx, q, store.DeadLetterStepArgs{
					RunID: runID, StepID: stepID, ClaimID: claim,
					Source:  store.DeadLetterSourcePermanent,
					Outcome: store.AttemptOutcomePermanent, Now: testNow,
				})
				return err
			},
		},
		{
			name: "CancelStep", legalFrom: store.StepStatusPending,
			// Broadened by 5.6's run-cancel sweep (retrying is not in this
			// matrix's status set; runctl_integration_test.go covers it).
			alsoLegalFrom:    []string{store.StepStatusReady},
			pendingRemaining: 1, pendingFired: 0,
			call: func(ctx context.Context, q store.Querier, runID uuid.UUID, stepID string, _ uuid.UUID) error {
				_, err := store.CancelStep(ctx, q, store.CancelStepArgs{
					RunID: runID, StepID: stepID,
					Reason: store.CancelReasonUpstreamDeadLettered, Now: testNow,
				})
				return err
			},
		},
		{
			name: "RequeueStep", legalFrom: store.StepStatusDeadLettered,
			call: func(ctx context.Context, q store.Querier, runID uuid.UUID, stepID string, _ uuid.UUID) error {
				_, err := store.RequeueStep(ctx, q, store.RequeueStepArgs{
					RunID: runID, StepID: stepID, Now: testNow,
				})
				return err
			},
		},
		{
			name: "ReviveStep", legalFrom: store.StepStatusCancelled,
			call: func(ctx context.Context, q store.Querier, runID uuid.UUID, stepID string, _ uuid.UUID) error {
				_, err := store.ReviveStep(ctx, q, store.ReviveStepArgs{
					RunID: runID, StepID: stepID,
					Reason: store.OutboxReasonDLQRequeue, Now: testNow,
				})
				return err
			},
		},
		{
			name: "TakeoverStep", legalFrom: store.StepStatusRunning,
			call: func(ctx context.Context, q store.Querier, runID uuid.UUID, stepID string, claim uuid.UUID) error {
				_, err := store.TakeoverStep(ctx, q, store.TakeoverStepArgs{
					RunID: runID, StepID: stepID, ClaimID: claim, Now: testNow,
				})
				return err
			},
		},
		{
			name: "ReadyStep", legalFrom: store.StepStatusPending,
			pendingRemaining: 0, pendingFired: 1,
			call: func(ctx context.Context, q store.Querier, runID uuid.UUID, stepID string, _ uuid.UUID) error {
				_, err := store.ReadyStep(ctx, q, store.ReadyStepArgs{RunID: runID, StepID: stepID, Now: testNow})
				return err
			},
		},
		{
			name: "SkipStep", legalFrom: store.StepStatusPending,
			pendingRemaining: 0, pendingFired: 0,
			call: func(ctx context.Context, q store.Querier, runID uuid.UUID, stepID string, _ uuid.UUID) error {
				_, err := store.SkipStep(ctx, q, store.SkipStepArgs{RunID: runID, StepID: stepID, Now: testNow})
				return err
			},
		},
	}
	statuses := []string{
		store.StepStatusPending, store.StepStatusReady, store.StepStatusRunning,
		store.StepStatusSucceeded, store.StepStatusDeadLettered, store.StepStatusSkipped,
		store.StepStatusCancelled,
	}

	for _, tr := range transitions {
		t.Run(tr.name, func(t *testing.T) {
			t.Parallel()
			for _, from := range statuses {
				run := mustCreateRun(t, s, nil)
				stepID := "step_" + from
				// Seed the step at the target status; running and the
				// terminal statuses are reached through real transitions
				// so claim/attempt state is authentic.
				var claim uuid.UUID
				create := func(status string, rem, fired int32) {
					if _, err := s.Steps().Create(ctx, gen.CreateRunStepParams{
						RunID: run.ID, StepID: stepID, StepType: "noop",
						Status: status, RemainingDeps: rem, FiredDeps: fired,
						UpdatedAt: testNow,
					}); err != nil {
						t.Fatalf("seeding %s step: %v", from, err)
					}
				}
				switch from {
				case store.StepStatusPending:
					create(store.StepStatusPending, tr.pendingRemaining, tr.pendingFired)
				case store.StepStatusReady:
					create(store.StepStatusReady, 0, 1)
				case store.StepStatusRunning:
					create(store.StepStatusReady, 0, 1)
					claim = *mustClaim(t, s, run.ID, stepID).ClaimID
				case store.StepStatusSucceeded:
					create(store.StepStatusReady, 0, 1)
					c := *mustClaim(t, s, run.ID, stepID).ClaimID
					mustSucceed(t, s, run.ID, stepID, c)
					claim = c
				case store.StepStatusDeadLettered:
					create(store.StepStatusReady, 0, 1)
					c := *mustClaim(t, s, run.ID, stepID).ClaimID
					if err := failStep(t, s, run.ID, stepID, c, nil); err != nil {
						t.Fatalf("seeding dead-lettered step: %v", err)
					}
					claim = c
				case store.StepStatusSkipped:
					create(store.StepStatusPending, 0, 0)
					if _, err := skipStep(t, s, run.ID, stepID); err != nil {
						t.Fatalf("seeding skipped step: %v", err)
					}
				case store.StepStatusCancelled:
					create(store.StepStatusPending, 1, 0)
					if err := cancelStep(t, s, run.ID, stepID); err != nil {
						t.Fatalf("seeding cancelled step: %v", err)
					}
				}

				eventsBefore := len(eventTypes(t, s, run.ID))
				err := s.WithTx(ctx, func(ctx context.Context, q store.Querier) error {
					return tr.call(ctx, q, run.ID, stepID, claim)
				})

				legal := from == tr.legalFrom
				for _, f := range tr.alsoLegalFrom {
					legal = legal || from == f
				}
				if legal {
					if err != nil {
						t.Errorf("%s from %q: unexpected rejection: %v", tr.name, from, err)
					}
					continue
				}
				te := conflictError(t, err, store.ConflictWrongStatus)
				if te.From != from {
					t.Errorf("%s from %q: TransitionError.From = %q", tr.name, from, te.From)
				}
				after, getErr := s.Steps().Get(ctx, run.ID, stepID)
				if getErr != nil {
					t.Fatalf("re-reading step: %v", getErr)
				}
				if after.Status != from {
					t.Errorf("%s from %q: rejected transition changed status to %q", tr.name, from, after.Status)
				}
				if got := len(eventTypes(t, s, run.ID)); got != eventsBefore {
					t.Errorf("%s from %q: rejected transition appended %d event(s)", tr.name, from, got-eventsBefore)
				}
			}
		})
	}
}

// TestStepGuardsAndFencing covers the non-status guards: counter guards on
// ReadyStep/SkipStep and claim_id fencing on the completion transitions.
func TestStepGuardsAndFencing(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	ctx := t.Context()

	t.Run("ready guard unsatisfied", func(t *testing.T) {
		t.Parallel()
		run := mustCreateRun(t, s, nil)
		for _, tc := range []struct {
			stepID           string
			remaining, fired int32
		}{
			{"unresolved_deps", 1, 1}, // resolved-but-not-all, join all
			{"nothing_fired", 0, 0},   // all resolved, none fired (skip territory)
		} {
			if _, err := s.Steps().Create(ctx, gen.CreateRunStepParams{
				RunID: run.ID, StepID: tc.stepID, StepType: "noop",
				Status: store.StepStatusPending, RemainingDeps: tc.remaining, FiredDeps: tc.fired,
				UpdatedAt: testNow,
			}); err != nil {
				t.Fatalf("seeding: %v", err)
			}
			err := readyStep(t, s, run.ID, tc.stepID, false)
			wantConflict(t, err, store.ConflictGuardFailed)
		}
	})

	t.Run("skip guard unsatisfied", func(t *testing.T) {
		t.Parallel()
		run := mustCreateRun(t, s, nil)
		if _, err := s.Steps().Create(ctx, gen.CreateRunStepParams{
			RunID: run.ID, StepID: "s", StepType: "noop",
			Status: store.StepStatusPending, RemainingDeps: 0, FiredDeps: 1,
			UpdatedAt: testNow,
		}); err != nil {
			t.Fatalf("seeding: %v", err)
		}
		_, err := skipStep(t, s, run.ID, "s")
		wantConflict(t, err, store.ConflictGuardFailed)
	})

	t.Run("claim fencing", func(t *testing.T) {
		t.Parallel()
		run := mustCreateRun(t, s, nil)
		if _, err := s.Steps().Create(ctx, gen.CreateRunStepParams{
			RunID: run.ID, StepID: "s", StepType: "noop", Status: store.StepStatusReady,
			UpdatedAt: testNow,
		}); err != nil {
			t.Fatalf("seeding: %v", err)
		}
		liveClaim := *mustClaim(t, s, run.ID, "s").ClaimID
		stale := uuid.New()

		_, err := succeedStep(t, s, run.ID, "s", stale, nil)
		te := conflictError(t, err, store.ConflictClaimMismatch)
		if te.CallerClaimID == nil || *te.CallerClaimID != stale {
			t.Errorf("CallerClaimID = %v, want %s", te.CallerClaimID, stale)
		}
		if te.CurrentClaimID == nil || *te.CurrentClaimID != liveClaim {
			t.Errorf("CurrentClaimID = %v, want %s", te.CurrentClaimID, liveClaim)
		}
		if err := failStep(t, s, run.ID, "s", stale, nil); err == nil {
			t.Error("DeadLetterStep with a stale claim succeeded")
		}

		// The real claim completes; a duplicate completion (redelivery)
		// is a wrong_status conflict — ACK-and-drop territory.
		mustSucceed(t, s, run.ID, "s", liveClaim)
		_, err = succeedStep(t, s, run.ID, "s", liveClaim, nil)
		wantConflict(t, err, store.ConflictWrongStatus)
	})

	t.Run("missing step and run", func(t *testing.T) {
		t.Parallel()
		run := mustCreateRun(t, s, nil)
		if _, err := claimStep(t, s, run.ID, "ghost"); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("claim of missing step: %v, want ErrNotFound", err)
		}
		if _, err := claimStep(t, s, uuid.New(), "s"); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("claim in missing run: %v, want ErrNotFound", err)
		}
	})
}

// TestTakeoverStep covers the lease-expiry takeover (ticket 4.5): the
// happy path — running → ready with the claim cleared, the holder's
// dangling attempt closed as `lost`, and the step_reclaimed event — plus
// the fenced rejections (stale observed claim, already completed) and the
// full takeover → re-claim → complete cycle the attempt history must
// legibly record.
func TestTakeoverStep(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	ctx := t.Context()

	takeover := func(runID uuid.UUID, stepID string, claim uuid.UUID) (gen.RunStep, error) {
		var step gen.RunStep
		err := s.WithTx(ctx, func(ctx context.Context, q store.Querier) error {
			var err error
			step, err = store.TakeoverStep(ctx, q, store.TakeoverStepArgs{
				RunID: runID, StepID: stepID, ClaimID: claim, Now: testNow,
			})
			return err
		})
		return step, err
	}
	seedReady := func(runID uuid.UUID, stepID string) {
		if _, err := s.Steps().Create(ctx, gen.CreateRunStepParams{
			RunID: runID, StepID: stepID, StepType: "noop",
			Status: store.StepStatusReady, FiredDeps: 1, UpdatedAt: testNow,
		}); err != nil {
			t.Fatalf("seeding step: %v", err)
		}
	}

	t.Run("takeover then reclaim", func(t *testing.T) {
		t.Parallel()
		run := mustCreateRun(t, s, nil)
		seedReady(run.ID, "s")
		claimedA := mustClaim(t, s, run.ID, "s")
		claimA := *claimedA.ClaimID

		step, err := takeover(run.ID, "s", claimA)
		if err != nil {
			t.Fatalf("TakeoverStep: %v", err)
		}
		if step.Status != store.StepStatusReady || step.ClaimID != nil {
			t.Errorf("step after takeover = %s claim=%v, want ready with the claim cleared", step.Status, step.ClaimID)
		}
		if step.AttemptCount != 1 {
			t.Errorf("attempt_count = %d, want 1 (takeover claims nothing)", step.AttemptCount)
		}
		if step.StartedAt == nil || !step.StartedAt.Equal(testNow) {
			t.Errorf("started_at = %v, want the first claim's %v preserved", step.StartedAt, testNow)
		}
		attempts, err := s.Attempts().ListByStep(ctx, run.ID, "s")
		if err != nil {
			t.Fatalf("listing attempts: %v", err)
		}
		if len(attempts) != 1 || attempts[0].Outcome == nil || *attempts[0].Outcome != store.AttemptOutcomeLost {
			t.Fatalf("attempts = %+v, want the holder's attempt closed with outcome lost", attempts)
		}
		if attempts[0].FinishedAt == nil {
			t.Error("lost attempt has no finished_at")
		}
		types := eventTypes(t, s, run.ID)
		want := []string{store.EventStepClaimed, store.EventStepReclaimed}
		if len(types) != len(want) || types[0] != want[0] || types[1] != want[1] {
			t.Errorf("event types = %v, want %v", types, want)
		}

		// The new holder claims normally: fresh claim, attempt 2, the
		// original started_at preserved — the reclaim legible in history.
		claimedB := mustClaim(t, s, run.ID, "s")
		if claimedB.ClaimID == nil || *claimedB.ClaimID == claimA {
			t.Error("re-claim after takeover did not issue a fresh claim_id")
		}
		if claimedB.AttemptCount != 2 {
			t.Errorf("attempt_count after re-claim = %d, want 2", claimedB.AttemptCount)
		}
		mustSucceed(t, s, run.ID, "s", *claimedB.ClaimID)
		attempts, err = s.Attempts().ListByStep(ctx, run.ID, "s")
		if err != nil {
			t.Fatalf("listing attempts: %v", err)
		}
		if len(attempts) != 2 {
			t.Fatalf("attempts = %+v, want 2 rows", attempts)
		}
		if attempts[0].ClaimID != claimA || *attempts[0].Outcome != store.AttemptOutcomeLost {
			t.Errorf("attempt 1 = %+v, want the displaced holder's claim closed lost", attempts[0])
		}
		if attempts[1].ClaimID != *claimedB.ClaimID || attempts[1].Outcome == nil || *attempts[1].Outcome != store.StepStatusSucceeded {
			t.Errorf("attempt 2 = %+v, want the new holder's claim closed succeeded", attempts[1])
		}
	})

	t.Run("stale observed claim is fenced", func(t *testing.T) {
		t.Parallel()
		run := mustCreateRun(t, s, nil)
		seedReady(run.ID, "s")
		liveClaim := *mustClaim(t, s, run.ID, "s").ClaimID
		stale := uuid.New()

		_, err := takeover(run.ID, "s", stale)
		te := conflictError(t, err, store.ConflictClaimMismatch)
		if te.CallerClaimID == nil || *te.CallerClaimID != stale {
			t.Errorf("CallerClaimID = %v, want %s", te.CallerClaimID, stale)
		}
		if te.CurrentClaimID == nil || *te.CurrentClaimID != liveClaim {
			t.Errorf("CurrentClaimID = %v, want %s", te.CurrentClaimID, liveClaim)
		}
		step, err := s.Steps().Get(ctx, run.ID, "s")
		if err != nil {
			t.Fatalf("reading step: %v", err)
		}
		if step.Status != store.StepStatusRunning || step.ClaimID == nil || *step.ClaimID != liveClaim {
			t.Errorf("fenced takeover mutated the step: %+v", step)
		}
	})

	t.Run("completed between observation and takeover", func(t *testing.T) {
		t.Parallel()
		run := mustCreateRun(t, s, nil)
		seedReady(run.ID, "s")
		claim := *mustClaim(t, s, run.ID, "s").ClaimID
		mustSucceed(t, s, run.ID, "s", claim)

		// ADR-005's race rule: the holder's completion landed first; the
		// takeover finds the step terminal and fails typed, reporting the
		// claim still on the row.
		_, err := takeover(run.ID, "s", claim)
		te := conflictError(t, err, store.ConflictWrongStatus)
		if te.From != store.StepStatusSucceeded {
			t.Errorf("TransitionError.From = %q, want succeeded", te.From)
		}
		if te.CurrentClaimID == nil || *te.CurrentClaimID != claim {
			t.Errorf("CurrentClaimID = %v, want %s", te.CurrentClaimID, claim)
		}
	})
}

// TestRunTransitionMatrix drives SucceedRun/FailRun through their guards
// and illegal from-statuses. Every rejection also asserts the run row is
// untouched and no event was appended — the same bar the step matrix holds.
func TestRunTransitionMatrix(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	ctx := t.Context()

	// assertRunUnchanged re-reads the run after a rejected transition and
	// verifies status and event log are exactly as before.
	assertRunUnchanged := func(t *testing.T, runID uuid.UUID, wantStatus string, eventsBefore int) {
		t.Helper()
		got, err := s.Runs().Get(ctx, runID)
		if err != nil {
			t.Fatalf("re-reading run: %v", err)
		}
		if got.Status != wantStatus {
			t.Errorf("rejected transition changed run status to %q, want %q", got.Status, wantStatus)
		}
		if n := len(eventTypes(t, s, runID)); n != eventsBefore {
			t.Errorf("rejected transition appended %d event(s)", n-eventsBefore)
		}
	}

	t.Run("succeed guard", func(t *testing.T) {
		t.Parallel()
		// Outstanding steps → guard_failed.
		run := mustCreateRun(t, s, func(p *gen.CreateRunParams) { p.StepsTotal = 2 })
		eventsBefore := len(eventTypes(t, s, run.ID))
		_, err := succeedRun(t, s, run.ID)
		wantConflict(t, err, store.ConflictGuardFailed)
		assertRunUnchanged(t, run.ID, store.RunStatusRunning, eventsBefore)

		// All steps terminal and none failed → succeeds.
		done := mustCreateRun(t, s, func(p *gen.CreateRunParams) { p.StepsTotal = 1 })
		if _, err := s.Steps().Create(ctx, gen.CreateRunStepParams{
			RunID: done.ID, StepID: "s", StepType: "noop", Status: store.StepStatusReady,
			UpdatedAt: testNow,
		}); err != nil {
			t.Fatalf("seeding: %v", err)
		}
		claim := *mustClaim(t, s, done.ID, "s").ClaimID
		mustSucceed(t, s, done.ID, "s", claim)
		got, err := succeedRun(t, s, done.ID)
		if err != nil {
			t.Fatalf("SucceedRun: %v", err)
		}
		if got.Status != store.RunStatusSucceeded || got.FinishedAt == nil || !got.FinishedAt.Equal(testNow) {
			t.Errorf("run = status %q finished %v, want succeeded at %v", got.Status, got.FinishedAt, testNow)
		}

		// Terminal (succeeded) run rejects both transitions untouched.
		eventsDone := len(eventTypes(t, s, done.ID))
		_, err = succeedRun(t, s, done.ID)
		wantConflict(t, err, store.ConflictWrongStatus)
		assertRunUnchanged(t, done.ID, store.RunStatusSucceeded, eventsDone)
		_, err = failRun(t, s, done.ID)
		wantConflict(t, err, store.ConflictWrongStatus)
		assertRunUnchanged(t, done.ID, store.RunStatusSucceeded, eventsDone)
	})

	t.Run("fail guard", func(t *testing.T) {
		t.Parallel()
		// No failed step → guard_failed.
		run := mustCreateRun(t, s, func(p *gen.CreateRunParams) { p.StepsTotal = 1 })
		eventsBefore := len(eventTypes(t, s, run.ID))
		_, err := failRun(t, s, run.ID)
		wantConflict(t, err, store.ConflictGuardFailed)
		assertRunUnchanged(t, run.ID, store.RunStatusRunning, eventsBefore)

		// A failed step unlocks the transition.
		if _, err := s.Steps().Create(ctx, gen.CreateRunStepParams{
			RunID: run.ID, StepID: "s", StepType: "noop", Status: store.StepStatusReady,
			UpdatedAt: testNow,
		}); err != nil {
			t.Fatalf("seeding: %v", err)
		}
		claim := *mustClaim(t, s, run.ID, "s").ClaimID
		if err := failStep(t, s, run.ID, "s", claim, json.RawMessage(`{"msg":"boom"}`)); err != nil {
			t.Fatalf("FailStep: %v", err)
		}
		got, err := failRun(t, s, run.ID)
		if err != nil {
			t.Fatalf("FailRun: %v", err)
		}
		if got.Status != store.RunStatusFailed || got.StepsFailed != 1 {
			t.Errorf("run = status %q steps_failed %d, want failed/1", got.Status, got.StepsFailed)
		}

		// Terminal (failed) run rejects both transitions untouched.
		eventsFailed := len(eventTypes(t, s, run.ID))
		_, err = succeedRun(t, s, run.ID)
		wantConflict(t, err, store.ConflictWrongStatus)
		assertRunUnchanged(t, run.ID, store.RunStatusFailed, eventsFailed)
		_, err = failRun(t, s, run.ID)
		wantConflict(t, err, store.ConflictWrongStatus)
		assertRunUnchanged(t, run.ID, store.RunStatusFailed, eventsFailed)
	})

	t.Run("missing run", func(t *testing.T) {
		t.Parallel()
		if _, err := succeedRun(t, s, uuid.New()); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("SucceedRun on missing run: %v, want ErrNotFound", err)
		}
	})
}

// TestTransitionAtomicity proves the "events appended atomically with
// their transition" criterion from the outside: a transition inside a
// transaction that subsequently rolls back leaves no status change, no
// attempt, no event, and no consumed sequence number.
func TestTransitionAtomicity(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	ctx := t.Context()
	run := instantiate(t, s, loadFixture(t, "fanout.json"))
	seqBefore := mustCreateRunSeq(t, s, run.ID)

	sentinel := errors.New("abort after transition")
	err := s.WithTx(ctx, func(ctx context.Context, q store.Querier) error {
		if _, err := store.ClaimStep(ctx, q, store.ClaimStepArgs{RunID: run.ID, StepID: "start", Now: testNow}); err != nil {
			t.Errorf("in-tx claim failed: %v", err)
		}
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("WithTx error = %v, want the sentinel", err)
	}

	step, err := s.Steps().Get(ctx, run.ID, "start")
	if err != nil {
		t.Fatalf("reading step: %v", err)
	}
	if step.Status != store.StepStatusReady || step.ClaimID != nil || step.AttemptCount != 0 {
		t.Errorf("rollback leaked into the step: status %q, claim %v, attempts %d",
			step.Status, step.ClaimID, step.AttemptCount)
	}
	if attempts, _ := s.Attempts().ListByStep(ctx, run.ID, "start"); len(attempts) != 0 {
		t.Errorf("rollback left %d attempt rows", len(attempts))
	}
	if got := mustCreateRunSeq(t, s, run.ID); got != seqBefore {
		t.Errorf("next_seq = %d after rollback, want %d", got, seqBefore)
	}
	if types := eventTypes(t, s, run.ID); len(types) != int(seqBefore) {
		t.Errorf("rollback left events: %v", types)
	}
}

// TestDroppedConflictKeepsLogGapFree pins the composed-transaction
// contract the package comment promises: a rejected transition writes
// nothing — not even a consumed next_seq — so a caller may drop the typed
// conflict (the join-any late-firing absorption, the completion-tx
// ACK-and-drop) and still commit a gap-free event log. This is the exact
// shape of M4.3's completion transaction.
func TestDroppedConflictKeepsLogGapFree(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	ctx := t.Context()
	run := instantiate(t, s, loadFixture(t, "fanout.json"))

	claim := *mustClaim(t, s, run.ID, "start").ClaimID
	err := s.WithTx(ctx, func(ctx context.Context, q store.Querier) error {
		if _, err := store.SucceedStep(ctx, q, store.SucceedStepArgs{
			RunID: run.ID, StepID: "start", ClaimID: claim,
			Output: json.RawMessage(`{"ok":true}`), Now: testNow,
		}); err != nil {
			return err
		}
		if _, err := store.ResolveEdge(ctx, q, store.ResolveEdgeArgs{
			RunID: run.ID, Ordinal: 0, Fired: true, Now: testNow,
		}); err != nil {
			return err
		}
		if _, err := store.ReadyStep(ctx, q, store.ReadyStepArgs{
			RunID: run.ID, StepID: "search_docs", Now: testNow,
		}); err != nil {
			return err
		}
		// Three rejected transitions, all deliberately dropped: a repeat
		// promotion (wrong_status — the join-any absorption shape), a
		// premature join-all promotion, and a premature rollup (both
		// guard_failed).
		_, err := store.ReadyStep(ctx, q, store.ReadyStepArgs{
			RunID: run.ID, StepID: "search_docs", Now: testNow,
		})
		wantConflict(t, err, store.ConflictWrongStatus)
		_, err = store.ReadyStep(ctx, q, store.ReadyStepArgs{
			RunID: run.ID, StepID: "gather", Now: testNow,
		})
		wantConflict(t, err, store.ConflictGuardFailed)
		_, err = store.SucceedRun(ctx, q, store.SucceedRunArgs{RunID: run.ID, Now: testNow})
		wantConflict(t, err, store.ConflictGuardFailed)
		return nil
	})
	if err != nil {
		t.Fatalf("composed tx with dropped conflicts: %v", err)
	}

	// run_created, step_ready(start), step_claimed, step_succeeded,
	// step_ready(search_docs) — and nothing from the dropped rejections.
	types := eventTypes(t, s, run.ID) // asserts seq contiguous from 1
	if len(types) != 5 || types[3] != store.EventStepSucceeded || types[4] != store.EventStepReady {
		t.Errorf("event log = %v, want 5 events ending step_succeeded, step_ready", types)
	}
	if seq := mustCreateRunSeq(t, s, run.ID); seq != int64(len(types)) {
		t.Errorf("next_seq = %d but %d events exist — dropped conflicts burned sequence numbers", seq, len(types))
	}
}

// mustCreateRunSeq reads the run's event-sequence high-water mark.
func mustCreateRunSeq(t *testing.T, s *store.Store, runID uuid.UUID) int64 {
	t.Helper()
	run, err := s.Runs().Get(t.Context(), runID)
	if err != nil {
		t.Fatalf("reading run: %v", err)
	}
	return run.NextSeq
}

// TestFanoutLifecycle drives the fanout fixture end-to-end through the
// transition primitives — including one 4.3-shaped composed completion
// transaction — and checks aggregates, events, and counter/edge agreement.
func TestFanoutLifecycle(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	ctx := t.Context()
	run := instantiate(t, s, loadFixture(t, "fanout.json"))

	// Fixture edge ordinals: 0..2 start→{search_docs,fetch_news,brainstorm},
	// 3..5 {search_docs,fetch_news,brainstorm}→gather, 6 gather→synthesize.
	branches := []string{"search_docs", "fetch_news", "brainstorm"}

	// Complete "start" the way M4.3 will: one transaction holding the
	// completion CAS, its out-edge resolutions, and the dependent
	// readiness promotions.
	claim := *mustClaim(t, s, run.ID, "start").ClaimID
	err := s.WithTx(ctx, func(ctx context.Context, q store.Querier) error {
		if _, err := store.SucceedStep(ctx, q, store.SucceedStepArgs{
			RunID: run.ID, StepID: "start", ClaimID: claim,
			Output: json.RawMessage(`{"ok":true}`), Now: testNow,
		}); err != nil {
			return err
		}
		for ord := int32(0); ord <= 2; ord++ {
			res, err := store.ResolveEdge(ctx, q, store.ResolveEdgeArgs{
				RunID: run.ID, Ordinal: ord, Fired: true, Now: testNow,
			})
			if err != nil {
				return err
			}
			if _, err := store.ReadyStep(ctx, q, store.ReadyStepArgs{
				RunID: run.ID, StepID: res.Edge.ToStep, Now: testNow,
			}); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("composed completion of start: %v", err)
	}

	// Premature rollup: three branches still in flight.
	_, err = succeedRun(t, s, run.ID)
	wantConflict(t, err, store.ConflictGuardFailed)

	// Idempotent retry: re-resolving an already-fired edge is a no-op.
	res, err := resolveEdge(t, s, run.ID, 0, true)
	if err != nil {
		t.Fatalf("re-resolving edge 0: %v", err)
	}
	if res.Resolved {
		t.Error("re-resolution reported Resolved = true")
	}
	if got := stepsByID(t, s, run.ID)["search_docs"]; got.RemainingDeps != 0 || got.FiredDeps != 1 {
		t.Errorf("re-resolution moved counters: remaining %d fired %d", got.RemainingDeps, got.FiredDeps)
	}

	// The three branches complete; gather's counters drain to (0, 3).
	for i, id := range branches {
		c := *mustClaim(t, s, run.ID, id).ClaimID
		mustSucceed(t, s, run.ID, id, c)
		res := mustResolve(t, s, run.ID, int32(3+i), true)
		if res.ToStep.StepID != "gather" {
			t.Fatalf("edge %d target = %q, want gather", 3+i, res.ToStep.StepID)
		}
		// gather is join-all: not ready until every parent resolved.
		if i < len(branches)-1 {
			err := readyStep(t, s, run.ID, "gather", false)
			wantConflict(t, err, store.ConflictGuardFailed)
		}
	}
	if err := readyStep(t, s, run.ID, "gather", false); err != nil {
		t.Fatalf("readying gather: %v", err)
	}

	c := *mustClaim(t, s, run.ID, "gather").ClaimID
	mustSucceed(t, s, run.ID, "gather", c)
	mustResolve(t, s, run.ID, 6, true)
	if err := readyStep(t, s, run.ID, "synthesize", false); err != nil {
		t.Fatalf("readying synthesize: %v", err)
	}
	c = *mustClaim(t, s, run.ID, "synthesize").ClaimID
	mustSucceed(t, s, run.ID, "synthesize", c)

	final, err := succeedRun(t, s, run.ID)
	if err != nil {
		t.Fatalf("SucceedRun: %v", err)
	}
	if final.StepsSucceeded != 6 || final.StepsFailed != 0 || final.StepsSkipped != 0 {
		t.Errorf("aggregates = (%d, %d, %d), want (6, 0, 0)",
			final.StepsSucceeded, final.StepsFailed, final.StepsSkipped)
	}

	// Every step: succeeded, output persisted, one closed attempt.
	for id, step := range stepsByID(t, s, run.ID) {
		if step.Status != store.StepStatusSucceeded {
			t.Errorf("step %q status = %q", id, step.Status)
		}
		if !jsonEqual(t, step.Output, json.RawMessage(`{"ok":true}`)) {
			t.Errorf("step %q output = %s", id, step.Output)
		}
		attempts, err := s.Attempts().ListByStep(ctx, run.ID, id)
		if err != nil {
			t.Fatalf("listing attempts of %q: %v", id, err)
		}
		if len(attempts) != 1 || attempts[0].Outcome == nil || *attempts[0].Outcome != store.StepStatusSucceeded ||
			attempts[0].FinishedAt == nil {
			t.Errorf("step %q attempts = %+v, want one closed succeeded attempt", id, attempts)
		}
	}
	assertCountersMatchEdges(t, s, run.ID)

	// The event log tells the whole story, in order, gap-free.
	types := eventTypes(t, s, run.ID)
	counts := map[string]int{}
	for _, typ := range types {
		counts[typ]++
	}
	want := map[string]int{
		store.EventRunCreated: 1, store.EventStepReady: 6,
		store.EventStepClaimed: 6, store.EventStepSucceeded: 6,
		store.EventRunSucceeded: 1,
	}
	for typ, n := range want {
		if counts[typ] != n {
			t.Errorf("event %q count = %d, want %d (log: %v)", typ, counts[typ], n, types)
		}
	}
	if types[len(types)-1] != store.EventRunSucceeded {
		t.Errorf("last event = %q, want run_succeeded", types[len(types)-1])
	}
}

// TestSkipPropagation: a linear a→b→c where a's out-edge resolves skipped
// cascades b and c to skipped, and the all-skipped tail still lets the run
// succeed.
func TestSkipPropagation(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	def := decodeDef(t, `{
		"schema_version": 1, "name": "skipline",
		"steps": [
			{"id": "a", "type": "noop"},
			{"id": "b", "type": "noop"},
			{"id": "c", "type": "noop"}
		],
		"edges": [
			{"from": "a", "to": "b", "when": "output.go == true"},
			{"from": "b", "to": "c"}
		]
	}`)
	run := instantiate(t, s, def)

	claim := *mustClaim(t, s, run.ID, "a").ClaimID
	mustSucceed(t, s, run.ID, "a", claim)

	// The engine evaluated `when` to false: edge 0 resolves skipped.
	res := mustResolve(t, s, run.ID, 0, false)
	if res.ToStep.RemainingDeps != 0 || res.ToStep.FiredDeps != 0 {
		t.Fatalf("b counters = (%d, %d), want (0, 0)", res.ToStep.RemainingDeps, res.ToStep.FiredDeps)
	}
	b, err := skipStep(t, s, run.ID, "b")
	if err != nil {
		t.Fatalf("skipping b: %v", err)
	}
	if b.FinishedAt != nil {
		t.Error("skipped step has finished_at — it never ran")
	}
	// b's own out-edge propagates the skip to c.
	mustResolve(t, s, run.ID, 1, false)
	if _, err := skipStep(t, s, run.ID, "c"); err != nil {
		t.Fatalf("skipping c: %v", err)
	}

	final, err := succeedRun(t, s, run.ID)
	if err != nil {
		t.Fatalf("SucceedRun over skipped tail: %v", err)
	}
	if final.StepsSucceeded != 1 || final.StepsSkipped != 2 {
		t.Errorf("aggregates = (%d succeeded, %d skipped), want (1, 2)", final.StepsSucceeded, final.StepsSkipped)
	}
	assertCountersMatchEdges(t, s, run.ID)
	types := eventTypes(t, s, run.ID)
	skips := 0
	for _, typ := range types {
		if typ == store.EventStepSkipped {
			skips++
		}
	}
	if skips != 2 {
		t.Errorf("step_skipped events = %d, want 2 (log: %v)", skips, types)
	}
}

// TestJoinAnyReadiness: a `join any` step readies on its first fired edge;
// later firings are absorbed as conflicts the caller drops.
func TestJoinAnyReadiness(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	def := decodeDef(t, `{
		"schema_version": 1, "name": "joinany",
		"steps": [
			{"id": "a", "type": "noop"},
			{"id": "b", "type": "noop"},
			{"id": "j", "type": "join", "config": {"mode": "any"}}
		],
		"edges": [
			{"from": "a", "to": "j"},
			{"from": "b", "to": "j"}
		]
	}`)
	run := instantiate(t, s, def)

	claim := *mustClaim(t, s, run.ID, "a").ClaimID
	mustSucceed(t, s, run.ID, "a", claim)
	res := mustResolve(t, s, run.ID, 0, true)
	if res.ToStep.RemainingDeps != 1 || res.ToStep.FiredDeps != 1 {
		t.Fatalf("j counters = (%d, %d), want (1, 1)", res.ToStep.RemainingDeps, res.ToStep.FiredDeps)
	}

	// join-all semantics would still wait; join-any readies now.
	err := readyStep(t, s, run.ID, "j", false)
	wantConflict(t, err, store.ConflictGuardFailed)
	if err := readyStep(t, s, run.ID, "j", true); err != nil {
		t.Fatalf("join-any ReadyStep: %v", err)
	}

	// The second parent fires later; the promotion attempt is absorbed.
	claim = *mustClaim(t, s, run.ID, "b").ClaimID
	mustSucceed(t, s, run.ID, "b", claim)
	mustResolve(t, s, run.ID, 1, true)
	err = readyStep(t, s, run.ID, "j", true)
	wantConflict(t, err, store.ConflictWrongStatus)
	assertCountersMatchEdges(t, s, run.ID)
}

// TestResolveEdgeRejections covers the ResolveEdge error surface.
func TestResolveEdgeRejections(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	ctx := t.Context()

	t.Run("loop edge", func(t *testing.T) {
		t.Parallel()
		run := instantiate(t, s, loadFixture(t, "critic_loop.json"))
		edges, err := s.Steps().ListEdgesByRun(ctx, run.ID)
		if err != nil {
			t.Fatalf("listing edges: %v", err)
		}
		loopOrdinal := int32(-1)
		for _, e := range edges {
			if e.EdgeType == store.EdgeTypeLoop {
				loopOrdinal = e.Ordinal
				break
			}
		}
		if loopOrdinal < 0 {
			t.Fatal("critic_loop fixture has no loop edge")
		}
		_, err = resolveEdge(t, s, run.ID, loopOrdinal, true)
		if err == nil || !strings.Contains(err.Error(), "loop edge") {
			t.Errorf("resolving a loop edge: %v, want loop-edge rejection", err)
		}
	})

	t.Run("conflicting verdict", func(t *testing.T) {
		t.Parallel()
		run := instantiate(t, s, loadFixture(t, "fanout.json"))
		claim := *mustClaim(t, s, run.ID, "start").ClaimID
		mustSucceed(t, s, run.ID, "start", claim)
		mustResolve(t, s, run.ID, 0, true)
		_, err := resolveEdge(t, s, run.ID, 0, false)
		if err == nil || !strings.Contains(err.Error(), "already resolved") {
			t.Errorf("conflicting re-resolution: %v, want already-resolved rejection", err)
		}
	})

	t.Run("missing edge", func(t *testing.T) {
		t.Parallel()
		run := instantiate(t, s, loadFixture(t, "fanout.json"))
		if _, err := resolveEdge(t, s, run.ID, 99, true); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("missing edge: %v, want ErrNotFound", err)
		}
	})

	t.Run("dangling target", func(t *testing.T) {
		t.Parallel()
		// Edge endpoints carry no FK (ADR-004); a dangling target must
		// surface as an integrity error, not silent success.
		run := mustCreateRun(t, s, nil)
		if _, err := s.Steps().CreateEdge(ctx, gen.CreateRunEdgeParams{
			RunID: run.ID, Ordinal: 0, FromStep: "x", ToStep: "ghost",
		}); err != nil {
			t.Fatalf("seeding edge: %v", err)
		}
		_, err := resolveEdge(t, s, run.ID, 0, true)
		if !errors.Is(err, store.ErrNotFound) || !strings.Contains(err.Error(), "ghost") {
			t.Errorf("dangling target: %v, want ErrNotFound naming the step", err)
		}
	})

	t.Run("counter underflow", func(t *testing.T) {
		t.Parallel()
		// An unresolved edge pointing at a step whose remaining_deps is
		// already 0 is corrupted bookkeeping (the two ADR-004
		// representations disagree). It must surface as a typed diagnosis,
		// not a raw CHECK violation.
		run := mustCreateRun(t, s, nil)
		if _, err := s.Steps().Create(ctx, gen.CreateRunStepParams{
			RunID: run.ID, StepID: "drained", StepType: "noop",
			RemainingDeps: 0, UpdatedAt: testNow,
		}); err != nil {
			t.Fatalf("seeding step: %v", err)
		}
		if _, err := s.Steps().CreateEdge(ctx, gen.CreateRunEdgeParams{
			RunID: run.ID, Ordinal: 0, FromStep: "x", ToStep: "drained",
		}); err != nil {
			t.Fatalf("seeding edge: %v", err)
		}
		_, err := resolveEdge(t, s, run.ID, 0, true)
		if err == nil || !strings.Contains(err.Error(), "bookkeeping corrupted") {
			t.Errorf("counter underflow: %v, want the corruption diagnosis", err)
		}
	})
}

// TestTransitionRequiresTx: transitions reject execution outside WithTx or
// with the wrong Querier — atomicity is structural, not conventional.
func TestTransitionRequiresTx(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	run := instantiate(t, s, loadFixture(t, "fanout.json"))
	args := store.ClaimStepArgs{RunID: run.ID, StepID: "start", Now: testNow}

	// Pool-backed Store as the Querier, no transaction.
	if _, err := store.ClaimStep(t.Context(), s, args); !errors.Is(err, store.ErrNoTx) {
		t.Errorf("pool-backed call: %v, want ErrNoTx", err)
	}

	// Inside WithTx but bypassing the callback's Querier.
	err := s.WithTx(t.Context(), func(ctx context.Context, _ store.Querier) error {
		_, err := store.ClaimStep(ctx, s, args)
		return err
	})
	if !errors.Is(err, store.ErrNoTx) {
		t.Errorf("outer-store call inside WithTx: %v, want ErrNoTx", err)
	}

	// Zero Now violates the injected-clock invariant.
	err = s.WithTx(t.Context(), func(ctx context.Context, q store.Querier) error {
		_, err := store.ClaimStep(ctx, q, store.ClaimStepArgs{RunID: run.ID, StepID: "start"})
		return err
	})
	if err == nil || !strings.Contains(err.Error(), "zero Now") {
		t.Errorf("zero Now: %v, want zero-Now rejection", err)
	}

	// Nothing above touched the step.
	step, err := s.Steps().Get(t.Context(), run.ID, "start")
	if err != nil {
		t.Fatalf("reading step: %v", err)
	}
	if step.Status != store.StepStatusReady || step.AttemptCount != 0 {
		t.Errorf("rejected calls mutated the step: %+v", step)
	}
}
