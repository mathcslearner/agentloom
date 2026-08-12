package engine

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/google/uuid"

	"github.com/mathcslearner/agentloom/internal/dag"
	"github.com/mathcslearner/agentloom/internal/exec"
	"github.com/mathcslearner/agentloom/internal/exec/effects"
	"github.com/mathcslearner/agentloom/internal/obs/log"
	"github.com/mathcslearner/agentloom/internal/queue"
	"github.com/mathcslearner/agentloom/internal/store"
	"github.com/mathcslearner/agentloom/internal/store/gen"
)

// Handle is the worker's queue.Handler: attempt the claim CAS for the
// delivered step and act per ADR-005's ACK discipline. A nil return acks
// the entry; an error leaves it in the PEL to redeliver via reclaim.
//
// On a won claim the executor runs and its result feeds the completion
// pipeline (complete.go): one transaction persisting the outcome, fencing
// the terminal CAS on claim_id, resolving out-edges, and fanning readiness
// out to successors. The ACK happens only after that transaction commits —
// ADR-005's "completion/failure transition committed → ACK" row — so a
// crash anywhere before commit redelivers, and the redelivery bounces off
// the claim CAS as a duplicate of finished work when the commit did land.
//
// A reclaimed delivery of a running step — the holder went silent past the
// lease TTL — takes the third path: the lease-expiry takeover (ticket 4.5).
// The takeover CAS (running → ready, clearing the holder's claim_id — the
// moment the zombie loses its fence) and a fresh claim run in one
// transaction, then the step executes normally.
func (e *Engine) Handle(ctx context.Context, d queue.Delivery) error {
	// The consumer already stamped entry_id, delivery_count, run_id,
	// step_id, and reason into the log context.
	ctx = log.With(ctx, log.WorkerID(e.workerID))

	now := e.now()
	var step gen.RunStep
	err := e.store.WithTx(ctx, func(ctx context.Context, q store.Querier) error {
		var err error
		step, err = store.ClaimStep(ctx, q, store.ClaimStepArgs{
			RunID:  d.Envelope.RunID,
			StepID: d.Envelope.StepID,
			Now:    now,
		})
		return err
	})
	if err != nil {
		dec := classifyClaimFailure(err, d.DeliveryCount)
		log.From(ctx).LogAttrs(ctx, dec.level, "claim rejected: "+dec.reason,
			slog.String("action", dec.action.String()),
			slog.Any("error", err))
		switch dec.action {
		case claimAckDrop:
			return nil
		case claimTakeover:
			return e.takeoverAndClaim(ctx, d, *dec.holderClaim)
		default:
			return err
		}
	}

	return e.execute(ctx, step)
}

// takeoverAndClaim is the lease-expiry takeover (ADR-005): one transaction
// composing TakeoverStep (running → ready, fenced on the observed holder's
// claim, closing its attempt as lost) with a fresh ClaimStep, then normal
// execution. One transaction removes the intermediate ready-with-no-entry
// state; both primitives take the run lock first, so the pair is race-free.
// If the reconciler's takeover won in between (step already ready), the
// takeover conflict is swallowed — a rejected transition writes nothing —
// and the claim proceeds on the entry already in hand.
func (e *Engine) takeoverAndClaim(ctx context.Context, d queue.Delivery, holderClaim uuid.UUID) error {
	now := e.now()
	var step gen.RunStep
	err := e.store.WithTx(ctx, func(ctx context.Context, q store.Querier) error {
		_, terr := store.TakeoverStep(ctx, q, store.TakeoverStepArgs{
			RunID: d.Envelope.RunID, StepID: d.Envelope.StepID,
			ClaimID: holderClaim, Now: now,
		})
		if terr != nil {
			var te *store.TransitionError
			already := errors.As(terr, &te) &&
				te.Reason == store.ConflictWrongStatus && te.From == store.StepStatusReady
			if !already {
				return terr
			}
			// Another takeover (the reconciler's) landed since this entry
			// was reclaimed; claim the ready step directly.
		}
		var cerr error
		step, cerr = store.ClaimStep(ctx, q, store.ClaimStepArgs{
			RunID: d.Envelope.RunID, StepID: d.Envelope.StepID, Now: now,
		})
		return cerr
	})
	if err != nil {
		dec := classifyTakeoverFailure(err)
		log.From(ctx).LogAttrs(ctx, dec.level, "takeover rejected: "+dec.reason,
			slog.String("action", dec.action.String()),
			slog.String("holder_claim_id", holderClaim.String()),
			slog.Any("error", err))
		if dec.action == claimAckDrop {
			return nil
		}
		return err
	}

	log.From(ctx).InfoContext(ctx, "lease-expiry takeover: step reclaimed from silent holder",
		slog.String("displaced_claim_id", holderClaim.String()),
		slog.String("claim_id", claimIDString(step)))
	return e.execute(ctx, step)
}

