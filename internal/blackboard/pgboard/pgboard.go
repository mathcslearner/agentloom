// Package pgboard is the Postgres-backed implementation of the run-scoped
// blackboard (ticket 12.2, ADR-014). It is the only importer of
// internal/store in the blackboard feature, keeping internal/blackboard a
// pure leaf: the leaf owns the domain types and rules, this subpackage binds
// them to storage. The engine builds one *Board over the shared store and
// binds a step-scoped handle (*StepBoard, implementing blackboard.Board)
// onto each StepContext; the write path counts tokens with the step's
// resolved counter, validates against the leaf's key/tag/value rules, and
// routes through store.PutBlackboardEntry (versioned, event-appending,
// claim-fenced). The pgboard translates the store's typed errors back into
// the leaf's so executors and the engine see one error vocabulary.
package pgboard

import (
	"context"
	"errors"
	"log/slog"
	"math"
	"time"

	"github.com/google/uuid"

	"github.com/mathcslearner/agentloom/internal/blackboard"
	"github.com/mathcslearner/agentloom/internal/store"
	"github.com/mathcslearner/agentloom/internal/store/gen"
	"github.com/mathcslearner/agentloom/internal/tokens"
)

// Board is the process-wide blackboard handle over a store. Bind it to a
// claimed step with ForStep; the zero value is not usable — construct with
// New.
type Board struct {
	store *store.Store
	now   func() time.Time
}

// Option customizes a Board.
type Option func(*Board)

// WithClock overrides the board's clock (project invariant: time is
// injectable — the entry's created_at comes from it).
func WithClock(now func() time.Time) Option {
	return func(b *Board) { b.now = now }
}

// New builds a Board over the given store.
func New(s *store.Store, opts ...Option) (*Board, error) {
	if s == nil {
		return nil, errors.New("pgboard: New requires a store")
	}
	b := &Board{store: s, now: time.Now}
	for _, opt := range opts {
		opt(b)
	}
	return b, nil
}

// ForStep binds the board to one claimed step: writes are attributed to the
// step and fenced on its claim, and their token counts use counter (the
// step's resolved model counter, or the chars/4 fallback for a non-llm
// step). counter must not be nil — the engine passes tokens.Fallback() when
// it has no model. logger may be nil.
func (b *Board) ForStep(runID uuid.UUID, stepID string, attempt int, claimID uuid.UUID, counter tokens.Counter, logger *slog.Logger) *StepBoard {
	if logger == nil {
		logger = slog.Default()
	}
	if counter == nil {
		counter = tokens.Fallback()
	}
	a := int32(1)
	switch {
	case attempt >= math.MaxInt32:
		a = math.MaxInt32
	case attempt >= 1:
		a = int32(attempt)
	}
	return &StepBoard{b: b, runID: runID, stepID: stepID, attempt: a, claimID: claimID, counter: counter, logger: logger}
}

// RunBoard binds the board to a run for read-only access with no step
// attribution or fencing (writes are still permitted but unattributed).
// Reserved for a future non-step writer; the API reads through the store
// directly.
func (b *Board) RunBoard(runID uuid.UUID) *StepBoard {
	return &StepBoard{b: b, runID: runID, counter: tokens.Fallback(), logger: slog.Default()}
}

// StepBoard is the blackboard bound to one claimed step — the concrete
// implementation of blackboard.Board the engine puts on StepContext.
type StepBoard struct {
	b       *Board
	runID   uuid.UUID
	stepID  string
	attempt int32
	claimID uuid.UUID
	counter tokens.Counter
	logger  *slog.Logger
}

var _ blackboard.Board = (*StepBoard)(nil)

// Get returns the head of a key.
func (sb *StepBoard) Get(ctx context.Context, key string) (blackboard.Entry, bool, error) {
	if err := blackboard.ValidateKey(key); err != nil {
		return blackboard.Entry{}, false, err
	}
	row, err := sb.b.store.Blackboard().Head(ctx, sb.runID, key)
	if errors.Is(err, store.ErrNotFound) {
		return blackboard.Entry{}, false, nil
	}
	if err != nil {
		return blackboard.Entry{}, false, err
	}
	return entryFromRow(row), true, nil
}

// History returns every version of a key in ascending order.
func (sb *StepBoard) History(ctx context.Context, key string) ([]blackboard.Entry, error) {
	if err := blackboard.ValidateKey(key); err != nil {
		return nil, err
	}
	rows, err := sb.b.store.Blackboard().History(ctx, sb.runID, key)
	if err != nil {
		return nil, err
	}
	return entriesFromRows(rows), nil
}

