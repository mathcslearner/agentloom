//go:build integration

package effects_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mathcslearner/agentloom/internal/dag"
	"github.com/mathcslearner/agentloom/internal/exec"
	"github.com/mathcslearner/agentloom/internal/exec/effects"
	"github.com/mathcslearner/agentloom/internal/store"
	"github.com/mathcslearner/agentloom/internal/store/gen"
	"github.com/mathcslearner/agentloom/internal/store/storetest"
)

// Ticket 5.5's journal-protocol suite: the record-intent → execute →
// record-result composition over the store primitives — short-circuit on a
// journaled result, dangling-intent takeover and re-execution, first-wins
// result racing, and misuse loudness in both modes.

var testNow = time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC)

const oneStepDef = `{
	"schema_version": 1,
	"name": "effects-journal",
	"steps": [{"id": "a", "type": "noop"}],
	"edges": []
}`

// harness is one isolated database with a run whose step "a" is claimed
// (the claim_id feeds ForStep). pool allows raw SQL for scenarios the repo
// surface deliberately has no verbs for (vanishing a row).
type harness struct {
	store *store.Store
	pool  *pgxpool.Pool
	run   gen.Run
	step  gen.RunStep
}

func newHarness(t *testing.T) harness {
	t.Helper()
	pool := storetest.NewDB(t)
	s := store.NewFromPool(pool)
	def, err := dag.Decode([]byte(oneStepDef))
	if err != nil {
		t.Fatalf("decoding definition: %v", err)
	}
	res, err := s.CreateRun(t.Context(), store.CreateRunArgs{Definition: def, Now: testNow})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	var step gen.RunStep
	err = s.WithTx(t.Context(), func(ctx context.Context, q store.Querier) error {
		var cerr error
		step, cerr = store.ClaimStep(ctx, q, store.ClaimStepArgs{RunID: res.Run.ID, StepID: "a", Now: testNow})
		return cerr
	})
	if err != nil {
		t.Fatalf("ClaimStep: %v", err)
	}
	return harness{store: s, pool: pool, run: res.Run, step: step}
}

func newJournal(t *testing.T, s *store.Store, opts ...effects.Option) *effects.Journal {
	t.Helper()
	opts = append([]effects.Option{effects.WithClock(func() time.Time { return testNow })}, opts...)
	j, err := effects.New(s, opts...)
	if err != nil {
		t.Fatalf("effects.New: %v", err)
	}
	return j
}

// TestDoJournalsOnceAndShortCircuits: the headline contract — fn fires
// exactly once; every later attempt gets the journaled result without
// executing.
func TestDoJournalsOnceAndShortCircuits(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	j := newJournal(t, h.store)
	ctx := t.Context()

	fires := 0
	fn := func(context.Context) (json.RawMessage, error) {
		fires++
		return json.RawMessage(`{"fired": true}`), nil
	}

	sj1 := j.ForStep(h.run.ID, "a", 1, *h.step.ClaimID, nil)
	out, err := sj1.Do(ctx, "call", fn)
	if err != nil {
		t.Fatalf("first Do: %v", err)
	}
	if fires != 1 || string(out) != `{"fired": true}` {
		t.Errorf("first Do: fires=%d out=%s, want one execution with its result", fires, out)
	}

	// A later attempt (retry or reclaim — the journal cannot tell and does
	// not care) short-circuits.
	sj2 := j.ForStep(h.run.ID, "a", 2, uuid.New(), nil)
	out, err = sj2.Do(ctx, "call", fn)
	if err != nil {
		t.Fatalf("second Do: %v", err)
	}
	if fires != 1 {
		t.Errorf("second Do re-executed: fires=%d, want 1 (short-circuit)", fires)
	}
	if string(out) != `{"fired": true}` {
		t.Errorf("second Do result = %s, want the journaled result", out)
	}

	// Distinct effect IDs are distinct effects.
	if _, err := sj2.Do(ctx, "other", fn); err != nil {
		t.Fatalf("Do on a second effect: %v", err)
	}
	if fires != 2 {
		t.Errorf("second effect: fires=%d, want 2 (its own journal row)", fires)
	}
}