// execute runs the claimed step's executor and settles the result through
// the completion pipeline (complete.go). The returned error follows the
// Handle/ACK contract: nil means a completion transaction committed (ACK);
// non-nil means nothing was decided and the entry must redeliver.
func (e *Engine) execute(ctx context.Context, step gen.RunStep) error {
	ctx = log.With(ctx, log.Attempt(int(step.AttemptCount)))
	logger := log.From(ctx)
	logger.InfoContext(ctx, "step claimed",
		slog.String("step_type", step.StepType),
		slog.String("claim_id", claimIDString(step)))

	executor, err := e.registry.Get(step.StepType)
	if err != nil {
		// Step types are validated against the catalog at submit time, so
		// a miss here means worker/definition version skew or corrupt
		// state — deterministic and permanent (ADR-006 taxonomy row 4), so
		// a recorded step failure, never a redelivery loop.
		logger.ErrorContext(ctx, "no executor registered for step type; recording step failure",
			slog.String("step_type", step.StepType),
			slog.Any("error", err))
		return e.completeFailure(ctx, step, err, dag.ClassPermanent)
	}

	timeout, terr := stepTimeout(step.Timeout)
	if terr != nil {
		// Corrupt materialized timeout — deterministic stored-state failure
		// (ADR-006 taxonomy row 7 in spirit), like a corrupt retry policy.
		logger.ErrorContext(ctx, "corrupt materialized step timeout; recording step failure",
			slog.Any("error", terr))
		return e.completeFailure(ctx, step, terr, dag.ClassPermanent)
	}
	// The claim always stamps a claim_id; the guard only keeps a corrupt
	// row from panicking the journal binding (misuse detection catches the
	// rest downstream).
	claimID := uuid.Nil
	if step.ClaimID != nil {
		claimID = *step.ClaimID
	}
	// The in-flight cancellation watch (ticket 5.6): polls the run's
	// status while the executor runs and cancels its context when the run
	// turns cancelling. Pure latency — the completion transactions below
	// re-check the run status under the run lock either way.
	watchCtx, stopWatch := e.watchRunCancel(ctx, step.RunID)
	out, expired, execErr := runExecutor(watchCtx, executor, exec.StepContext{
		StepType: dag.StepType(step.StepType),
		Config:   step.Config,
		Input:    nil, // input rendering is M6; run_steps carries no input yet
		Attempt:  int(step.AttemptCount),
		// Stable per (run, step) — the same key on every attempt, retry,
		// reclaim, and takeover of this step (ticket 5.5).
		IdempotencyKey: effects.Key(step.RunID, step.StepID),
		Effects:        e.effects.ForStep(step.RunID, step.StepID, int(step.AttemptCount), claimID, logger),
		Logger:         logger,
	}, timeout)
	stopWatch()
	if errors.Is(context.Cause(watchCtx), errRunCancelled) {
		// The routing below stays uniform: a success is honored (the
		// completion transaction skips fan-out on a cancelling run) and a
		// failure settles the step as cancelled — both via the run-status
		// check inside the completion transaction, which is the authority.
		logger.InfoContext(ctx, "executor interrupted by run cancellation",
			slog.Bool("executor_errored", execErr != nil))
	}
	if expired {
		if execErr != nil {
			// The engine assigns the timeout class from context state
			// (ADR-006 row 3) — the executor's own error, whatever it
			// wrapped, is the recorded cause.
			return e.completeFailure(ctx, step,
				fmt.Errorf("step timed out after %s: %w", timeout, execErr), dag.ClassTimeout)
		}
		// Success racing the deadline: the work is done, and discarding it
		// to record a timeout would waste budget and re-run side effects.
		logger.WarnContext(ctx, "executor succeeded after its timeout deadline; honoring the result",
			slog.Duration("timeout", timeout))
	}
	if execErr != nil {
		// Worker-side classification at completion time (ADR-006): a
		// declared class is honored, config-decode misses are permanent,
		// and everything unclassified defaults to transient.
		return e.completeFailure(ctx, step, execErr, classifyFailure(execErr))
	}
	return e.completeSuccess(ctx, step, out)
}

