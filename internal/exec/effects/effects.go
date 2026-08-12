// Package effects is the side-effect journal (ticket 5.5): the helper
// executors use to make external side effects effectively-once across
// retries, reclaims, and zombie takeovers. The protocol is record-intent →
// execute → record-result against the side_effects table, each journal
// phase in its own short transaction, never holding one across the
// external call. A journaled result short-circuits re-execution on every
// later attempt of the same step.
//
// What the journal does and does not guarantee: once a result row commits,
// the effect never fires again — that is the exactly-once half. A crash
// strictly between the external call and the result commit leaves a
// dangling intent; the next attempt takes it over and re-executes, the
// residual at-least-once window that cannot close without external
// cooperation. That cooperation is the per-(run, step) idempotency key
// (Key): executors pass it to services that dedupe on it, which is exactly
// what M8's http_request does with its Idempotency-Key header.
package effects

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"time"

	"github.com/google/uuid"

	"github.com/mathcslearner/agentloom/internal/dag"
	"github.com/mathcslearner/agentloom/internal/exec"
	"github.com/mathcslearner/agentloom/internal/obs/log"
	"github.com/mathcslearner/agentloom/internal/store"
	"github.com/mathcslearner/agentloom/internal/store/gen"
)

// keyNamespace is the fixed UUIDv5 namespace idempotency keys derive
// under. It must never change: the key contract is "stable per (run,
// step), forever" — a namespace bump would hand every in-flight run a new
// key and double-fire effects at external services that dedupe on it.
var keyNamespace = uuid.MustParse("d9c1b6e8-3f52-4a07-9b6d-8e04a51c2f73")

// Key returns the stable idempotency key for (runID, stepID): a UUIDv5
// over a fixed namespace, so it is deterministic (no storage, survives
// anything), identical across attempts/retries/reclaims/takeovers by
// construction, and opaque (leaks no internal IDs to external services).
func Key(runID uuid.UUID, stepID string) string {
	return uuid.NewSHA1(keyNamespace, []byte(runID.String()+"/"+stepID)).String()
}

// MisuseError reports a journal protocol violation: recording a result for
// an effect whose intent was never recorded (or whose intent token was
// already consumed). This is a bug in the calling executor, not a runtime
// condition — in strict mode (dev/test) the journal panics instead, so the
// bug cannot hide behind a retry loop. Unwraps to a permanent
// *exec.ClassifiedError, so the non-strict path dead-letters the step
// rather than retrying an unwinnable protocol violation.
type MisuseError struct {
	// EffectID is the effect the violation was detected on.
	EffectID string
	// Reason describes the violation.
	Reason string
}

func (e *MisuseError) Error() string {
	return fmt.Sprintf("side-effect journal misuse on effect %q: %s", e.EffectID, e.Reason)
}

// Journal writes the side-effect journal for one worker process. Bind it
// to a claimed step with ForStep; the zero value is not usable — construct
// with New.
type Journal struct {
	store *store.Store
	// now is the injected clock stamped onto intent_at/result_at.
	now func() time.Time
	// strict makes protocol misuse panic (dev/test loudness) instead of
	// returning a MisuseError.
	strict bool
}

// Option customizes a Journal.
type Option func(*Journal)

// WithClock overrides the journal's clock (project invariant: time is
// injectable).
func WithClock(now func() time.Time) Option {
	return func(j *Journal) { j.now = now }
}

// WithStrict sets strict mode: journal misuse panics — the loud dev/test
// behavior (the worker's consumer converts the panic into a no-ACK, so the
// entry walks the poison path into the DLQ, bounded by the delivery-count
// threshold). Non-strict returns the typed permanent error instead, which
// dead-letters the step cleanly.
func WithStrict(strict bool) Option {
	return func(j *Journal) { j.strict = strict }
}

// New builds a Journal over the given store.
func New(s *store.Store, opts ...Option) (*Journal, error) {
	if s == nil {
		return nil, errors.New("effects: New requires a store")
	}
	j := &Journal{store: s, now: time.Now, strict: false}
	for _, opt := range opts {
		opt(j)
	}
	return j, nil
}

