package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgerrcode"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/mathcslearner/agentloom/internal/store/gen"
)

// BlackboardRepo reads the run-scoped blackboard (ticket 12.2, ADR-014).
// Writes go through PutBlackboardEntry, a transition-style helper that
// allocates the next version under the run lock and appends the
// blackboard_updated event, so two concurrent writers of one key can never
// collide and every write is auditable. These read methods serve the
// programmatic StepContext.Blackboard reads (via the pgboard handle) and the
// blackboard API (GET /v1/runs/{id}/blackboard).
type BlackboardRepo interface {
	// Head returns the latest version of a key (ErrNotFound when the key has
	// never been written).
	Head(ctx context.Context, runID uuid.UUID, key string) (gen.BlackboardEntry, error)
	// History returns every version of a key in ascending version order.
	History(ctx context.Context, runID uuid.UUID, key string) ([]gen.BlackboardEntry, error)
	// ListHeads returns the head of every key matching the optional key/tag
	// filters (empty slice = no filter on that dimension), ordered by key.
	ListHeads(ctx context.Context, runID uuid.UUID, keys, tags []string) ([]gen.ListBlackboardHeadsRow, error)
	// ListHeadsPage is the API's keyset page over heads (strictly after
	// afterKey), with the same optional filters.
	ListHeadsPage(ctx context.Context, arg gen.ListBlackboardHeadsPageParams) ([]gen.ListBlackboardHeadsPageRow, error)
	// ListVersionsPage is the API's keyset page over full history (strictly
	// after the (afterKey, afterVersion) cursor), with the same filters.
	ListVersionsPage(ctx context.Context, arg gen.ListBlackboardVersionsPageParams) ([]gen.BlackboardEntry, error)
}

type blackboardRepo struct{ q *gen.Queries }

func (r blackboardRepo) Head(ctx context.Context, runID uuid.UUID, key string) (gen.BlackboardEntry, error) {
	row, err := r.q.GetBlackboardHead(ctx, gen.GetBlackboardHeadParams{RunID: runID, Key: key})
	return row, wrapErr("get blackboard head", err)
}

func (r blackboardRepo) History(ctx context.Context, runID uuid.UUID, key string) ([]gen.BlackboardEntry, error) {
	rows, err := r.q.ListBlackboardHistory(ctx, gen.ListBlackboardHistoryParams{RunID: runID, Key: key})
	return rows, wrapErr("list blackboard history", err)
}

func (r blackboardRepo) ListHeads(ctx context.Context, runID uuid.UUID, keys, tags []string) ([]gen.ListBlackboardHeadsRow, error) {
	rows, err := r.q.ListBlackboardHeads(ctx, gen.ListBlackboardHeadsParams{RunID: runID, Keys: emptyIfNil(keys), Tags: emptyIfNil(tags)})
	return rows, wrapErr("list blackboard heads", err)
}

func (r blackboardRepo) ListHeadsPage(ctx context.Context, arg gen.ListBlackboardHeadsPageParams) ([]gen.ListBlackboardHeadsPageRow, error) {
	arg.Keys, arg.Tags = emptyIfNil(arg.Keys), emptyIfNil(arg.Tags)
	rows, err := r.q.ListBlackboardHeadsPage(ctx, arg)
	return rows, wrapErr("list blackboard heads page", err)
}

func (r blackboardRepo) ListVersionsPage(ctx context.Context, arg gen.ListBlackboardVersionsPageParams) ([]gen.BlackboardEntry, error) {
	arg.Keys, arg.Tags = emptyIfNil(arg.Keys), emptyIfNil(arg.Tags)
	rows, err := r.q.ListBlackboardVersionsPage(ctx, arg)
	return rows, wrapErr("list blackboard versions page", err)
}

// emptyIfNil normalizes a nil filter slice to a non-nil empty slice — the
// queries' cardinality(...) = 0 "no filter" sentinel needs a real array, and
// pgx encodes a nil []string as NULL, which cardinality rejects.
func emptyIfNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// BlackboardVersionConflict reports a compare-and-swap mismatch on a
// blackboard write: the caller expected the head to be Expected but it was
// Current (0 = the key did not exist). Unwraps to ErrConflict; the pgboard
// handle translates it to *blackboard.VersionConflictError for the engine,
// which surfaces it as a transient step error so the retry re-reads the head.
type BlackboardVersionConflict struct {
	Key      string
	Expected int
	Current  int
}

func (e *BlackboardVersionConflict) Error() string {
	return fmt.Sprintf("blackboard CAS conflict on key %q: expected version %d, current %d", e.Key, e.Expected, e.Current)
}

func (e *BlackboardVersionConflict) Unwrap() error { return ErrConflict }

// ErrBlackboardFenced reports a blackboard write from a step whose claim no
// longer matches the run_steps row — a zombie whose lease was taken over
// (ADR-005 fencing). The pgboard translates it to blackboard.ErrFenced.
var ErrBlackboardFenced = errors.New("blackboard write rejected by claim fence")

