//go:build integration

package api_test

// Ops-views integration (ticket 18.6): the cross-run dead-letter list, the
// queue-health system stats, and whoami — driven through the API against the
// production dispatcher, a live worker fleet, and a real Redis queue (the
// newFleet composition, extended with the queue-introspection seam).

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/mathcslearner/agentloom/internal/api"
	"github.com/mathcslearner/agentloom/internal/queue"
	"github.com/mathcslearner/agentloom/internal/store"
)

// deffmt fills the dead-lettering definition template with a unique name.
func deffmt(name string) string { return fmt.Sprintf(dlqDef, name) }

// fleetQueueStats is the test adapter onto the queue behind /v1/system/stats —
// the api.QueueIntrospector the fleet wires (the cmd/api adapter's twin). Every
// call is a read; a not-yet-bootstrapped group reports zeros, not an error.
type fleetQueueStats struct {
	q       *queue.Queue
	delayed *queue.Delayed
	idle    time.Duration
}

func (a fleetQueueStats) QueueStats(ctx context.Context) (api.QueueStatsView, error) {
	view := api.QueueStatsView{Stream: a.q.Stream(), Group: a.q.Group(), Workers: []api.ConsumerView{}}
	stats, err := a.q.Stats(ctx)
	switch {
	case errors.Is(err, queue.ErrNoGroup):
		view.LagKnown = true
		return view, nil
	case err != nil:
		return api.QueueStatsView{}, err
	}
	view.Length = stats.Length
	view.Pending = stats.Pending
	view.ReadyDepth = stats.ReadyDepth()
	view.LagKnown = stats.Lag >= 0
	if n, err := a.delayed.Len(ctx); err == nil {
		view.Delayed = n
	} else {
		return api.QueueStatsView{}, err
	}
	consumers, err := a.q.ListConsumers(ctx)
	if err != nil && !errors.Is(err, queue.ErrNoGroup) {
		return api.QueueStatsView{}, err
	}
	for _, c := range consumers {
		active := c.Idle <= a.idle
		if active {
			view.WorkersActive++
		}
		view.Workers = append(view.Workers, api.ConsumerView{
			ID: c.Name, IdleMs: c.Idle.Milliseconds(), Pending: c.Pending, Active: active,
		})
	}
	return view, nil
}

// dlqDef is a definition whose first step exhausts a 2-attempt budget into the
// DLQ (retries_exhausted), landing the fail_fast run failed. fail_n_times keys
// off the durable attempt number, so a later requeue's attempt 3 passes.
const dlqDef = `{
	"schema_version": 1,
	"name": "%s",
	"steps": [
		{"id": "flaky", "type": "fail_n_times", "config": {"n": 2},
		 "retry": {"max_attempts": 2, "backoff": {"initial": "50ms", "cap": "100ms", "multiplier": 2}, "jitter": "full", "retry_on": ["transient"]}},
		{"id": "done", "type": "noop"}
	],
	"edges": [{"from": "flaky", "to": "done"}]
}`

