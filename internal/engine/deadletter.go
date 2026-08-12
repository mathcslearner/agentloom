package engine

// Dead-letter handling (ticket 5.4, ADR-006): the run disposition applied
// when a step reaches the terminal failure state, the poison-message
// handler that turns the queue's delivery-count threshold into a durable
// DLQ record, and the internal requeue op (API exposure lands in M6.5).
//
// The write-off walk here is the eager alternative ADR-006 chose over
// lazy "run ends when nothing can move" evaluation: the transaction that
// dead-letters a step also transitions every step whose readiness is now
// impossible to `cancelled`, so the run rollup stays a counter check and
// the UI gets an honest per-step answer immediately.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/mathcslearner/agentloom/internal/dag"
	"github.com/mathcslearner/agentloom/internal/obs/log"
	"github.com/mathcslearner/agentloom/internal/queue"
	"github.com/mathcslearner/agentloom/internal/store"
	"github.com/mathcslearner/agentloom/internal/store/gen"
)

// planWriteOff computes the fixed point of "pending steps whose readiness
// is now impossible" over the run's current graph state — the write-off
// set of ADR-006's continue_independent_branches. Pure; steps and edges
// are the run's rows as read inside the dead-lettering transaction (the
// dead step already shows dead_lettered).
//
// A source in {dead_lettered, cancelled, failed} never resolves its
// out-edges (`failed` covers pre-5.4 legacy rows). A pending target is
// impossible when:
//   - non-join-any: any unresolved incoming normal edge has a blocked
//     source — remaining_deps can then never drain to zero, so the step
//     can neither ready nor skip;
//   - join-any: fired_deps is zero AND every unresolved incoming normal
//     edge has a blocked source — a join-any readies on its first fired
//     edge, so it survives as long as any live parent might still fire.
//
// Newly-impossible steps count as blocked sources for further rounds
// (their own out-edges will never resolve either). Returns the step ids
// in discovery order (rounds, slice order within a round) — deterministic
// given the store's deterministic ListByRun order. The only error is a
// join config that no longer decodes (corrupt stored state), which the
// caller surfaces rather than guessing survival semantics.
func planWriteOff(steps []gen.RunStep, edges []gen.RunEdge) ([]string, error) {
	blocked := make(map[string]bool)
	for _, st := range steps {
		switch st.Status {
		case store.StepStatusDeadLettered, store.StepStatusCancelled, store.StepStatusFailed:
			blocked[st.StepID] = true
		}
	}
	// Unresolved incoming normal edges per target.
	unresolvedIn := make(map[string][]gen.RunEdge)
	for _, e := range edges {
		if e.EdgeType != store.EdgeTypeNormal || e.Resolution != store.EdgeResolutionUnresolved {
			continue
		}
		unresolvedIn[e.ToStep] = append(unresolvedIn[e.ToStep], e)
	}

	var writeOff []string
	marked := make(map[string]bool)
	for changed := true; changed; {
		changed = false
		for _, st := range steps {
			if st.Status != store.StepStatusPending || marked[st.StepID] {
				continue
			}
			inc := unresolvedIn[st.StepID]
			blockedIn, liveIn := 0, 0
			for _, e := range inc {
				if blocked[e.FromStep] || marked[e.FromStep] {
					blockedIn++
				} else {
					liveIn++
				}
			}
			joinAny, err := isJoinAny(st)
			if err != nil {
				return nil, err
			}
			var impossible bool
			if joinAny {
				impossible = st.FiredDeps == 0 && len(inc) > 0 && liveIn == 0
			} else {
				impossible = blockedIn > 0
			}
			if impossible {
				marked[st.StepID] = true
				writeOff = append(writeOff, st.StepID)
				changed = true
			}
		}
	}
	return writeOff, nil
}

// deadLetterDisposition applies the workflow failure policy inside the
// transaction that just dead-lettered a step (ADR-006 "Run disposition"):
// fail_fast fails the run immediately; continue_independent_branches
// writes off the newly-impossible descendants and attempts the
// all-terminal rollup. Rollup/rerollup conflicts are dropped — the guards
// pass exactly when they should. Returns which steps were cancelled and
// whether the run terminalized.
func deadLetterDisposition(ctx context.Context, q store.Querier, onFailure string, runID uuid.UUID, now time.Time) (cancelled []string, runFailed bool, err error) {
	if onFailure != string(dag.ContinueIndependentBranches) {
		// fail_fast — the default, and the honest reading of any
		// unrecognized materialized value (corrupt stored state fails the
		// run rather than silently continuing).
		_, err := store.FailRun(ctx, q, store.FailRunArgs{RunID: runID, Now: now})
		if err == nil {
			return nil, true, nil
		}
		// Wrong status (a parallel branch already failed the run) is the
		// only conflict the guard can produce here; drop it.
		var te *store.TransitionError
		if errors.As(err, &te) {
			return nil, false, nil
		}
		return nil, false, err
	}

	steps, err := q.Steps().ListByRun(ctx, runID)
	if err != nil {
		return nil, false, err
	}
	edges, err := q.Steps().ListEdgesByRun(ctx, runID)
	if err != nil {
		return nil, false, err
	}
	writeOff, err := planWriteOff(steps, edges)
	if err != nil {
		return nil, false, err
	}
	for _, stepID := range writeOff {
		if _, err := store.CancelStep(ctx, q, store.CancelStepArgs{
			RunID: runID, StepID: stepID,
			Reason: store.CancelReasonUpstreamDeadLettered, Now: now,
		}); err != nil {
			return nil, false, err
		}
	}
	// The dead-letter (with its write-off) may have terminalized the last
	// live step; attempt the all-terminal failure rollup and drop the
	// conflict otherwise — sibling completions attempt it again.
	_, err = store.FailRunRollup(ctx, q, store.FailRunArgs{RunID: runID, Now: now})
	if err == nil {
		return writeOff, true, nil
	}
	var te *store.TransitionError
	if errors.As(err, &te) {
		return writeOff, false, nil
	}
	return nil, false, err
}

