//go:build integration

package api_test

// Ticket 6.5's lifecycle-endpoint contract tests: cancel, park/unpark, and
// requeue driven purely through the API over a real store with no worker
// fleet — the idle-run shapes where the request transaction alone settles
// everything. The end-to-end round trips (in-flight cancel, park across a
// live fleet, DLQ requeue to completion) live in
// lifecycle_e2e_integration_test.go.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"

	"github.com/mathcslearner/agentloom/internal/api"
	"github.com/mathcslearner/agentloom/internal/store"
)

// submitProbeRun submits the one-noop probe definition and returns its id.
func submitProbeRun(t *testing.T, srv *httptest.Server, key string) string {
	t.Helper()
	var sub api.SubmitRunResponse
	if status := postJSON(t, srv, key, submitBody(t, probeDefJSON, ""), &sub); status != http.StatusCreated {
		t.Fatalf("probe submit = %d, want 201", status)
	}
	return sub.RunID
}

// postOp POSTs a bodyless lifecycle op and decodes the response.
func postOp(t *testing.T, srv *httptest.Server, key, path string, out any) int {
	t.Helper()
	return doAuth(t, srv, http.MethodPost, path, key, nil, out).StatusCode
}

func TestCancelIdleRunFinalizes(t *testing.T) {
	t.Parallel()
	_, srv, key := newServer(t)
	runID := submitProbeRun(t, srv, key)

	// No worker holds a claim, so the request transaction sweeps the ready
	// entry step and finalizes in one shot.
	var res api.CancelRunResponse
	if status := postOp(t, srv, key, "/v1/runs/"+runID+"/cancel", &res); status != http.StatusOK {
		t.Fatalf("POST cancel = %d, want 200", status)
	}
	if !res.Finalized {
		t.Error("idle-run cancel not finalized in the request transaction")
	}
	if len(res.CancelledSteps) != 1 || res.CancelledSteps[0] != "a" {
		t.Errorf("cancelled_steps = %v, want [a]", res.CancelledSteps)
	}
	if res.Run.CancelReason != store.RunCancelReasonManual {
		t.Errorf("cancel_reason = %q, want manual", res.Run.CancelReason)
	}

	var run api.RunResponse
	if status := getJSON(t, srv, key, "/v1/runs/"+runID, &run); status != http.StatusOK {
		t.Fatalf("GET run = %d, want 200", status)
	}
	if run.Run.Status != store.RunStatusCancelled {
		t.Errorf("run status = %q, want cancelled", run.Run.Status)
	}
	if run.Run.StepsCancelled != 1 {
		t.Errorf("steps_cancelled = %d, want 1", run.Run.StepsCancelled)
	}

	// A second cancel is a wrong-state conflict, not a silent no-op.
	var envelope api.ErrorBody
	if status := postOp(t, srv, key, "/v1/runs/"+runID+"/cancel", &envelope); status != http.StatusConflict {
		t.Fatalf("second cancel = %d, want 409", status)
	}
	if envelope.Error.Code != api.ErrCodeConflict {
		t.Errorf("second cancel code = %q, want %q", envelope.Error.Code, api.ErrCodeConflict)
	}
}

func TestParkUnparkCycle(t *testing.T) {
	t.Parallel()
	_, srv, key := newServer(t)
	runID := submitProbeRun(t, srv, key)

	var parked api.ParkRunResponse
	if status := postOp(t, srv, key, "/v1/runs/"+runID+"/park", &parked); status != http.StatusOK {
		t.Fatalf("POST park = %d, want 200", status)
	}
	if parked.Run.Status != store.RunStatusParked || parked.Run.ParkReason != store.ParkReasonManual {
		t.Errorf("parked run = %+v, want parked/manual", parked.Run)
	}

	// Parking a parked run conflicts.
	var envelope api.ErrorBody
	if status := postOp(t, srv, key, "/v1/runs/"+runID+"/park", &envelope); status != http.StatusConflict {
		t.Fatalf("double park = %d, want 409", status)
	}

	var unparked api.UnparkRunResponse
	if status := postOp(t, srv, key, "/v1/runs/"+runID+"/unpark", &unparked); status != http.StatusOK {
		t.Fatalf("POST unpark = %d, want 200", status)
	}
	if unparked.Run.Status != store.RunStatusRunning || unparked.Run.ParkReason != "" {
		t.Errorf("unparked run = %+v, want running with cleared reason", unparked.Run)
	}
	// No dispatcher drained the entry step's outbox row, so it still has a
	// pending dispatch and needs no re-outbox.
	if len(unparked.Dispatched) != 0 {
		t.Errorf("dispatched = %v, want [] (outbox row still pending)", unparked.Dispatched)
	}

	// Unparking a running run conflicts.
	if status := postOp(t, srv, key, "/v1/runs/"+runID+"/unpark", &envelope); status != http.StatusConflict {
		t.Fatalf("unpark running run = %d, want 409", status)
	}
}

