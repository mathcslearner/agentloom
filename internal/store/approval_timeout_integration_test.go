//go:build integration

package store_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mathcslearner/agentloom/internal/store"
	"github.com/mathcslearner/agentloom/internal/store/gen"
)

// TestExpireApprovalCAS is the 15.4 timeout arbiter at the store level: a
// reject/approve timeout policy CASes pending → expired through DecideApproval
// with a timeout source, records the decision fields + expired_at, and appends
// the distinct approval_expired event (not approval_decided). A second decision
// on the now non-pending row loses the CAS.
func TestExpireApprovalCAS(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	ctx := t.Context()
	runID, row := parkGate(t, s)
	expiredAt := testNow.Add(48 * time.Hour)

	var decided gen.Approval
	if err := s.WithTx(ctx, func(ctx context.Context, q store.Querier) error {
		var err error
		decided, err = store.DecideApproval(ctx, q, store.DecideApprovalArgs{
			ID: row.ID, RunID: runID, StepID: "gate", AttemptNo: 1,
			Status: store.ApprovalStatusExpired, Decision: "reject",
			DecidedBy: store.ApprovalActorTimeout, Source: store.ApprovalSourceTimeout,
			ExpiredAt: &expiredAt, TimeoutAt: row.TimeoutAt, Now: testNow,
		})
		return err
	}); err != nil {
		t.Fatalf("DecideApproval (expire): %v", err)
	}
	if decided.Status != store.ApprovalStatusExpired {
		t.Errorf("status = %q, want expired", decided.Status)
	}
	if decided.Decision == nil || *decided.Decision != "reject" {
		t.Errorf("decision = %v, want reject", decided.Decision)
	}
	if decided.DecisionSource == nil || *decided.DecisionSource != store.ApprovalSourceTimeout {
		t.Errorf("decision_source = %v, want timeout", decided.DecisionSource)
	}
	if decided.ExpiredAt == nil {
		t.Error("expired_at not stamped")
	}
	got := eventTypes(t, s, runID)
	if !containsEvent(got, store.EventApprovalExpired) {
		t.Errorf("events = %v, want approval_expired", got)
	}
	if containsEvent(got, store.EventApprovalDecided) {
		t.Errorf("events = %v, a timeout must not emit approval_decided", got)
	}

	// A second decision on the non-pending row loses the CAS.
	err := s.WithTx(ctx, func(ctx context.Context, q store.Querier) error {
		_, derr := store.DecideApproval(ctx, q, store.DecideApprovalArgs{
			ID: row.ID, RunID: runID, StepID: "gate", AttemptNo: 1,
			Status: store.ApprovalStatusApproved, Decision: "approve",
			DecidedBy: "key_op", Source: store.ApprovalSourceHuman, Now: testNow,
		})
		return derr
	})
	var notPending *store.ApprovalNotPendingError
	if !errors.As(err, &notPending) || notPending.Status != store.ApprovalStatusExpired {
		t.Fatalf("second decide err = %v, want *ApprovalNotPendingError(expired)", err)
	}
}

// TestExpireVsDecideSingleWinner: a human decision and a timeout expiry racing
// the SAME approvals row have exactly one winner — the single-arbiter CAS. The
// loser gets *ApprovalNotPendingError and writes nothing (the tx rolls back).
func TestExpireVsDecideSingleWinner(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	ctx := t.Context()
	runID, row := parkGate(t, s)
	expiredAt := testNow.Add(48 * time.Hour)

	var wg sync.WaitGroup
	wg.Add(2)
	results := make([]error, 2)
	decide := func(i int, args store.DecideApprovalArgs) {
		defer wg.Done()
		results[i] = s.WithTx(ctx, func(ctx context.Context, q store.Querier) error {
			_, err := store.DecideApproval(ctx, q, args)
			return err
		})
	}
	go decide(0, store.DecideApprovalArgs{
		ID: row.ID, RunID: runID, StepID: "gate", AttemptNo: 1,
		Status: store.ApprovalStatusApproved, Decision: "approve",
		DecidedBy: "key_op", Source: store.ApprovalSourceHuman, Now: testNow,
	})
	go decide(1, store.DecideApprovalArgs{
		ID: row.ID, RunID: runID, StepID: "gate", AttemptNo: 1,
		Status: store.ApprovalStatusExpired, Decision: "reject",
		DecidedBy: store.ApprovalActorTimeout, Source: store.ApprovalSourceTimeout,
		ExpiredAt: &expiredAt, TimeoutAt: row.TimeoutAt, Now: testNow,
	})
	wg.Wait()

	winners, losers := 0, 0
	for _, err := range results {
		var notPending *store.ApprovalNotPendingError
		switch {
		case err == nil:
			winners++
		case errors.As(err, &notPending):
			losers++
		default:
			t.Fatalf("unexpected decide error: %v", err)
		}
	}
	if winners != 1 || losers != 1 {
		t.Fatalf("winners=%d losers=%d, want exactly one of each", winners, losers)
	}
	// Exactly one settlement event landed.
	got := eventTypes(t, s, runID)
	n := 0
	for _, e := range got {
		if e == store.EventApprovalDecided || e == store.EventApprovalExpired {
			n++
		}
	}
	if n != 1 {
		t.Errorf("settlement events = %d (%v), want exactly 1", n, got)
	}
}

