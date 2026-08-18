//go:build integration

package engine_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mathcslearner/agentloom/internal/dag"
	"github.com/mathcslearner/agentloom/internal/engine"
	"github.com/mathcslearner/agentloom/internal/notify"
	"github.com/mathcslearner/agentloom/internal/queue"
	"github.com/mathcslearner/agentloom/internal/queue/queuetest"
	"github.com/mathcslearner/agentloom/internal/store"
	"github.com/mathcslearner/agentloom/internal/store/gen"
)

// noSleep is the injected webhook backoff sleep: instant, so retry timing does
// not slow the test (and cannot outrun the queue lease).
func noSleep(context.Context, time.Duration) error { return nil }

// webhookRecorder is a test HMAC-validating receiver. It replies with the
// scripted statuses in order (per request), records each request, and counts
// how many distinct delivery ids it accepted with a 2xx.
type webhookRecorder struct {
	mu         sync.Mutex
	secret     string
	statuses   []int // returned in order; the last is repeated once exhausted
	hits       int
	invalidSig int
	accepted   map[string]int // delivery id -> count of 2xx responses
}

func newWebhookRecorder(secret string, statuses ...int) *webhookRecorder {
	return &webhookRecorder{secret: secret, statuses: statuses, accepted: map[string]int{}}
}

func (rec *webhookRecorder) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		ts := r.Header.Get(notify.HeaderTimestamp)
		sig := r.Header.Get(notify.HeaderSignature)
		id := r.Header.Get(notify.HeaderDeliveryID)
		rec.mu.Lock()
		defer rec.mu.Unlock()
		if !notify.Verify(rec.secret, ts, body, sig) {
			rec.invalidSig++
		}
		status := rec.statuses[len(rec.statuses)-1]
		if rec.hits < len(rec.statuses) {
			status = rec.statuses[rec.hits]
		}
		rec.hits++
		if status >= 200 && status < 300 {
			rec.accepted[id]++
		}
		w.WriteHeader(status)
	}
}

func (rec *webhookRecorder) snapshot() (hits, invalid, distinct int) {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	return rec.hits, rec.invalidSig, len(rec.accepted)
}

// waitEvent polls until the run has at least one event of typ (or fails).
func waitEvent(t *testing.T, s *store.Store, runID uuid.UUID, typ string) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if countEvents(t, s, runID, typ) > 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("run never emitted a %q event", typ)
}

// TestApprovalWebhookDeliveredOnce is 15.5 DoD-1: a parked human_approval step
// delivers exactly one valid HMAC-signed notification despite injected
// transient failures (500, 500, 200), and a re-invocation short-circuits on
// the side-effect journal (no second POST).
func TestApprovalWebhookDeliveredOnce(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	secret := "webhook-secret"
	rec := newWebhookRecorder(secret, 500, 500, 200)
	srv := httptest.NewServer(rec.handler())
	defer srv.Close()

	wh, err := notify.NewWebhook(notify.WebhookConfig{
		URL: srv.URL, Secret: secret, MaxAttempts: 3,
		Now: func() time.Time { return testNow }, Sleep: noSleep,
	})
	if err != nil {
		t.Fatalf("NewWebhook: %v", err)
	}

	s, h, runID := setupWithParams(t, readDef(t, "approval_gate.json"), json.RawMessage(`{"topic": "turtles"}`))
	d := startDispatcher(t, s, h.Queue())
	reg := approvalRegistry(t)
	w, err := engine.New(s, reg, "worker-a", engine.WithDispatchNudge(d.Nudge), engine.WithNotifier(wh))
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	h.Spawn("worker-a", w.Handle, queuetest.LeaseConfig(2*time.Second))

	waitStep(t, s, runID, "approve_publish", "awaiting_human", func(st gen.RunStep) bool {
		return st.Status == store.StepStatusAwaitingHuman
	})
	waitEvent(t, s, runID, store.EventApprovalNotified)

	hits, invalid, distinct := rec.snapshot()
	if hits != 3 {
		t.Errorf("webhook hit %d times, want 3 (500, 500, 200)", hits)
	}
	if invalid != 0 {
		t.Errorf("%d requests had an invalid HMAC signature, want 0", invalid)
	}
	if distinct != 1 {
		t.Errorf("distinct accepted deliveries = %d, want exactly 1", distinct)
	}

	// The delivery is journaled done, so the notification is effectively-once.
	approvals, err := s.Approvals().ListByRun(ctx, runID)
	if err != nil {
		t.Fatalf("ListByRun: %v", err)
	}
	ap := approvals[0]
	eff, err := s.SideEffects().Get(ctx, runID, "approve_publish", "approval_notify:"+ap.ID.String())
	if err != nil {
		t.Fatalf("reading side-effect journal row: %v", err)
	}
	if eff.Status != store.SideEffectStatusDone {
		t.Errorf("journal status = %q, want done", eff.Status)
	}
	if n := countEvents(t, s, runID, store.EventApprovalNotified); n != 1 {
		t.Errorf("approval_notified events = %d, want 1", n)
	}

	// Re-invoking the notifier for the same approval must short-circuit on the
	// journal — no second POST reaches the receiver.
	step, err := s.Steps().Get(ctx, runID, "approve_publish")
	if err != nil {
		t.Fatalf("reading step: %v", err)
	}
	w.NotifyApproval(ctx, step, store.ApprovalRow{
		ID: ap.ID, Title: ap.Title, Description: ap.Description, Payload: ap.Payload,
		AllowedDecisions: ap.AllowedDecisions, AllowEdit: ap.AllowEdit,
		EditSchema: ap.EditSchema, TimeoutAt: ap.TimeoutAt,
	}, "")
	if hits2, _, _ := rec.snapshot(); hits2 != hits {
		t.Errorf("re-invocation POSTed again (%d → %d hits); journal did not short-circuit", hits, hits2)
	}
}