func TestSetRunBudget(t *testing.T) {
	t.Parallel()
	_, srv, key := newServer(t)
	runID := submitProbeRun(t, srv, key)

	// Raise the budget on the running probe run.
	var res api.SetBudgetResponse
	resp := doAuth(t, srv, http.MethodPatch, "/v1/runs/"+runID+"/budget", key, []byte(`{"budget_usd": 2.5}`), &res)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PATCH budget = %d, want 200", resp.StatusCode)
	}
	if res.Run.Cost.BudgetUSD == nil || *res.Run.Cost.BudgetUSD != "2.5" {
		t.Errorf("budget_usd = %v, want 2.5", res.Run.Cost.BudgetUSD)
	}
	if res.Run.Cost.BudgetNanoUSD == nil || *res.Run.Cost.BudgetNanoUSD != 2_500_000_000 {
		t.Errorf("budget_nano_usd = %v, want 2_500_000_000", res.Run.Cost.BudgetNanoUSD)
	}
	if res.Run.Cost.OnBudgetExceeded != "park" {
		t.Errorf("on_budget_exceeded = %q, want park (the default)", res.Run.Cost.OnBudgetExceeded)
	}

	// The GET run view reflects the raised budget.
	var run api.RunResponse
	if status := getJSON(t, srv, key, "/v1/runs/"+runID, &run); status != http.StatusOK {
		t.Fatalf("GET run = %d, want 200", status)
	}
	if run.Run.Cost.BudgetNanoUSD == nil || *run.Run.Cost.BudgetNanoUSD != 2_500_000_000 {
		t.Errorf("run view budget = %v, want 2_500_000_000", run.Run.Cost.BudgetNanoUSD)
	}

	// A missing/non-positive budget is a 400.
	var envelope api.ErrorBody
	for _, body := range []string{`{}`, `{"budget_usd": 0}`, `{"budget_usd": -1}`} {
		bad := doAuth(t, srv, http.MethodPatch, "/v1/runs/"+runID+"/budget", key, []byte(body), &envelope)
		if bad.StatusCode != http.StatusBadRequest {
			t.Errorf("PATCH %s = %d, want 400", body, bad.StatusCode)
		}
	}

	// A missing run is a 404.
	miss := doAuth(t, srv, http.MethodPatch, "/v1/runs/"+uuid.NewString()+"/budget", key, []byte(`{"budget_usd": 1}`), &envelope)
	if miss.StatusCode != http.StatusNotFound {
		t.Errorf("PATCH missing run = %d, want 404", miss.StatusCode)
	}
}

func TestLifecycleMisses(t *testing.T) {
	t.Parallel()
	_, srv, key := newServer(t)
	runID := submitProbeRun(t, srv, key)
	ghost := uuid.NewString()

	cases := []struct {
		name, path string
		status     int
		code       string
	}{
		{"cancel bad uuid", "/v1/runs/not-a-uuid/cancel", http.StatusBadRequest, api.ErrCodeInvalidRequest},
		{"cancel unknown run", "/v1/runs/" + ghost + "/cancel", http.StatusNotFound, api.ErrCodeRunNotFound},
		{"park unknown run", "/v1/runs/" + ghost + "/park", http.StatusNotFound, api.ErrCodeRunNotFound},
		{"unpark unknown run", "/v1/runs/" + ghost + "/unpark", http.StatusNotFound, api.ErrCodeRunNotFound},
		{"requeue unknown run", "/v1/runs/" + ghost + "/steps/a/requeue", http.StatusNotFound, api.ErrCodeRunNotFound},
		{"requeue unknown step", "/v1/runs/" + runID + "/steps/ghost/requeue", http.StatusNotFound, api.ErrCodeStepNotFound},
		{"requeue non-dead-lettered step", "/v1/runs/" + runID + "/steps/a/requeue", http.StatusConflict, api.ErrCodeConflict},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var envelope api.ErrorBody
			status := postOp(t, srv, key, tc.path, &envelope)
			if status != tc.status {
				t.Fatalf("status = %d, want %d", status, tc.status)
			}
			if envelope.Error.Code != tc.code {
				t.Errorf("code = %q, want %q", envelope.Error.Code, tc.code)
			}
		})
	}
}

// TestRequeueOnCancelledRunRefused pins the ErrRunNotRequeueable mapping:
// a dead-lettered step of a cancelled run answers 409, never a requeue. The
// dead letter is manufactured with the store's unfenced poison transition —
// the one dead-lettering path that needs no worker.
func TestRequeueOnCancelledRunRefused(t *testing.T) {
	t.Parallel()
	s, srv, key := newServer(t)
	runID := submitProbeRun(t, srv, key)

	id := uuid.MustParse(runID)
	txErr := s.WithTx(t.Context(), func(ctx context.Context, q store.Querier) error {
		_, err := store.PoisonDeadLetterStep(ctx, q, store.PoisonDeadLetterStepArgs{
			RunID: id, StepID: "a", Now: testNow,
		})
		return err
	})
	if txErr != nil {
		t.Fatalf("poison dead-lettering the step: %v", txErr)
	}

	// The run is still running (poison records no disposition here); the
	// cancel finalizes immediately — its only step is already terminal.
	var cancel api.CancelRunResponse
	if status := postOp(t, srv, key, "/v1/runs/"+runID+"/cancel", &cancel); status != http.StatusOK {
		t.Fatalf("POST cancel = %d, want 200", status)
	}
	if !cancel.Finalized {
		t.Fatal("cancel did not finalize a run whose steps are all terminal")
	}

	var envelope api.ErrorBody
	if status := postOp(t, srv, key, "/v1/runs/"+runID+"/steps/a/requeue", &envelope); status != http.StatusConflict {
		t.Fatalf("requeue on cancelled run = %d, want 409", status)
	}
	if envelope.Error.Code != api.ErrCodeConflict {
		t.Errorf("code = %q, want %q", envelope.Error.Code, api.ErrCodeConflict)
	}
}
