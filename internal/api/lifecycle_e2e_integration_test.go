//go:build integration

package api_test

// Ticket 6.5's acceptance round trips: DLQ requeue, cancel, and
// park/unpark driven through the API against the production dispatcher and
// a live worker fleet — the same composition as 4.6's headline test.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/mathcslearner/agentloom/internal/api"
	"github.com/mathcslearner/agentloom/internal/engine"
	"github.com/mathcslearner/agentloom/internal/exec"
	"github.com/mathcslearner/agentloom/internal/queue"
	"github.com/mathcslearner/agentloom/internal/queue/queuetest"
	"github.com/mathcslearner/agentloom/internal/store"
	"github.com/mathcslearner/agentloom/internal/store/storetest"
)

// fleet is the API + dispatcher + two-worker composition the round trips
// run against.
type fleet struct {
	store *store.Store
	srv   *httptest.Server
	key   string
	h     *queuetest.Harness
}

// newFleet boots the full stack. Worker engines poll for run cancellation
// every 50ms so the cancel round trip converges promptly.
func newFleet(t *testing.T) fleet {
	t.Helper()
	ctx := t.Context()
	s := store.NewFromPool(storetest.NewDB(t))
	h := queuetest.New(t)
	h.EnsureGroup(ctx)
	rootKey := mintTestKey(t)
	handler, err := api.New(s, time.Now, nil, rootKey, api.RateLimitOptions{})
	if err != nil {
		t.Fatalf("api.New: %v", err)
	}
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	d, err := engine.NewDispatcher(s, h.Queue(), engine.DispatcherConfig{
		Interval: 10 * time.Millisecond, Batch: 16,
	})
	if err != nil {
		t.Fatalf("NewDispatcher: %v", err)
	}
	dctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	t.Cleanup(func() { cancel(); <-done })
	go func() { defer close(done); d.Run(dctx) }()
	for _, name := range []string{"worker-a", "worker-b"} {
		eng, err := engine.New(s, exec.Builtins(), name,
			engine.WithDispatchNudge(d.Nudge),
			engine.WithRetryScheduler(h.Delayed()),
			engine.WithCancelPollInterval(50*time.Millisecond))
		if err != nil {
			t.Fatalf("engine.New: %v", err)
		}
		// A fast promoter so delayed retries fire on their real backoff.
		h.Spawn(name, eng.Handle, queue.ConsumerConfig{
			Block: 500 * time.Millisecond, Batch: 1,
			PromoterTick: 50 * time.Millisecond,
		})
	}
	return fleet{store: s, srv: srv, key: rootKey, h: h}
}