// TestExpireApprovalParkMarks: the park policy stamps expired_at, keeps the
// approval pending (still decidable), parks the run (reason awaiting_human),
// and emits approval_expired with action run_parked. A second application finds
// expired_at set and returns *ApprovalAlreadyExpiredError — the run is not
// re-parked (idempotent under redelivery).
func TestExpireApprovalParkMarks(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	ctx := t.Context()
	runID, row := parkGate(t, s)

	var res store.ExpireApprovalParkResult
	if err := s.WithTx(ctx, func(ctx context.Context, q store.Querier) error {
		var err error
		res, err = store.ExpireApprovalPark(ctx, q, store.ExpireApprovalParkArgs{
			ID: row.ID, RunID: runID, StepID: "gate", AttemptNo: 1,
			TimeoutAt: row.TimeoutAt, Now: testNow,
		})
		return err
	}); err != nil {
		t.Fatalf("ExpireApprovalPark: %v", err)
	}
	if res.Action != store.ApprovalActionRunParked {
		t.Errorf("action = %q, want run_parked", res.Action)
	}
	if res.Approval.Status != store.ApprovalStatusPending {
		t.Errorf("approval status = %q, want still pending", res.Approval.Status)
	}
	if res.Approval.ExpiredAt == nil {
		t.Error("expired_at not stamped")
	}
	run, _ := s.Runs().Get(ctx, runID)
	if run.Status != store.RunStatusParked {
		t.Errorf("run status = %q, want parked", run.Status)
	}
	if run.ParkReason == nil || *run.ParkReason != store.ParkReasonAwaitingHuman {
		t.Errorf("park reason = %v, want awaiting_human", run.ParkReason)
	}
	if got := eventTypes(t, s, runID); !containsEvent(got, store.EventApprovalExpired) {
		t.Errorf("events = %v, want approval_expired", got)
	}

	// A redelivered expiry finds expired_at set → already-expired, no re-park.
	err := s.WithTx(ctx, func(ctx context.Context, q store.Querier) error {
		_, perr := store.ExpireApprovalPark(ctx, q, store.ExpireApprovalParkArgs{
			ID: row.ID, RunID: runID, StepID: "gate", AttemptNo: 1,
			TimeoutAt: row.TimeoutAt, Now: testNow,
		})
		return perr
	})
	var already *store.ApprovalAlreadyExpiredError
	if !errors.As(err, &already) {
		t.Fatalf("second park err = %v, want *ApprovalAlreadyExpiredError", err)
	}
}

// TestGetPendingApprovalByStep resolves a step's open approval by (run, step),
// and returns ErrNotFound once the approval is off pending.
func TestGetPendingApprovalByStep(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	ctx := t.Context()
	runID, row := parkGate(t, s)

	got, err := s.Approvals().GetPendingByStep(ctx, runID, "gate")
	if err != nil || got.ID != row.ID {
		t.Fatalf("GetPendingByStep = %+v, err %v; want row %s", got, err, row.ID)
	}

	// Cancel it off pending; the pending lookup now misses.
	if err := s.WithTx(ctx, func(ctx context.Context, q store.Querier) error {
		_, cerr := store.CancelAwaitingHumanStep(ctx, q, store.CancelAwaitingHumanStepArgs{
			RunID: runID, StepID: "gate", Reason: store.CancelReasonRunCancelled, Now: testNow,
		})
		return cerr
	}); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if _, err := s.Approvals().GetPendingByStep(ctx, runID, "gate"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("GetPendingByStep after cancel err = %v, want ErrNotFound", err)
	}
}

// TestListOverdueApprovals returns only pending, unexpired, timeout-bearing
// approvals whose deadline is at or before the threshold.
func TestListOverdueApprovals(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	ctx := t.Context()
	runID, row := parkGate(t, s) // timeout_at = testNow + 48h

	// Before the deadline: not overdue.
	before := row.TimeoutAt.Add(-time.Hour)
	overdue := listOverdue(t, s, before)
	if containsOverdue(overdue, runID) {
		t.Errorf("approval listed overdue before its deadline: %+v", overdue)
	}
	// After the deadline: overdue.
	overdue = listOverdue(t, s, row.TimeoutAt.Add(time.Hour))
	if !containsOverdue(overdue, runID) {
		t.Errorf("approval not listed overdue past its deadline: %+v", overdue)
	}

	// Once its park policy is applied (expired_at set), it drops out.
	if err := s.WithTx(ctx, func(ctx context.Context, q store.Querier) error {
		_, perr := store.ExpireApprovalPark(ctx, q, store.ExpireApprovalParkArgs{
			ID: row.ID, RunID: runID, StepID: "gate", AttemptNo: 1,
			TimeoutAt: row.TimeoutAt, Now: testNow,
		})
		return perr
	}); err != nil {
		t.Fatalf("ExpireApprovalPark: %v", err)
	}
	overdue = listOverdue(t, s, row.TimeoutAt.Add(time.Hour))
	if containsOverdue(overdue, runID) {
		t.Errorf("expired-marked approval still listed overdue: %+v", overdue)
	}
}

func listOverdue(t *testing.T, s *store.Store, before time.Time) []store.OverdueApproval {
	t.Helper()
	var out []store.OverdueApproval
	if err := s.WithTx(t.Context(), func(ctx context.Context, q store.Querier) error {
		var err error
		out, err = store.ListOverdueApprovals(ctx, q, before, 100)
		return err
	}); err != nil {
		t.Fatalf("ListOverdueApprovals: %v", err)
	}
	return out
}

func containsOverdue(list []store.OverdueApproval, runID uuid.UUID) bool {
	for _, a := range list {
		if a.RunID == runID && a.StepID == "gate" {
			return true
		}
	}
	return false
}