// TestApprovalWebhookFailureNeverBlocksRun is 15.5 DoD-3: a webhook that never
// succeeds records a warning event but never affects run correctness — the
// step stays parked and decidable, and a decision drives it to completion.
func TestApprovalWebhookFailureNeverBlocksRun(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	secret := "s"
	rec := newWebhookRecorder(secret, 500) // always 500
	srv := httptest.NewServer(rec.handler())
	defer srv.Close()

	wh, err := notify.NewWebhook(notify.WebhookConfig{
		URL: srv.URL, Secret: secret, MaxAttempts: 2,
		Now: func() time.Time { return testNow }, Sleep: noSleep,
	})
	if err != nil {
		t.Fatalf("NewWebhook: %v", err)
	}

	s, h, runID := setupWithParams(t, readDef(t, "approval_gate.json"), json.RawMessage(`{"topic": "otters"}`))
	d := startDispatcher(t, s, h.Queue())
	reg := approvalRegistry(t)
	w, err := engine.New(s, reg, "worker-a",
		engine.WithDispatchNudge(d.Nudge), engine.WithNotifier(wh), engine.WithClock(func() time.Time { return testNow }))
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	h.Spawn("worker-a", w.Handle, queuetest.LeaseConfig(2*time.Second))

	waitStep(t, s, runID, "approve_publish", "awaiting_human", func(st gen.RunStep) bool {
		return st.Status == store.StepStatusAwaitingHuman
	})
	// The notification failed but the park is intact: a warning event, run still
	// running, PEL empty (the park ACKed despite the webhook failure).
	waitEvent(t, s, runID, store.EventApprovalNotificationFailed)
	if run, _ := s.Runs().Get(ctx, runID); run.Status != store.RunStatusRunning {
		t.Fatalf("run status = %q after a failed notification, want running", run.Status)
	}
	if n := countEvents(t, s, runID, store.EventApprovalNotified); n != 0 {
		t.Errorf("approval_notified events = %d, want 0 (delivery failed)", n)
	}
	stats := h.WaitStats(ctx, func(st queue.StreamStats) bool { return st.Pending == 0 })
	if stats.Pending != 0 {
		t.Errorf("PEL has %d entries; a failed webhook must not hold the lease", stats.Pending)
	}

	// The run is fully decidable: approve → the gate succeeds → publish runs →
	// run succeeds. The webhook failure changed nothing.
	approvals, err := s.Approvals().ListByRun(ctx, runID)
	if err != nil {
		t.Fatalf("ListByRun: %v", err)
	}
	if _, err := w.Decide(ctx, approvals[0].ID, engine.DecideRequest{
		Decision: dag.ApprovalApprove, DecidedBy: "test", Source: store.ApprovalSourceHuman,
	}); err != nil {
		t.Fatalf("Decide: %v", err)
	}
	waitRun(t, s, runID, store.RunStatusSucceeded)
}

// TestApprovalWebhookPermanentFailure: a 4xx is permanent — one attempt, one
// warning event, no retries.
func TestApprovalWebhookPermanentFailure(t *testing.T) {
	t.Parallel()

	secret := "s"
	rec := newWebhookRecorder(secret, 400)
	srv := httptest.NewServer(rec.handler())
	defer srv.Close()

	wh, err := notify.NewWebhook(notify.WebhookConfig{
		URL: srv.URL, Secret: secret, MaxAttempts: 3,
		Now: func() time.Time { return testNow }, Sleep: noSleep,
	})
	if err != nil {
		t.Fatalf("NewWebhook: %v", err)
	}

	s, h, runID := setupWithParams(t, readDef(t, "approval_gate.json"), json.RawMessage(`{"topic": "seals"}`))
	d := startDispatcher(t, s, h.Queue())
	reg := approvalRegistry(t)
	w, err := engine.New(s, reg, "worker-a", engine.WithDispatchNudge(d.Nudge), engine.WithNotifier(wh))
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	h.Spawn("worker-a", w.Handle, queuetest.LeaseConfig(2*time.Second))

	waitStep(t, s, runID, "approve_publish", "awaiting_human", func(st gen.RunStep) bool {
		return st.Status == store.StepStatusAwaitingHuman
	})
	waitEvent(t, s, runID, store.EventApprovalNotificationFailed)
	if hits, _, _ := rec.snapshot(); hits != 1 {
		t.Errorf("webhook hit %d times, want 1 (no retry on 4xx)", hits)
	}
}