// TestDoTakesOverDanglingIntent: a recorder that died between intent and
// result leaves the effect ambiguous — the next attempt re-arms the intent
// and re-executes (the residual at-least-once window).
func TestDoTakesOverDanglingIntent(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	j := newJournal(t, h.store)
	ctx := t.Context()

	// Attempt 1 records intent and "crashes" (never completes).
	sj1 := j.ForStep(h.run.ID, "a", 1, *h.step.ClaimID, nil)
	if _, err := sj1.Begin(ctx, "call"); err != nil {
		t.Fatalf("Begin: %v", err)
	}

	fires := 0
	sj2 := j.ForStep(h.run.ID, "a", 2, uuid.New(), nil)
	out, err := sj2.Do(ctx, "call", func(context.Context) (json.RawMessage, error) {
		fires++
		return json.RawMessage(`{"attempt": 2}`), nil
	})
	if err != nil {
		t.Fatalf("Do after dangling intent: %v", err)
	}
	if fires != 1 || string(out) != `{"attempt": 2}` {
		t.Errorf("takeover Do: fires=%d out=%s, want re-execution", fires, out)
	}
	eff, err := h.store.SideEffects().Get(ctx, h.run.ID, "a", "call")
	if err != nil {
		t.Fatalf("reading journal row: %v", err)
	}
	if eff.Status != store.SideEffectStatusDone || eff.Attempt != 2 {
		t.Errorf("row = status %q attempt %d, want done under attempt 2", eff.Status, eff.Attempt)
	}
}

// TestDoErrorLeavesIntentDangling: fn failing does not journal a result —
// the intent stays, and the retry re-executes.
func TestDoErrorLeavesIntentDangling(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	j := newJournal(t, h.store)
	ctx := t.Context()

	sj := j.ForStep(h.run.ID, "a", 1, *h.step.ClaimID, nil)
	boom := errors.New("provider 503")
	if _, err := sj.Do(ctx, "call", func(context.Context) (json.RawMessage, error) {
		return nil, boom
	}); !errors.Is(err, boom) {
		t.Fatalf("Do with failing fn: %v, want the fn's error", err)
	}
	eff, err := h.store.SideEffects().Get(ctx, h.run.ID, "a", "call")
	if err != nil {
		t.Fatalf("reading journal row: %v", err)
	}
	if eff.Status != store.SideEffectStatusIntent {
		t.Errorf("row after failed fn = %q, want a dangling intent", eff.Status)
	}

	// The retry re-executes and completes.
	fires := 0
	sj2 := j.ForStep(h.run.ID, "a", 2, uuid.New(), nil)
	if _, err := sj2.Do(ctx, "call", func(context.Context) (json.RawMessage, error) {
		fires++
		return json.RawMessage(`{"ok": true}`), nil
	}); err != nil || fires != 1 {
		t.Errorf("retry Do: err=%v fires=%d, want clean re-execution", err, fires)
	}
}