// TestListDeadLettersAcrossRuns: two runs dead-letter a step; GET /v1/dead-letters
// lists both open, filters by run and source, and a requeue-then-succeed moves
// the step out of the open set while status=all still shows the death.
func TestListDeadLettersAcrossRuns(t *testing.T) {
	t.Parallel()
	f := newFleet(t)

	runA := f.submit(t, []byte(deffmt("dlq-a")))
	runB := f.submit(t, []byte(deffmt("dlq-b")))
	for _, id := range []string{runA, runB} {
		f.watchRun(t, id, func(r api.RunResponse) bool { return r.Run.Status == store.RunStatusFailed })
	}

	// status=open (default) lists both, newest-first, with live context.
	var page api.DeadLetterListResponse
	if s := getJSON(t, f.srv, f.key, "/v1/dead-letters", &page); s != http.StatusOK {
		t.Fatalf("GET /v1/dead-letters = %d", s)
	}
	if len(page.DeadLetters) != 2 {
		t.Fatalf("open dead letters = %d, want 2", len(page.DeadLetters))
	}
	for _, d := range page.DeadLetters {
		if d.StepID != "flaky" || d.Source != store.DeadLetterSourceRetriesExhausted ||
			d.StepStatus != store.StepStatusDeadLettered || !d.Open || d.StepType != "fail_n_times" {
			t.Fatalf("dead letter = %+v, want an open retries_exhausted flaky", d)
		}
	}

	// run_id filter narrows to one run.
	if s := getJSON(t, f.srv, f.key, "/v1/dead-letters?run_id="+runA, &page); s != http.StatusOK {
		t.Fatalf("filtered GET = %d", s)
	}
	if len(page.DeadLetters) != 1 || page.DeadLetters[0].RunID != runA {
		t.Fatalf("run_id filter = %+v, want only runA", page.DeadLetters)
	}

	// source filter with a non-matching source is empty (not an error).
	if s := getJSON(t, f.srv, f.key, "/v1/dead-letters?source=poison", &page); s != http.StatusOK {
		t.Fatalf("source filter = %d", s)
	}
	if len(page.DeadLetters) != 0 {
		t.Fatalf("poison filter = %d rows, want 0", len(page.DeadLetters))
	}

	// A bad status is a 400.
	if s := getJSON(t, f.srv, f.key, "/v1/dead-letters?status=bogus", &page); s != http.StatusBadRequest {
		t.Fatalf("bad status = %d, want 400", s)
	}

	// Requeue runA's step; it succeeds on attempt 3, leaving the open set.
	var requeued api.RequeueStepResponse
	if s := postOp(t, f.srv, f.key, "/v1/runs/"+runA+"/steps/flaky/requeue", &requeued); s != http.StatusOK {
		t.Fatalf("requeue = %d", s)
	}
	f.watchRun(t, runA, func(r api.RunResponse) bool { return r.Run.Status == store.RunStatusSucceeded })

	if s := getJSON(t, f.srv, f.key, "/v1/dead-letters?status=open", &page); s != http.StatusOK {
		t.Fatalf("open after requeue = %d", s)
	}
	if len(page.DeadLetters) != 1 || page.DeadLetters[0].RunID != runB {
		t.Fatalf("open after requeue = %+v, want only runB still open", page.DeadLetters)
	}
	// status=all still shows the (now closed) runA death.
	if s := getJSON(t, f.srv, f.key, "/v1/dead-letters?status=all&run_id="+runA, &page); s != http.StatusOK {
		t.Fatalf("all filter = %d", s)
	}
	if len(page.DeadLetters) != 1 || page.DeadLetters[0].Open {
		t.Fatalf("all/runA = %+v, want one closed death", page.DeadLetters)
	}
	f.h.WaitQuiescent(t.Context())
}

// TestSystemStats: with a wired queue introspector the endpoint reports the live
// queue block (stream/group, workers) and the Postgres backlog. A dead-lettered
// run bumps the open DLQ count.
func TestSystemStats(t *testing.T) {
	t.Parallel()
	f := newFleet(t)

	runID := f.submit(t, []byte(deffmt("dlq-stats")))
	f.watchRun(t, runID, func(r api.RunResponse) bool { return r.Run.Status == store.RunStatusFailed })

	var stats api.SystemStatsResponse
	if s := getJSON(t, f.srv, f.key, "/v1/system/stats", &stats); s != http.StatusOK {
		t.Fatalf("GET /v1/system/stats = %d", s)
	}
	if stats.Queue == nil {
		t.Fatalf("queue is null, want a live queue block; queue_error=%q", stats.QueueError)
	}
	if stats.Queue.Stream != f.h.Queue().Stream() || stats.Queue.Group != f.h.Queue().Group() {
		t.Errorf("queue stream/group = %s/%s, want %s/%s",
			stats.Queue.Stream, stats.Queue.Group, f.h.Queue().Stream(), f.h.Queue().Group())
	}
	// The two fleet workers are registered consumers.
	if len(stats.Queue.Workers) < 2 {
		t.Errorf("workers = %d, want >= 2 (the fleet's consumers)", len(stats.Queue.Workers))
	}
	if stats.DeadLetters.Open < 1 {
		t.Errorf("dead_letters.open = %d, want >= 1", stats.DeadLetters.Open)
	}
	if stats.ObservedAt.IsZero() {
		t.Error("observed_at is zero")
	}
	f.h.WaitQuiescent(t.Context())
}

// TestWhoAmI: the root credential and a scoped key each report their own id and
// scopes.
func TestWhoAmI(t *testing.T) {
	t.Parallel()
	f := newFleet(t)

	var me api.WhoAmIResponse
	if s := getJSON(t, f.srv, f.key, "/v1/auth/whoami", &me); s != http.StatusOK {
		t.Fatalf("whoami (root) = %d", s)
	}
	if me.KeyID != "root" || len(me.Scopes) != 1 || me.Scopes[0] != string(api.ScopeAdmin) {
		t.Errorf("root whoami = %+v, want key_id root with [admin]", me)
	}

	// Mint a read-only key and confirm it reports exactly its scope.
	created := createKey(t, f.srv, f.key, api.CreateKeyRequest{
		Name: "reader", Scopes: []string{string(api.ScopeRead)},
	})
	var readMe api.WhoAmIResponse
	if s := getJSON(t, f.srv, created.Key, "/v1/auth/whoami", &readMe); s != http.StatusOK {
		t.Fatalf("whoami (reader) = %d", s)
	}
	if readMe.KeyID != created.ID || len(readMe.Scopes) != 1 || readMe.Scopes[0] != string(api.ScopeRead) {
		t.Errorf("reader whoami = %+v, want its own id with [read]", readMe)
	}
}
