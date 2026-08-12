//go:build integration

package crash

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mathcslearner/agentloom/internal/store"
)

// Graceful shutdown & drain, against real processes and real SIGTERM
// (ticket 5.7). The SIGKILL suite proves recovery from sudden death; this
// one proves the opposite guarantee — that a *graceful* restart needs no
// recovery at all: a SIGTERMed worker finishes what it holds, acks, exits
// clean, and leaves nothing for reclaim. Plus the bounded half: a drain
// deadline abandons an unfinishable step to the crash path on purpose.

// countEventsOfType counts a run's events of the given type across all
// steps — the zero-reclaim-churn probe.
func countEventsOfType(t *testing.T, s *store.Store, runID uuid.UUID, eventType string) int {
	t.Helper()
	events, err := s.Events().List(context.Background(), runID, 0, 1000)
	if err != nil {
		t.Fatalf("listing events of %s: %v", runID, err)
	}
	n := 0
	for _, ev := range events {
		if ev.Type == eventType {
			n++
		}
	}
	return n
}

// loadRun is one continuously-submitted run with its counter files.
type loadRun struct {
	id       uuid.UUID
	counters map[string]string
}

// TestRollingRestartUnderLoadZeroChurn is acceptance criterion 1: a
// rolling restart of a two-worker fleet under continuous load loses no
// runs and causes zero reclaim churn. A submitter instantiates a
// counter → sleep → counter run every 250ms throughout; each worker is
// SIGTERMed and replaced in turn. At quiescence every run has succeeded,
// every step's history is exactly one succeeded attempt (no lost, no
// retry), no step_reclaimed event exists anywhere, every counter file
// holds exactly one line, and both drained workers exited 0 after logging
// their drain summaries.
func TestRollingRestartUnderLoadZeroChurn(t *testing.T) {
	t.Parallel()

	s, h, env := setupWorld(t)
	// This scenario kills no one, so reclaim latency is irrelevant — but
	// a CI stall could push a delivered-yet-queued batch entry past a 2s
	// lease TTL and trigger a spurious (harmless, churn-free) reclaim. A
	// 5s TTL keeps the zero-churn assertion honest about what it measures.
	env["AGENTLOOM_QUEUE_LEASE_TTL"] = "5s"
	// Batch 4 so the drain's batch-remainder path runs end-to-end: a
	// SIGTERM mid-batch must finish the queued entries too, not just the
	// in-flight one.
	env["AGENTLOOM_QUEUE_CONSUMER_BATCH"] = "4"

	dir := t.TempDir()
	var (
		mu   sync.Mutex
		runs []loadRun
	)
	snapshot := func() []loadRun {
		mu.Lock()
		defer mu.Unlock()
		return append([]loadRun(nil), runs...)
	}
	// succeeded counts terminal runs, failing loudly if any run reached a
	// non-succeeded terminal state (a lost run, dead-letter, or cancel —
	// all bugs here).
	succeeded := func() int {
		t.Helper()
		n := 0
		for _, r := range snapshot() {
			run, err := s.Runs().Get(context.Background(), r.id)
			if err != nil {
				t.Fatalf("reading run %s: %v", r.id, err)
			}
			switch run.Status {
			case store.RunStatusSucceeded:
				n++
			case store.RunStatusRunning:
			default:
				t.Fatalf("run %s reached %q under rolling restart:\n%s", r.id, run.Status, dumpSteps(t, s, r.id))
			}
		}
		return n
	}

	stopSubmit := make(chan struct{})
	submitDone := make(chan struct{})
	go func() {
		defer close(submitDone)
		for i := 0; ; i++ {
			select {
			case <-stopSubmit:
				return
			case <-time.After(250 * time.Millisecond):
			}
			counters := map[string]string{
				"before": filepath.Join(dir, fmt.Sprintf("run%d-before.log", i)),
				"after":  filepath.Join(dir, fmt.Sprintf("run%d-after.log", i)),
			}
			id, err := newRun(s, crashDef(t, "300ms", counters))
			if err != nil {
				t.Errorf("submitter: %v", err) // Errorf is goroutine-safe; Fatalf is not
				return
			}
			mu.Lock()
			runs = append(runs, loadRun{id: id, counters: counters})
			mu.Unlock()
		}
	}()
	defer func() { // stop the load before any fatal unwinds the world
		select {
		case <-stopSubmit:
		default:
			close(stopSubmit)
		}
		<-submitDone
	}()

	workers := []*workerProc{
		spawnWorker(t, "worker-a", env),
		spawnWorker(t, "worker-b", env),
	}

	// One worker at a time: SIGTERM, assert a clean narrated drain,
	// replace, let load flow between the restarts — the rolling restart.
	waitFor(t, "first runs succeeding under load", func() bool { return succeeded() >= 3 })
	for i, name := range []string{"worker-a2", "worker-b2"} {
		old := workers[i]
		// SIGTERM only once the victim provably holds at least one lease,
		// so the drain has real in-flight work to finish — a SIGTERM to an
		// idle worker would prove nothing about draining.
		waitFor(t, old.name+" holding a lease under load", func() bool {
			for _, p := range pendingEntries(t, h) {
				if p.Consumer == old.id() {
					return true
				}
			}
			return false
		})
		old.terminate()
		if !old.logged("shutdown drain complete") {
			old.dumpOutput()
			t.Fatalf("%s exited without logging its drain summary", old.name)
		}
		workers[i] = spawnWorker(t, name, env)
		before := succeeded()
		waitFor(t, "load flowing after restarting "+old.name, func() bool { return succeeded() >= before+3 })
	}

	close(stopSubmit)
	<-submitDone
	all := snapshot()
	if len(all) < 10 {
		t.Fatalf("only %d runs were submitted; the scenario needs sustained load", len(all))
	}
	t.Logf("rolling restart complete: %d runs submitted", len(all))
	waitFor(t, "all submitted runs succeeded", func() bool { return succeeded() == len(all) })

	// Zero churn, zero loss, zero duplicate effects — for every run.
	for _, r := range all {
		if n := countEventsOfType(t, s, r.id, "step_reclaimed"); n != 0 {
			t.Errorf("run %s has %d step_reclaimed events, want 0 (reclaim churn from a drained worker)", r.id, n)
		}
		for _, step := range []string{"before", "nap", "after"} {
			requireAttempts(t, s, r.id, step, []string{"succeeded"})
		}
		requireCounterFiredOnce(t, r.counters["before"], "before")
		requireCounterFiredOnce(t, r.counters["after"], "after")
	}
	requireOutboxEmpty(t, s)
	h.WaitQuiescent(context.Background())
}