// TestCompleteFirstWins: a zombie's late result loses to the result already
// journaled by the attempt that took over — the journal stays
// single-valued and both callers observe the same result.
func TestCompleteFirstWins(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	j := newJournal(t, h.store)
	ctx := t.Context()

	// Zombie attempt 1 records intent, stalls mid-execute.
	zombie := j.ForStep(h.run.ID, "a", 1, *h.step.ClaimID, nil)
	zin, err := zombie.Begin(ctx, "call")
	if err != nil {
		t.Fatalf("zombie Begin: %v", err)
	}

	// Attempt 2 takes over, executes, and journals its result.
	sj2 := j.ForStep(h.run.ID, "a", 2, uuid.New(), nil)
	out, err := sj2.Do(ctx, "call", func(context.Context) (json.RawMessage, error) {
		return json.RawMessage(`{"winner": 2}`), nil
	})
	if err != nil || string(out) != `{"winner": 2}` {
		t.Fatalf("takeover Do: out=%s err=%v", out, err)
	}

	// The zombie wakes and records its own result: first writer won, the
	// zombie gets the stored result back, not its own.
	got, err := zombie.Complete(ctx, zin, json.RawMessage(`{"winner": 1}`))
	if err != nil {
		t.Fatalf("zombie Complete: %v", err)
	}
	if string(got) != `{"winner": 2}` {
		t.Errorf("zombie Complete returned %s, want the first-recorded result", got)
	}
	eff, err := h.store.SideEffects().Get(ctx, h.run.ID, "a", "call")
	if err != nil {
		t.Fatalf("reading journal row: %v", err)
	}
	if string(eff.Result) != `{"winner": 2}` {
		t.Errorf("journaled result = %s, want the first writer's intact", eff.Result)
	}
}

// TestMisuseNonStrict: protocol violations surface as permanent-classified
// *MisuseError — the step dead-letters instead of retrying a bug.
func TestMisuseNonStrict(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	j := newJournal(t, h.store) // strict off by default
	ctx := t.Context()
	sj := j.ForStep(h.run.ID, "a", 1, *h.step.ClaimID, nil)

	assertMisuse := func(err error, wantReason string) {
		t.Helper()
		var mu *effects.MisuseError
		if !errors.As(err, &mu) || !strings.Contains(mu.Reason, wantReason) {
			t.Fatalf("error = %v, want *MisuseError with reason containing %q", err, wantReason)
		}
		var ce *exec.ClassifiedError
		if !errors.As(err, &ce) || ce.Class != dag.ClassPermanent {
			t.Errorf("misuse not classified permanent: %v", err)
		}
	}

	// Execute without intent: no Begin token at all.
	_, err := sj.Complete(ctx, nil, json.RawMessage(`{}`))
	assertMisuse(err, "without a Begin token")

	// A consumed token cannot complete twice.
	in, err := sj.Begin(ctx, "call")
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if _, err := sj.Complete(ctx, in, json.RawMessage(`{"n": 1}`)); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	_, err = sj.Complete(ctx, in, json.RawMessage(`{"n": 2}`))
	assertMisuse(err, "already consumed")

	// A done token from a short-circuiting Begin must not be completed.
	in2, err := sj.Begin(ctx, "call")
	if err != nil {
		t.Fatalf("Begin on done effect: %v", err)
	}
	if !in2.Done() {
		t.Fatal("Begin on done effect: token not Done")
	}
	_, err = sj.Complete(ctx, in2, json.RawMessage(`{}`))
	assertMisuse(err, "already journaled done")

	// The DB-detected case: a token whose intent row vanished (execute
	// without intent as seen from the store).
	in3, err := sj.Begin(ctx, "vanishing")
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if _, err := h.pool.Exec(ctx,
		`DELETE FROM side_effects WHERE run_id = $1 AND step_id = 'a' AND effect_id = 'vanishing'`,
		h.run.ID); err != nil {
		t.Fatalf("deleting intent row: %v", err)
	}
	_, err = sj.Complete(ctx, in3, json.RawMessage(`{}`))
	assertMisuse(err, "no intent row")
}

// TestMisuseStrictPanics: strict mode (dev/test) panics on misuse — the
// loudest possible failure, converted by the worker's consumer into the
// poison path.
func TestMisuseStrictPanics(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	j := newJournal(t, h.store, effects.WithStrict(true))
	sj := j.ForStep(h.run.ID, "a", 1, *h.step.ClaimID, nil)

	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("Complete without Begin under strict mode did not panic")
		}
		if !strings.Contains(fmt.Sprint(r), "journal misuse") {
			t.Errorf("panic = %v, want a journal-misuse message", r)
		}
	}()
	_, _ = sj.Complete(t.Context(), nil, json.RawMessage(`{}`))
}