// watchRun polls GET /v1/runs/{id} until pred is satisfied or a 15s
// deadline passes, returning the last response.
func (f fleet) watchRun(t *testing.T, runID string, pred func(api.RunResponse) bool) api.RunResponse {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	var run api.RunResponse
	for {
		if getJSON(t, f.srv, f.key, "/v1/runs/"+runID, &run) != http.StatusOK {
			t.Fatal("GET run failed mid-watch")
		}
		if pred(run) {
			return run
		}
		if time.Now().After(deadline) {
			t.Fatalf("run never reached the awaited state; last:\n%+v", run)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func (f fleet) submit(t *testing.T, def []byte) string {
	t.Helper()
	var sub api.SubmitRunResponse
	if status := postJSON(t, f.srv, f.key, submitBody(t, def, ""), &sub); status != http.StatusCreated {
		t.Fatalf("submit = %d, want 201", status)
	}
	return sub.RunID
}

func stepView(t *testing.T, run api.RunResponse, id string) api.StepView {
	t.Helper()
	s, ok := stepFrom(run, id)
	if !ok {
		t.Fatalf("step %s missing from response", id)
	}
	return s
}

// TestRequeueDeadLetteredStepViaAPI is the DLQ half of the acceptance
// criterion: a step exhausts its retry budget into the DLQ, the API
// surfaces the dead letter, POST requeue re-arms the budget, and the run
// executes to success (fail_n_times keys off the durable attempt number,
// so attempt 3 passes).
func TestRequeueDeadLetteredStepViaAPI(t *testing.T) {
	t.Parallel()
	f := newFleet(t)
	def := []byte(`{
		"schema_version": 1,
		"name": "requeue-roundtrip",
		"steps": [
			{"id": "flaky", "type": "fail_n_times", "config": {"n": 2},
			 "retry": {"max_attempts": 2, "backoff": {"initial": "50ms", "cap": "100ms", "multiplier": 2}, "jitter": "full", "retry_on": ["transient"]}},
			{"id": "done", "type": "noop"}
		],
		"edges": [{"from": "flaky", "to": "done"}]
	}`)
	runID := f.submit(t, def)

	// The 2-attempt budget exhausts and fail_fast lands the run failed.
	run := f.watchRun(t, runID, func(r api.RunResponse) bool {
		return r.Run.Status == store.RunStatusFailed
	})
	if got := stepView(t, run, "flaky").Status; got != store.StepStatusDeadLettered {
		t.Fatalf("flaky status = %q, want dead_lettered", got)
	}
	if len(run.DeadLetters) != 1 || run.DeadLetters[0].StepID != "flaky" ||
		run.DeadLetters[0].Source != store.DeadLetterSourceRetriesExhausted {
		t.Fatalf("dead_letters = %+v, want one retries_exhausted record for flaky", run.DeadLetters)
	}

	var requeued api.RequeueStepResponse
	if status := postOp(t, f.srv, f.key, "/v1/runs/"+runID+"/steps/flaky/requeue", &requeued); status != http.StatusOK {
		t.Fatalf("POST requeue = %d, want 200", status)
	}
	if !requeued.RunResumed || requeued.Status != store.StepStatusReady {
		t.Errorf("requeue response = %+v, want run_resumed with the step ready", requeued)
	}

	run = f.watchRun(t, runID, func(r api.RunResponse) bool {
		return r.Run.Status == store.RunStatusSucceeded
	})
	flaky := stepView(t, run, "flaky")
	if flaky.Status != store.StepStatusSucceeded || len(flaky.Attempts) != 3 {
		t.Errorf("flaky = %q with %d attempts, want succeeded on attempt 3", flaky.Status, len(flaky.Attempts))
	}
	if got := stepView(t, run, "done").Status; got != store.StepStatusSucceeded {
		t.Errorf("done status = %q, want succeeded", got)
	}
	if run.Run.StepsFailed != 0 {
		t.Errorf("steps_failed = %d, want 0 after the cure", run.Run.StepsFailed)
	}
	f.h.WaitQuiescent(t.Context())
}

// TestCancelInFlightRunViaAPI: an executor mid-sleep is interrupted by the
// cancellation watch after POST cancel, the run converges to cancelled,
// and the queue quiesces.
func TestCancelInFlightRunViaAPI(t *testing.T) {
	t.Parallel()
	f := newFleet(t)
	def := []byte(`{
		"schema_version": 1,
		"name": "cancel-roundtrip",
		"steps": [
			{"id": "nap", "type": "sleep", "config": {"duration": "30s"}},
			{"id": "after", "type": "noop"}
		],
		"edges": [{"from": "nap", "to": "after"}]
	}`)
	runID := f.submit(t, def)

	// Wait until a worker actually holds the step (attempt open) so the
	// cancel exercises the in-flight path, not the idle sweep.
	f.watchRun(t, runID, func(r api.RunResponse) bool {
		s, ok := stepFrom(r, "nap")
		return ok && s.Status == store.StepStatusRunning
	})

	var res api.CancelRunResponse
	if status := postOp(t, f.srv, f.key, "/v1/runs/"+runID+"/cancel", &res); status != http.StatusOK {
		t.Fatalf("POST cancel = %d, want 200", status)
	}
	if res.Finalized {
		t.Error("cancel of an in-flight run reported finalized — the worker still held the step")
	}

	run := f.watchRun(t, runID, func(r api.RunResponse) bool {
		return r.Run.Status == store.RunStatusCancelled
	})
	nap := stepView(t, run, "nap")
	if nap.Status != store.StepStatusCancelled {
		t.Errorf("nap status = %q, want cancelled", nap.Status)
	}
	if len(nap.Attempts) != 1 || nap.Attempts[0].Outcome != store.StepStatusCancelled {
		t.Errorf("nap attempts = %+v, want exactly [cancelled]", nap.Attempts)
	}
	if got := stepView(t, run, "after").Status; got != store.StepStatusCancelled {
		t.Errorf("after status = %q, want cancelled (swept)", got)
	}
	f.h.WaitQuiescent(t.Context())
}

// TestParkUnparkRoundTripViaAPI: a parked run's in-flight step settles and
// its successor strands ready (delivery consumed by the claim guard); the
// unpark re-outboxes exactly that successor and the run completes.
func TestParkUnparkRoundTripViaAPI(t *testing.T) {
	t.Parallel()
	f := newFleet(t)
	def := []byte(`{
		"schema_version": 1,
		"name": "park-roundtrip",
		"steps": [
			{"id": "nap", "type": "sleep", "config": {"duration": "500ms"}},
			{"id": "after", "type": "noop"}
		],
		"edges": [{"from": "nap", "to": "after"}]
	}`)
	runID := f.submit(t, def)

	// Park while nap is in flight.
	f.watchRun(t, runID, func(r api.RunResponse) bool {
		s, ok := stepFrom(r, "nap")
		return ok && s.Status == store.StepStatusRunning
	})
	if status := postOp(t, f.srv, f.key, "/v1/runs/"+runID+"/park", nil); status != http.StatusOK {
		t.Fatalf("POST park = %d, want 200", status)
	}

	// The in-flight completion proceeds (park pauses dispatch, not
	// settlement), readying "after"; its delivery bounces off the claim
	// guard. Wait until it is stranded: ready, zero attempts, no pending
	// outbox row.
	f.watchRun(t, runID, func(r api.RunResponse) bool {
		s, ok := stepFrom(r, "after")
		return ok && s.Status == store.StepStatusReady && r.Run.Status == store.RunStatusParked
	})
	deadline := time.Now().Add(10 * time.Second)
	for {
		tasks, err := f.store.Outbox().List(t.Context(), 10)
		if err != nil {
			t.Fatalf("listing outbox: %v", err)
		}
		if len(tasks) == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("outbox never drained while parked: %+v", tasks)
		}
		time.Sleep(20 * time.Millisecond)
	}
	var afterView api.RunResponse
	if getJSON(t, f.srv, f.key, "/v1/runs/"+runID, &afterView) != http.StatusOK {
		t.Fatal("GET run failed")
	}
	if got := stepView(t, afterView, "after"); len(got.Attempts) != 0 {
		t.Errorf("parked successor has %d attempts, want 0 — the fleet claimed a parked run's step", len(got.Attempts))
	}

	// Unpark re-dispatches exactly the stranded successor.
	var unparked api.UnparkRunResponse
	if status := postOp(t, f.srv, f.key, "/v1/runs/"+runID+"/unpark", &unparked); status != http.StatusOK {
		t.Fatalf("POST unpark = %d, want 200", status)
	}
	if len(unparked.Dispatched) != 1 || unparked.Dispatched[0] != "after" {
		t.Errorf("dispatched = %v, want [after]", unparked.Dispatched)
	}

	run := f.watchRun(t, runID, func(r api.RunResponse) bool {
		return r.Run.Status == store.RunStatusSucceeded
	})
	if got := stepView(t, run, "after").Status; got != store.StepStatusSucceeded {
		t.Errorf("after status = %q, want succeeded", got)
	}
	f.h.WaitQuiescent(t.Context())
}