// claimAction is what the handler does with a delivery whose claim (or
// takeover) transaction failed.
type claimAction int

const (
	// claimRedeliver: nothing was decided — leave the entry in the PEL.
	claimRedeliver claimAction = iota
	// claimAckDrop: consuming this delivery is provably unnecessary — ACK
	// and drop.
	claimAckDrop
	// claimTakeover: reclaimed delivery of a running step — perform the
	// lease-expiry takeover (4.5) and execute.
	claimTakeover
)

// String renders the action for structured logs.
func (a claimAction) String() string {
	switch a {
	case claimAckDrop:
		return "ack_drop"
	case claimTakeover:
		return "takeover"
	default:
		return "redeliver"
	}
}

// claimDecision is a classified claim/takeover failure.
type claimDecision struct {
	action claimAction
	reason string
	level  slog.Level
	// holderClaim is the observed holder's fencing token, set only for
	// claimTakeover — the takeover CAS is fenced on it.
	holderClaim *uuid.UUID
}

// classifyClaimFailure maps a failed claim onto ADR-005's ACK-discipline
// table. Pure — unit-tested without a database.
func classifyClaimFailure(err error, deliveryCount int64) claimDecision {
	var te *store.TransitionError
	switch {
	case errors.As(err, &te):
		if te.Reason == store.ConflictRunNotRunning {
			// The run-status guard (ticket 5.2, ADR-006): the run is
			// terminal (later: parked/cancelling), so executing this step
			// is provably unnecessary — drop the delivery.
			return claimDecision{
				action: claimAckDrop, level: slog.LevelInfo,
				reason: "run not running (" + te.From + ") — dropping delivery",
			}
		}
		if te.Reason != store.ConflictWrongStatus {
			// ClaimStep guards only on status, so any other reason is a
			// protocol surprise; keep the entry pending — the rising
			// delivery count walks it to the visible poison path.
			return claimDecision{
				action: claimRedeliver, level: slog.LevelError,
				reason: "unexpected conflict reason " + string(te.Reason),
			}
		}
		switch te.From {
		case store.StepStatusSucceeded, store.StepStatusFailed, store.StepStatusSkipped,
			store.StepStatusDeadLettered, store.StepStatusCancelled:
			return claimDecision{
				action: claimAckDrop, level: slog.LevelInfo,
				reason: "step already terminal (" + te.From + ") — duplicate of finished work",
			}
		case store.StepStatusRetrying:
			// A delivery of a retrying step whose backoff has not elapsed
			// (a due one would have matched the claim CAS): an early
			// duplicate, or the original entry redelivered after a
			// post-commit crash. Dropping is safe — next_attempt_at is
			// durable, and the delayed entry or the reconciler's
			// overdue-retrying heal carries the future dispatch.
			return claimDecision{
				action: claimAckDrop, level: slog.LevelInfo,
				reason: "step retrying with backoff pending — future dispatch is covered",
			}
		case store.StepStatusRunning:
			if deliveryCount <= 1 {
				// Fresh-delivery path: a concurrent duplicate. The live
				// holder's own entry covers the crash case.
				return claimDecision{
					action: claimAckDrop, level: slog.LevelInfo,
					reason: "step already running — concurrent duplicate delivery",
				}
			}
			// Reclaim path: the holder went silent past the lease TTL —
			// take over (ADR-005's lease-expiry takeover, ticket 4.5).
			if te.CurrentClaimID == nil {
				// A running step always carries a claim; nil is corrupt
				// state, and without an observed claim the fenced takeover
				// CAS cannot run. Keep the entry pending — visible.
				return claimDecision{
					action: claimRedeliver, level: slog.LevelError,
					reason: "running step has no claim_id — corrupt state, cannot take over",
				}
			}
			return claimDecision{
				action: claimTakeover, level: slog.LevelWarn,
				reason:      "reclaimed delivery of a running step — holder presumed dead, taking over",
				holderClaim: te.CurrentClaimID,
			}
		default:
			// pending (never dispatched → outbox bug) or an unknown
			// status: not provably unnecessary, keep it pending.
			return claimDecision{
				action: claimRedeliver, level: slog.LevelError,
				reason: "step in unexpected status " + te.From,
			}
		}
	case errors.Is(err, store.ErrNotFound):
		// Dangling reference: run or step row gone (e.g. deleted under a
		// retention policy).
		return claimDecision{
			action: claimAckDrop, level: slog.LevelWarn,
			reason: "run or step not found — dangling reference",
		}
	default:
		// Transport/transaction failure: nothing was decided; redeliver.
		return claimDecision{
			action: claimRedeliver, level: slog.LevelError,
			reason: "claim transaction failed",
		}
	}
}