// BlackboardPutArgs are the inputs to PutBlackboardEntry: one blackboard
// write, already validated (key/tag grammar, value size) and token-counted
// by the pgboard handle. Money-free; the store just allocates the version,
// evaluates the CAS guard, inserts, and events.
type BlackboardPutArgs struct {
	RunID uuid.UUID
	Key   string
	Value json.RawMessage
	// TokenCount is the value's size under TokenCounter (a tokens.Counter
	// fingerprint); stored together so a consumer can recompute on mismatch.
	TokenCount   int32
	TokenCounter string
	// Tags are the entry's validated, deduped, sorted labels (never nil — an
	// empty set is an empty slice so the NOT NULL column is satisfied).
	Tags []string
	// AuthorStepID / AuthorAttempt attribute the write to a step; empty/0 for
	// a non-step writer. When AuthorStepID and FenceClaimID are both set the
	// write is claim-fenced against that step's run_steps row.
	AuthorStepID  string
	AuthorAttempt int32
	// ExpectedVersion is the optional CAS guard: nil writes unconditionally,
	// a non-nil value requires the current head to equal it (0 = must not
	// exist). A mismatch is a *BlackboardVersionConflict.
	ExpectedVersion *int
	// FenceClaimID, when set alongside AuthorStepID, fences the write: the
	// authoring step's run_steps.claim_id must equal it, else the write is a
	// zombie's and rejected with ErrBlackboardFenced. Nil skips fencing
	// (declarative writes ride the already-fenced success CAS; the API and
	// non-step writers do not fence).
	FenceClaimID *uuid.UUID
	// Now is the injected current time (the row's created_at). Required.
	Now time.Time
}

// PutBlackboardEntry appends one version of a blackboard key, atomically
// with the blackboard_updated event (ticket 12.2, ADR-014). It must run
// inside WithTx. It takes the run lock (existence + the uniform run→step
// ordering, and the serialization that makes version allocation collision-
// free), optionally fences on the authoring step's claim, evaluates the CAS
// guard against the current head, inserts head+1, and events — all under one
// transaction. A rejected CAS or fence returns a typed error having written
// nothing (no seq burned), like the guarded transitions.
func PutBlackboardEntry(ctx context.Context, q Querier, args BlackboardPutArgs) (gen.BlackboardEntry, error) {
	const op = "put blackboard entry"
	gq, err := transitionQueries(ctx, q, op, args.Now)
	if err != nil {
		return gen.BlackboardEntry{}, err
	}
	if args.Key == "" {
		return gen.BlackboardEntry{}, fmt.Errorf("store: %s: empty Key", op)
	}
	if len(args.Value) == 0 {
		return gen.BlackboardEntry{}, fmt.Errorf("store: %s: empty Value", op)
	}
	if _, err := lockRun(ctx, gq, op, args.RunID); err != nil {
		return gen.BlackboardEntry{}, err
	}
	// Fence: a taken-over zombie must not mutate shared memory the new holder
	// now owns. Only applies to step-authored writes with a presented claim.
	if args.FenceClaimID != nil && args.AuthorStepID != "" {
		step, err := gq.GetRunStep(ctx, gen.GetRunStepParams{RunID: args.RunID, StepID: args.AuthorStepID})
		if err != nil {
			return gen.BlackboardEntry{}, wrapErr(op+": read authoring step", err)
		}
		if step.ClaimID == nil || *step.ClaimID != *args.FenceClaimID {
			return gen.BlackboardEntry{}, fmt.Errorf("store: %s: key %q, step %q: %w", op, args.Key, args.AuthorStepID, ErrBlackboardFenced)
		}
	}
	head, err := gq.BlackboardHeadVersion(ctx, gen.BlackboardHeadVersionParams{RunID: args.RunID, Key: args.Key})
	if err != nil {
		return gen.BlackboardEntry{}, wrapErr(op+": read head version", err)
	}
	if args.ExpectedVersion != nil && int(head) != *args.ExpectedVersion {
		return gen.BlackboardEntry{}, &BlackboardVersionConflict{Key: args.Key, Expected: *args.ExpectedVersion, Current: int(head)}
	}
	tags := args.Tags
	if tags == nil {
		tags = []string{}
	}
	var authorStep *string
	var authorAttempt *int32
	if args.AuthorStepID != "" {
		authorStep = &args.AuthorStepID
		a := args.AuthorAttempt
		authorAttempt = &a
	}
	entry, err := gq.InsertBlackboardEntry(ctx, gen.InsertBlackboardEntryParams{
		RunID:         args.RunID,
		Key:           args.Key,
		Version:       head + 1,
		Value:         args.Value,
		TokenCount:    args.TokenCount,
		TokenCounter:  args.TokenCounter,
		Tags:          tags,
		AuthorStepID:  authorStep,
		AuthorAttempt: authorAttempt,
		CreatedAt:     args.Now,
	})
	if err != nil {
		// A unique-violation here means a racing writer under a *different*
		// run lock inserted the same (run, key, version) — impossible while
		// the run lock serializes writers, but surfaced honestly if it ever
		// happens rather than silently lost.
		if isUniqueViolation(err) {
			return gen.BlackboardEntry{}, &BlackboardVersionConflict{Key: args.Key, Expected: int(head), Current: int(head + 1)}
		}
		return gen.BlackboardEntry{}, wrapErr(op+": insert entry", err)
	}
	evt := BlackboardUpdatedEvent{
		Key:           entry.Key,
		Version:       entry.Version,
		Tags:          entry.Tags,
		TokenCount:    entry.TokenCount,
		AuthorStepID:  args.AuthorStepID,
		AuthorAttempt: args.AuthorAttempt,
	}
	if err := appendEvent(ctx, gq, op, args.RunID, evt); err != nil {
		return gen.BlackboardEntry{}, err
	}
	return entry, nil
}

// isUniqueViolation reports whether err is a raw Postgres unique-key
// violation. InsertBlackboardEntry is called on the raw generated queries
// (not the wrapErr-wrapping repo), so the driver error is unwrapped here.
func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == pgerrcode.UniqueViolation
}