// HandlePoison is the queue's PoisonHandler (3.4's callback, wired by
// cmd/worker since ticket 5.4): an entry whose delivery count crossed the
// threshold is consumed by dead-lettering its step — the one ACK that
// happens on a failure path, because the DLQ row is the durable
// consumption of the message (ADR-006). A nil return acks the entry; an
// error leaves it pending for the next reclaim pass (transport failures
// are retryable by design).
func (e *Engine) HandlePoison(ctx context.Context, p queue.PoisonMessage) error {
	ctx = log.With(ctx, log.WorkerID(e.workerID))
	logger := log.From(ctx)
	if p.Envelope == nil {
		// An undecodable envelope has no step identity to key a DLQ row to
		// (the dead_letters table is keyed to a real step). Preserve the
		// raw contents in the log — loudly — and consume the entry: the
		// alternative is the pre-5.4 eternal pending spin this handler
		// exists to end.
		logger.ErrorContext(ctx, "poison message with undecodable envelope; consuming without a DLQ record",
			slog.String("entry_id", p.ID),
			slog.Int64("delivery_count", p.DeliveryCount),
			slog.Any("raw_values", p.Values))
		return nil
	}

	// The failure policy is immutable for the life of the run, so the
	// pre-transaction read is safe (the completeSuccess params
	// convention).
	run, err := e.store.Runs().Get(ctx, p.Envelope.RunID)
	if errors.Is(err, store.ErrNotFound) {
		logger.WarnContext(ctx, "poison message for a missing run — dangling reference, consuming",
			slog.String("entry_id", p.ID))
		return nil
	}
	if err != nil {
		return err
	}

	payload, merr := json.Marshal(p.Values)
	if merr != nil {
		payload = nil // raw values that cannot re-encode still leave the summary
	}
	summary, merr := json.Marshal(map[string]any{
		"message":        fmt.Sprintf("poison message: %d deliveries exceeded the threshold", p.DeliveryCount),
		"source":         store.DeadLetterSourcePoison,
		"entry_id":       p.ID,
		"delivery_count": p.DeliveryCount,
	})
	if merr != nil {
		summary = json.RawMessage(`{"message": "poison message", "source": "poison"}`)
	}

	now := e.now()
	var cancelled []string
	runFailed := false
	txErr := e.store.WithTx(ctx, func(ctx context.Context, q store.Querier) error {
		if _, err := store.PoisonDeadLetterStep(ctx, q, store.PoisonDeadLetterStepArgs{
			RunID: p.Envelope.RunID, StepID: p.Envelope.StepID,
			Error: summary, Payload: payload, Now: now,
		}); err != nil {
			return err
		}
		var derr error
		cancelled, runFailed, derr = deadLetterDisposition(ctx, q, run.OnFailure, p.Envelope.RunID, now)
		if derr != nil {
			return derr
		}
		if !runFailed {
			// A poison step of a *cancelling* run (its handlers kept dying
			// while the run quiesced): both dispositions conflict-drop on
			// the run status, and the DLQ row may have settled the last
			// in-flight step — attempt the cancel finalization (ticket 5.6).
			if _, cerr := attemptCancelRollup(ctx, q, p.Envelope.RunID, now); cerr != nil {
				return cerr
			}
		}
		return nil
	})
	if txErr != nil {
		var te *store.TransitionError
		if errors.As(txErr, &te) {
			// The step is already terminal — the poison entry is a stale
			// duplicate of decided work; consuming it is provably safe.
			logger.WarnContext(ctx, "poison message for an already-terminal step — consuming",
				slog.String("entry_id", p.ID),
				slog.String("step_status", te.From))
			return nil
		}
		if errors.Is(txErr, store.ErrNotFound) {
			logger.WarnContext(ctx, "poison message for a missing step — dangling reference, consuming",
				slog.String("entry_id", p.ID))
			return nil
		}
		logger.ErrorContext(ctx, "poison dead-letter transaction failed; entry stays pending",
			slog.Any("error", txErr))
		return txErr
	}
	logger.ErrorContext(ctx, "poison message dead-lettered",
		slog.String("entry_id", p.ID),
		slog.Int64("delivery_count", p.DeliveryCount),
		slog.String("on_failure", run.OnFailure),
		slog.Int("steps_cancelled", len(cancelled)),
		slog.Bool("run_failed", runFailed))
	return nil
}

