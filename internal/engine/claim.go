package engine

import (
	"context"
	"errors"
	"log/slog"

	"github.com/mathcslearner/agentloom/internal/dag"
	"github.com/mathcslearner/agentloom/internal/exec"
	"github.com/mathcslearner/agentloom/internal/obs/log"
	"github.com/mathcslearner/agentloom/internal/queue"
	"github.com/mathcslearner/agentloom/internal/store"
	"github.com/mathcslearner/agentloom/internal/store/gen"
)

// Handle is the worker's queue.Handler: attempt the claim CAS for the
// delivered step and act per ADR-005's ACK discipline. A nil return acks
// the entry; an error leaves it in the PEL to redeliver via reclaim.
//
// On a won claim the executor runs, but its result is only logged and the
// step deliberately stays `running`: the completion transaction (persist
// output, CAS running → terminal, edge fan-out) is ticket 4.3, which
// replaces this tail and moves the ACK after that transaction commits.
// Until then the entry is acked once the claim has decided — redelivery
// could only bounce off the running-status CAS, so retrying buys nothing.
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
			slog.Bool("ack", dec.ack),
			slog.Any("error", err))
		if dec.ack {
			return nil
		}
		return err
	}

	e.execute(ctx, step)
	return nil
}

// execute runs the claimed step's executor and logs the outcome. v0 tail
// (ticket 4.2): the output is discarded and the step stays `running` —
// persisting it and computing successor readiness is the completion
// transaction, ticket 4.3.
func (e *Engine) execute(ctx context.Context, step gen.RunStep) {
	ctx = log.With(ctx, log.Attempt(int(step.AttemptCount)))
	logger := log.From(ctx)
	logger.InfoContext(ctx, "step claimed",
		slog.String("step_type", step.StepType),
		slog.String("claim_id", claimIDString(step)))

	executor, err := e.registry.Get(step.StepType)
	if err != nil {
		// Step types are validated against the catalog at submit time, so
		// a miss here means worker/definition version skew or corrupt
		// state — permanent, not retryable. Log loudly and drop; the step
		// stays running until reconciliation semantics (M4.4/M5) reap it.
		logger.ErrorContext(ctx, "no executor registered for step type; discarding delivery",
			slog.String("step_type", step.StepType),
			slog.Any("error", err))
		return
	}

	out, err := executor.Execute(ctx, exec.StepContext{
		StepType: dag.StepType(step.StepType),
		Config:   step.Config,
		Input:    nil, // input rendering is M6; run_steps carries no input yet
		Attempt:  int(step.AttemptCount),
		Logger:   logger,
	})
	if err != nil {
		logger.WarnContext(ctx, "executor failed; result discarded (completion pipeline is ticket 4.3)",
			slog.Any("error", err))
		return
	}
	logger.InfoContext(ctx, "executor succeeded; result discarded (completion pipeline is ticket 4.3)",
		slog.Int("output_bytes", len(out.Data)))
}

// claimDecision is what the handler does with a delivery whose claim
// transaction failed.
type claimDecision struct {
	// ack: true = consuming this delivery is provably unnecessary — ACK
	// and drop; false = leave the entry in the PEL to redeliver.
	ack    bool
	reason string
	level  slog.Level
}

// classifyClaimFailure maps a failed claim onto ADR-005's ACK-discipline
// table. Pure — unit-tested without a database.
func classifyClaimFailure(err error, deliveryCount int64) claimDecision {
	var te *store.TransitionError
	switch {
	case errors.As(err, &te):
		if te.Reason != store.ConflictWrongStatus {
			// ClaimStep guards only on status, so any other reason is a
			// protocol surprise; keep the entry pending — the rising
			// delivery count walks it to the visible poison path.
			return claimDecision{
				ack: false, level: slog.LevelError,
				reason: "unexpected conflict reason " + string(te.Reason),
			}
		}
		switch te.From {
		case store.StepStatusSucceeded, store.StepStatusFailed, store.StepStatusSkipped:
			return claimDecision{
				ack: true, level: slog.LevelInfo,
				reason: "step already terminal (" + te.From + ") — duplicate of finished work",
			}
		case store.StepStatusRunning:
			if deliveryCount <= 1 {
				// Fresh-delivery path: a concurrent duplicate. The live
				// holder's own entry covers the crash case.
				return claimDecision{
					ack: true, level: slog.LevelInfo,
					reason: "step already running — concurrent duplicate delivery",
				}
			}
			// Reclaim path: the holder's lease expired. Lease-expiry
			// takeover (running → ready, clear claim_id, reclaim) is
			// ticket 4.5; until it lands the entry stays pending, rising
			// toward the poison path — visible, never silently dropped.
			return claimDecision{
				ack: false, level: slog.LevelWarn,
				reason: "reclaimed delivery of a running step — lease-expiry takeover lands in ticket 4.5",
			}
		default:
			// pending (never dispatched → outbox bug) or an unknown
			// status: not provably unnecessary, keep it pending.
			return claimDecision{
				ack: false, level: slog.LevelError,
				reason: "step in unexpected status " + te.From,
			}
		}
	case errors.Is(err, store.ErrNotFound):
		// Dangling reference: run or step row gone (e.g. deleted under a
		// retention policy).
		return claimDecision{
			ack: true, level: slog.LevelWarn,
			reason: "run or step not found — dangling reference",
		}
	default:
		// Transport/transaction failure: nothing was decided; redeliver.
		return claimDecision{
			ack: false, level: slog.LevelError,
			reason: "claim transaction failed",
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