// classifyTakeoverFailure maps a failed takeover-and-claim transaction onto
// the ACK discipline. Pure — unit-tested without a database. The From=ready
// takeover conflict never reaches here (swallowed inside the transaction);
// a claim conflict (To=running) cannot race under the held run lock, so it
// is a protocol surprise.
func classifyTakeoverFailure(err error) claimDecision {
	var te *store.TransitionError
	switch {
	case errors.As(err, &te):
		if te.Reason == store.ConflictRunNotRunning {
			// The post-takeover claim hit the run-status guard (ticket
			// 5.2): the run is terminal, so the takeover rolled back with
			// it and the delivery is unnecessary.
			return claimDecision{
				action: claimAckDrop, level: slog.LevelInfo,
				reason: "run not running (" + te.From + ") — dropping reclaimed delivery",
			}
		}
		if te.To != store.StepStatusReady {
			return claimDecision{
				action: claimRedeliver, level: slog.LevelError,
				reason: "unexpected conflict on the post-takeover claim",
			}
		}
		switch {
		case te.Reason == store.ConflictClaimMismatch:
			// The step was taken over and re-claimed by a live holder since
			// this entry's observation — the observed claim is stale. The
			// new holder claimed off its own entry, which covers its crash
			// case, so dropping this one is safe.
			return claimDecision{
				action: claimAckDrop, level: slog.LevelWarn,
				reason: "step re-claimed by a newer holder since observation — stale takeover dropped",
			}
		case te.Reason == store.ConflictWrongStatus &&
			(te.From == store.StepStatusSucceeded || te.From == store.StepStatusFailed ||
				te.From == store.StepStatusSkipped || te.From == store.StepStatusRetrying ||
				te.From == store.StepStatusDeadLettered || te.From == store.StepStatusCancelled):
			// ADR-005's takeover race rule: the holder's completion — a
			// terminal one, or since 5.2 a retry routing (5.4: or a
			// dead-lettering) — committed between the reclaim and the
			// takeover CAS. Either way that commit consumed the work this
			// entry carried.
			return claimDecision{
				action: claimAckDrop, level: slog.LevelInfo,
				reason: "step completed between reclaim and takeover (" + te.From + ") — duplicate of finished work",
			}
		default:
			return claimDecision{
				action: claimRedeliver, level: slog.LevelError,
				reason: "unexpected takeover conflict (" + string(te.Reason) + " from " + te.From + ")",
			}
		}
	case errors.Is(err, store.ErrNotFound):
		return claimDecision{
			action: claimAckDrop, level: slog.LevelWarn,
			reason: "run or step not found — dangling reference",
		}
	default:
		return claimDecision{
			action: claimRedeliver, level: slog.LevelError,
			reason: "takeover transaction failed",
		}
	}
}

// claimIDString renders the step's claim ID for logs. ClaimStep always
// returns a non-nil claim, but a nil-check beats a panic in a log call.
func claimIDString(step gen.RunStep) string {
	if step.ClaimID == nil {
		return "<none>"
	}
	return step.ClaimID.String()
}