// ForStep binds the journal to one claimed step. attempt and claimID are
// recorded on intent rows as diagnostics (who last held the intent), never
// as fencing — the result write is first-wins by the status guard. logger
// may be nil.
func (j *Journal) ForStep(runID uuid.UUID, stepID string, attempt int, claimID uuid.UUID, logger *slog.Logger) *StepJournal {
	if logger == nil {
		logger = slog.Default()
	}
	// Attempts are small positive numbers (policy caps max_attempts at
	// 100); the clamp to [1, MaxInt32] only guards a corrupt caller from
	// an overflowed or CHECK-violating journal row.
	a := int32(1)
	switch {
	case attempt >= math.MaxInt32:
		a = math.MaxInt32
	case attempt >= 1:
		a = int32(attempt)
	}
	return &StepJournal{j: j, runID: runID, stepID: stepID, attempt: a, claimID: claimID, logger: logger}
}

// StepJournal is the journal bound to one claimed step — the concrete
// implementation of exec.EffectJournal, plus the Begin/Complete primitives
// for executors whose effect does not fit the single-callback Do shape.
type StepJournal struct {
	j       *Journal
	runID   uuid.UUID
	stepID  string
	attempt int32
	claimID uuid.UUID
	logger  *slog.Logger
}

var _ exec.EffectJournal = (*StepJournal)(nil)

// Intent is the token Begin returns: proof the intent phase ran. A token
// whose Done method reports true carries the journaled result and must not
// be Completed; a fresh token is consumed by exactly one Complete.
type Intent struct {
	effectID string
	done     bool
	result   json.RawMessage
	consumed bool
}

// Done reports whether the effect already has a journaled result — the
// short-circuit: skip execution and use Result.
func (in *Intent) Done() bool { return in.done }

// Result is the journaled result; meaningful only when Done reports true.
func (in *Intent) Result() json.RawMessage { return in.result }

// Do implements exec.EffectJournal: record-intent → fn → record-result,
// with a journaled result short-circuiting fn. An error from fn is
// returned as-is with the intent left dangling — the effect may have
// partially fired, and the next attempt takes the intent over and
// re-executes.
func (sj *StepJournal) Do(ctx context.Context, effectID string, fn func(context.Context) (json.RawMessage, error)) (json.RawMessage, error) {
	in, err := sj.Begin(ctx, effectID)
	if err != nil {
		return nil, err
	}
	if in.Done() {
		return in.Result(), nil
	}
	out, err := fn(ctx)
	if err != nil {
		return nil, err
	}
	return sj.Complete(ctx, in, out)
}

// Begin runs the intent phase in one short transaction: an existing done
// row short-circuits (the returned token's Done reports true), a dangling
// intent from a dead attempt is taken over (fn must re-execute), and
// otherwise a fresh intent is inserted. Safe against the concurrent-writer
// races a zombie takeover can produce: the row lock serializes recorders,
// and an insert lost to a racing insert retries once against the row that
// now exists.
func (sj *StepJournal) Begin(ctx context.Context, effectID string) (*Intent, error) {
	if effectID == "" {
		return nil, sj.misuse(effectID, "empty effect ID")
	}
	// One retry: two fresh recorders can both see no row and race the
	// insert; the loser's second pass locks the winner's row.
	var insertConflict *store.ConflictError
	for range 2 {
		in := &Intent{effectID: effectID}
		err := sj.j.store.WithTx(ctx, func(ctx context.Context, q store.Querier) error {
			eff, err := q.SideEffects().GetForUpdate(ctx, sj.runID, sj.stepID, effectID)
			switch {
			case err == nil:
				if eff.Status == store.SideEffectStatusDone {
					in.done, in.result = true, eff.Result
					sj.logger.InfoContext(ctx, "side effect already journaled — short-circuiting execution",
						slog.String("effect_id", effectID),
						slog.Int("recorded_by_attempt", int(eff.Attempt)),
						log.Attempt(int(sj.attempt)))
					return nil
				}
				// A dangling intent: its recorder died between intent and
				// result, so the effect may or may not have fired. Re-arm
				// and re-execute — the documented at-least-once window the
				// idempotency key absorbs at the external service.
				_, terr := q.SideEffects().TakeoverIntent(ctx, gen.TakeoverSideEffectIntentParams{
					RunID: sj.runID, StepID: sj.stepID, EffectID: effectID,
					Attempt: sj.attempt, ClaimID: sj.claimID, IntentAt: sj.j.now(),
				})
				if terr == nil {
					sj.logger.WarnContext(ctx, "taking over dangling side-effect intent — effect may fire twice; idempotency key dedupes externally",
						slog.String("effect_id", effectID),
						slog.Int("previous_attempt", int(eff.Attempt)),
						log.Attempt(int(sj.attempt)))
				}
				return terr
			case errors.Is(err, store.ErrNotFound):
				_, ierr := q.SideEffects().InsertIntent(ctx, gen.InsertSideEffectIntentParams{
					RunID: sj.runID, StepID: sj.stepID, EffectID: effectID,
					Attempt: sj.attempt, ClaimID: sj.claimID, IntentAt: sj.j.now(),
				})
				return ierr
			default:
				return err
			}
		})
		if err == nil {
			return in, nil
		}
		if errors.As(err, &insertConflict) {
			continue
		}
		return nil, fmt.Errorf("effects: recording intent for %q: %w", effectID, err)
	}
	return nil, fmt.Errorf("effects: recording intent for %q: lost the insert race twice", effectID)
}