// List returns the head of each key matching the filter, ordered by key.
func (sb *StepBoard) List(ctx context.Context, filter blackboard.ListFilter) ([]blackboard.Entry, error) {
	rows, err := sb.b.store.Blackboard().ListHeads(ctx, sb.runID, filter.Keys, filter.Tags)
	if err != nil {
		return nil, err
	}
	out := make([]blackboard.Entry, len(rows))
	for i, r := range rows {
		out[i] = blackboard.Entry{
			Key: r.Key, Version: int(r.Version), Value: r.Value,
			TokenCount: int(r.TokenCount), TokenCounter: r.TokenCounter,
			Tags: r.Tags, AuthorStepID: deref(r.AuthorStepID), AuthorAttempt: derefInt(r.AuthorAttempt),
			CreatedAt: r.CreatedAt,
		}
	}
	return out, nil
}

// Put appends a new version of a key, attributed to and fenced on this step.
// It validates the key/tag grammar and value size (permanent typed errors),
// compacts and token-counts the value, then writes through the store,
// translating a CAS conflict to *blackboard.VersionConflictError and a fence
// rejection to blackboard.ErrFenced.
func (sb *StepBoard) Put(ctx context.Context, args blackboard.PutArgs) (blackboard.Entry, error) {
	if err := blackboard.ValidateKey(args.Key); err != nil {
		return blackboard.Entry{}, err
	}
	tags, err := blackboard.NormalizeTags(args.Tags)
	if err != nil {
		return blackboard.Entry{}, err
	}
	if err := blackboard.ValidateValue(args.Key, args.Value); err != nil {
		return blackboard.Entry{}, err
	}
	// Canonicalize the value so identical logical values store identical
	// bytes and count identically — the ADR-014 determinism requirement the
	// stored (token_count, counter_id) rests on.
	value := blackboard.CanonicalValue(args.Value)
	count := sb.counter.Count(string(value))

	put := store.BlackboardPutArgs{
		RunID: sb.runID, Key: args.Key, Value: value,
		TokenCount: clampInt32(count), TokenCounter: sb.counter.ID(),
		Tags:            tags,
		AuthorStepID:    sb.stepID,
		AuthorAttempt:   sb.attempt,
		ExpectedVersion: args.ExpectedVersion,
		Now:             sb.b.now(),
	}
	// A step-bound board with a real claim fences its writes; the read-only
	// RunBoard (no stepID) does not.
	if sb.stepID != "" && sb.claimID != uuid.Nil {
		put.FenceClaimID = &sb.claimID
	}
	row, err := putThroughStore(ctx, sb.b.store, put)
	if err != nil {
		return blackboard.Entry{}, translateStoreErr(err)
	}
	return entryFromRow(row), nil
}

// putThroughStore runs one PutBlackboardEntry in its own short transaction —
// the effects-journal model: a blackboard write during execution is its own
// atomic unit, never held across other work.
func putThroughStore(ctx context.Context, s *store.Store, args store.BlackboardPutArgs) (gen.BlackboardEntry, error) {
	var row gen.BlackboardEntry
	err := s.WithTx(ctx, func(ctx context.Context, q store.Querier) error {
		var err error
		row, err = store.PutBlackboardEntry(ctx, q, args)
		return err
	})
	return row, err
}

// translateStoreErr maps the store's typed blackboard errors onto the leaf's,
// so executors and the engine see one vocabulary (and the engine's transient/
// permanent classification keys off the leaf types).
func translateStoreErr(err error) error {
	var ce *store.BlackboardVersionConflict
	if errors.As(err, &ce) {
		return &blackboard.VersionConflictError{Key: ce.Key, Expected: ce.Expected, Current: ce.Current}
	}
	if errors.Is(err, store.ErrBlackboardFenced) {
		return blackboard.ErrFenced
	}
	return err
}

func entryFromRow(r gen.BlackboardEntry) blackboard.Entry {
	return blackboard.Entry{
		Key: r.Key, Version: int(r.Version), Value: r.Value,
		TokenCount: int(r.TokenCount), TokenCounter: r.TokenCounter,
		Tags: r.Tags, AuthorStepID: deref(r.AuthorStepID), AuthorAttempt: derefInt(r.AuthorAttempt),
		CreatedAt: r.CreatedAt,
	}
}

func entriesFromRows(rows []gen.BlackboardEntry) []blackboard.Entry {
	out := make([]blackboard.Entry, len(rows))
	for i, r := range rows {
		out[i] = entryFromRow(r)
	}
	return out
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func derefInt(i *int32) int {
	if i == nil {
		return 0
	}
	return int(*i)
}

func clampInt32(n int) int32 {
	switch {
	case n < 0:
		return 0
	case n > math.MaxInt32:
		return math.MaxInt32
	default:
		return int32(n)
	}
}