// RequeueResult is what Requeue did.
type RequeueResult struct {
	// Step is the requeued step, back in ready.
	Step gen.RunStep
	// RunResumed reports the run was failed and re-opened (failed →
	// running).
	RunResumed bool
	// Revived lists the written-off (cancelled) steps whose readiness the
	// requeue made possible again — back to pending.
	Revived []string
	// Dispatched lists the ready steps re-outboxed with reason
	// dlq_requeue: the requeued step plus any fail_fast siblings whose
	// deliveries were consumed while the run was failed.
	Dispatched []string
}

// Requeue is the internal DLQ requeue op (ticket 5.4; M6.5 exposes it via
// the API): one transaction that resets the dead-lettered step to ready
// (its full retry policy re-armed — the budget counts from the
// dead_letters baseline), re-opens the run if the dead-letter had failed
// it, revives written-off descendants that are no longer impossible, and
// writes dlq_requeue outbox rows for every ready step with no pending
// dispatch. A typed *store.TransitionError surfaces when the step is not
// dead_lettered. Post-commit, the dispatcher nudge fires.
func (e *Engine) Requeue(ctx context.Context, runID uuid.UUID, stepID string) (RequeueResult, error) {
	ctx = log.With(ctx, log.RunID(runID.String()), log.StepID(stepID))
	now := e.now()
	var res RequeueResult
	txErr := e.store.WithTx(ctx, func(ctx context.Context, q store.Querier) error {
		step, err := store.RequeueStep(ctx, q, store.RequeueStepArgs{
			RunID: runID, StepID: stepID, Now: now,
		})
		if err != nil {
			return err
		}
		res.Step = step

		// RequeueStep holds the run lock, so this read is the current row.
		run, err := q.Runs().Get(ctx, runID)
		if err != nil {
			return err
		}
		if run.Status == store.RunStatusCancelling || run.Status == store.RunStatusCancelled {
			// A cancelled run is terminal by operator intent (ticket 5.6):
			// requeueing a dead-lettered step of one would strand it —
			// the claim path refuses the run — or silently undo the cancel.
			return fmt.Errorf("engine: Requeue: run %s is %s — cancelled runs are not requeueable", runID, run.Status)
		}
		if run.Status == store.RunStatusFailed {
			if _, err := store.ResumeRun(ctx, q, store.ResumeRunArgs{RunID: runID, Now: now}); err != nil {
				return err
			}
			res.RunResumed = true
		}

		// Revival recompute: with the requeued step live again, re-derive
		// the impossible set from the remaining dead-lettered steps,
		// treating currently-cancelled steps as pending. Cancelled steps
		// outside the recomputed set are no longer blocked — revive them.
		steps, err := q.Steps().ListByRun(ctx, runID)
		if err != nil {
			return err
		}
		var anyCancelled bool
		asIfPending := make([]gen.RunStep, len(steps))
		for i, st := range steps {
			if st.Status == store.StepStatusCancelled {
				st.Status = store.StepStatusPending
				anyCancelled = true
			}
			asIfPending[i] = st
		}
		if anyCancelled {
			edges, err := q.Steps().ListEdgesByRun(ctx, runID)
			if err != nil {
				return err
			}
			stillImpossible, err := planWriteOff(asIfPending, edges)
			if err != nil {
				return err
			}
			impossible := make(map[string]bool, len(stillImpossible))
			for _, id := range stillImpossible {
				impossible[id] = true
			}
			for _, st := range steps {
				if st.Status != store.StepStatusCancelled || impossible[st.StepID] {
					continue
				}
				if _, err := store.ReviveStep(ctx, q, store.ReviveStepArgs{
					RunID: runID, StepID: st.StepID,
					Reason: store.OutboxReasonDLQRequeue, Now: now,
				}); err != nil {
					return err
				}
				res.Revived = append(res.Revived, st.StepID)
			}
		}

		// Re-dispatch: the requeued step has no outbox row by definition;
		// fail_fast siblings stranded in ready lost their deliveries to
		// the run-status guard while the run was failed.
		toDispatch, err := q.Steps().ListReadyWithoutOutbox(ctx, runID)
		if err != nil {
			return err
		}
		for _, id := range toDispatch {
			if _, err := q.Outbox().Create(ctx, runID, id, store.OutboxReasonDLQRequeue); err != nil {
				return err
			}
		}
		res.Dispatched = toDispatch
		return nil
	})
	if txErr != nil {
		return RequeueResult{}, txErr
	}
	log.From(ctx).InfoContext(ctx, "dead-lettered step requeued",
		slog.Bool("run_resumed", res.RunResumed),
		slog.Int("steps_revived", len(res.Revived)),
		slog.Int("steps_dispatched", len(res.Dispatched)))
	if len(res.Dispatched) > 0 && e.nudge != nil {
		e.nudge()
	}
	return res, nil
}