// Complete runs the result phase in one short transaction: mark the effect
// done with its journaled result. First writer wins — if a racing
// completer (a zombie's late write after a takeover re-executed) already
// recorded a result, that stored result is returned instead, keeping the
// journal single-valued. Calling Complete without a fresh token from Begin
// — nil, already done, already consumed, or a row that was never intended
// — is journal misuse: panic in strict mode, a permanent typed error
// otherwise.
func (sj *StepJournal) Complete(ctx context.Context, in *Intent, result json.RawMessage) (json.RawMessage, error) {
	switch {
	case in == nil:
		return nil, sj.misuse("", "recording a result without a Begin token (execute without intent)")
	case in.done:
		return nil, sj.misuse(in.effectID, "recording a result for an effect already journaled done")
	case in.consumed:
		return nil, sj.misuse(in.effectID, "Begin token already consumed by a prior Complete")
	}
	in.consumed = true

	var journaled json.RawMessage
	err := sj.j.store.WithTx(ctx, func(ctx context.Context, q store.Querier) error {
		now := sj.j.now()
		_, err := q.SideEffects().Complete(ctx, gen.CompleteSideEffectParams{
			RunID: sj.runID, StepID: sj.stepID, EffectID: in.effectID,
			Result: result, ResultAt: &now,
		})
		if err == nil {
			journaled = result
			return nil
		}
		if !errors.Is(err, store.ErrNotFound) {
			return err
		}
		// The status-guarded update matched nothing: either a racing
		// completer won (row is done — honor its result) or the intent row
		// does not exist at all (protocol violation).
		eff, gerr := q.SideEffects().Get(ctx, sj.runID, sj.stepID, in.effectID)
		if gerr == nil && eff.Status == store.SideEffectStatusDone {
			sj.logger.WarnContext(ctx, "side-effect result raced a concurrent completer — honoring the first-recorded result",
				slog.String("effect_id", in.effectID),
				slog.Int("winning_attempt", int(eff.Attempt)),
				log.Attempt(int(sj.attempt)))
			journaled = eff.Result
			return nil
		}
		if gerr == nil || errors.Is(gerr, store.ErrNotFound) {
			return sj.misuse(in.effectID, "recording a result for an effect with no intent row (execute without intent)")
		}
		return gerr
	})
	if err != nil {
		var mu *MisuseError
		if errors.As(err, &mu) {
			return nil, err
		}
		return nil, fmt.Errorf("effects: recording result for %q: %w", in.effectID, err)
	}
	return journaled, nil
}

// misuse reports a protocol violation: panic in strict mode (dev/test —
// loud, unhideable), otherwise a permanent-classified error so the step
// dead-letters instead of retrying a bug.
func (sj *StepJournal) misuse(effectID, reason string) error {
	mu := &MisuseError{EffectID: effectID, Reason: reason}
	if sj.j.strict {
		panic(fmt.Sprintf("effects: %v (run %s step %q attempt %d)", mu, sj.runID, sj.stepID, sj.attempt))
	}
	sj.logger.Error("side-effect journal misuse",
		slog.String("effect_id", effectID),
		slog.String("reason", reason),
		log.Attempt(int(sj.attempt)))
	return &exec.ClassifiedError{Class: dag.ClassPermanent, Err: mu}
}