// TestDrainTimeoutAbandonedStepReclaimed is acceptance criterion 2: a
// drain deadline shorter than the in-flight step abandons it — the worker
// still exits 0, well before the step could have finished, logging the
// abandoned step's disposition — and the abandoned lease expires
// naturally into the survivor's reclaim/takeover: attempt history
// lost → succeeded, exactly one step_reclaimed event, effects fired once.
func TestDrainTimeoutAbandonedStepReclaimed(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	counters := map[string]string{
		"before": filepath.Join(dir, "before.log"),
		"after":  filepath.Join(dir, "after.log"),
	}
	// 6s sleep against a 500ms drain budget: the drain can only abandon.
	s, h, env, runID := setupRun(t, crashDef(t, "6s", counters))
	env["AGENTLOOM_WORKER_DRAIN_TIMEOUT"] = "500ms"

	workers := []*workerProc{
		spawnWorker(t, "worker-a", env),
		spawnWorker(t, "worker-b", env),
	}

	var holder string
	waitFor(t, "nap running with a known lease holder", func() bool {
		if stepStatus(t, s, runID, "nap") != store.StepStatusRunning {
			return false
		}
		holder = leaseHolder(t, h, "nap")
		return holder != ""
	})
	victim := byID(t, workers, holder)
	t.Logf("SIGTERMing %s (worker_id %s) mid-sleep with a 500ms drain budget", victim.name, holder)

	start := time.Now()
	victim.terminate()
	if elapsed := time.Since(start); elapsed >= 6*time.Second {
		t.Errorf("victim took %v to exit — it finished the sleep instead of abandoning at the drain deadline", elapsed)
	}
	for _, want := range []string{
		"shutdown abandoned in-flight step",
		"aborted at the drain deadline",
		"shutdown drain complete",
	} {
		if !victim.logged(want) {
			victim.dumpOutput()
			t.Fatalf("victim's shutdown sequence never logged %q", want)
		}
	}

	// The survivor reclaims the abandoned lease after TTL expiry, takes
	// the step over, re-executes, and completes the run.
	waitRunSucceeded(t, s, runID)
	requireAttempts(t, s, runID, "nap", []string{"lost", "succeeded"})
	if n := countStepEvents(t, s, runID, "step_reclaimed", "nap"); n != 1 {
		t.Errorf("nap has %d step_reclaimed events, want exactly 1", n)
	}
	for _, step := range []string{"before", "after"} {
		requireAttempts(t, s, runID, step, []string{"succeeded"})
		requireCounterFiredOnce(t, counters[step], step)
	}
	requireOutboxEmpty(t, s)
	h.WaitQuiescent(context.Background())
}
