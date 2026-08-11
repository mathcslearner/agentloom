//go:build integration

package crash

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/mathcslearner/agentloom/internal/store"
)

// crashDef is the SIGKILL scenario's workflow: two counter steps (each
// appending to its own file — the external side-effect ledger) bracketing
// a sleep long enough to guarantee the kill lands mid-execution. The
// counters prove effects fired exactly once; the sleep is the crash
// target (it is idempotent by nature, so its re-execution is safe).
func crashDef(t *testing.T, sleep string, counterPaths map[string]string) string {
	t.Helper()
	path := func(step string) string {
		b, err := json.Marshal(counterPaths[step])
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	return fmt.Sprintf(`{
		"schema_version": 1,
		"name": "crash-recovery",
		"steps": [
			{"id": "before", "type": "counter", "config": {"path": %s}},
			{"id": "nap", "type": "sleep", "config": {"duration": %q}},
			{"id": "after", "type": "counter", "config": {"path": %s}}
		],
		"edges": [
			{"from": "before", "to": "nap"},
			{"from": "nap", "to": "after"}
		]
	}`, path("before"), sleep, path("after"))
}

// TestWorkerSIGKILLMidStepRecovered is the flagship guarantee (crash
// matrix W3), against real processes: two workers run a counter → sleep →
// counter chain; the worker holding the sleep step's lease is SIGKILLed
// mid-sleep. The survivor must reclaim the entry after lease expiry, take
// over the step (running → ready → running under a fresh claim), re-execute
// it, and complete the run — with the attempt history reading lost →
// succeeded, the reclaim visible in the event log, successors dispatched
// exactly once, and every counter file holding exactly one line.
func TestWorkerSIGKILLMidStepRecovered(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	counters := map[string]string{
		"before": filepath.Join(dir, "before.log"),
		"after":  filepath.Join(dir, "after.log"),
	}
	// 4s: long enough that observing `running` and delivering SIGKILL
	// (~tens of ms later) always lands mid-sleep, short enough to keep the
	// survivor's full re-execution inside the scenario budget.
	s, h, env, runID := setupRun(t, crashDef(t, "4s", counters))

	workers := []*workerProc{
		spawnWorker(t, "worker-a", env),
		spawnWorker(t, "worker-b", env),
	}

	// Wait until the sleep step is running AND its PEL entry names a
	// holder — then that consumer is provably the worker mid-sleep.
	var holder string
	waitFor(t, "nap running with a known lease holder", func() bool {
		if stepStatus(t, s, runID, "nap") != store.StepStatusRunning {
			return false
		}
		holder = leaseHolder(t, h, "nap")
		return holder != ""
	})
	victim := byID(t, workers, holder)
	t.Logf("SIGKILLing %s (worker_id %s) mid-sleep", victim.name, holder)
	victim.kill()

	// The survivor reclaims after lease expiry (~1.5×TTL), takes over,
	// re-executes the sleep, and drives the run home.
	waitRunSucceeded(t, s, runID)
	h.WaitQuiescent(t.Context())

	// Attempt history proves the reclaim: attempt 1 (the dead worker's
	// claim) closed as lost, attempt 2 (a fresh claim) succeeded.
	requireAttempts(t, s, runID, "nap", []string{store.AttemptOutcomeLost, store.StepStatusSucceeded})
	requireAttempts(t, s, runID, "before", []string{store.StepStatusSucceeded})
	requireAttempts(t, s, runID, "after", []string{store.StepStatusSucceeded})
	if got := countStepEvents(t, s, runID, store.EventStepReclaimed, "nap"); got != 1 {
		t.Errorf("step_reclaimed(nap) events = %d, want exactly 1", got)
	}

	// No double effects and no double dispatch: each counter's file has
	// exactly one line, each step succeeded exactly once, and the
	// successor of the crashed step was readied exactly once.
	requireCounterFiredOnce(t, counters["before"], "before")
	requireCounterFiredOnce(t, counters["after"], "after")
	for _, stepID := range []string{"before", "nap", "after"} {
		if got := countStepEvents(t, s, runID, store.EventStepSucceeded, stepID); got != 1 {
			t.Errorf("step_succeeded(%s) events = %d, want exactly 1", stepID, got)
		}
		if got := countStepEvents(t, s, runID, store.EventStepReady, stepID); got != 1 {
			t.Errorf("step_ready(%s) events = %d, want exactly 1", stepID, got)
		}
	}
	requireOutboxEmpty(t, s)
}

// restartDef is the restart scenario's workflow: a longer chain whose
// counters checkpoint progress, so the test can prove pre-crash steps were
// not re-run after the fleet restarts.
func restartDef(t *testing.T, counterPaths map[string]string) string {
	t.Helper()
	path := func(step string) string {
		b, err := json.Marshal(counterPaths[step])
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	return fmt.Sprintf(`{
		"schema_version": 1,
		"name": "restart-resume",
		"steps": [
			{"id": "c1", "type": "counter", "config": {"path": %s}},
			{"id": "nap1", "type": "sleep", "config": {"duration": "500ms"}},
			{"id": "c2", "type": "counter", "config": {"path": %s}},
			{"id": "nap2", "type": "sleep", "config": {"duration": "4s"}},
			{"id": "c3", "type": "counter", "config": {"path": %s}}
		],
		"edges": [
			{"from": "c1", "to": "nap1"},
			{"from": "nap1", "to": "c2"},
			{"from": "c2", "to": "nap2"},
			{"from": "nap2", "to": "c3"}
		]
	}`, path("c1"), path("c2"), path("c3"))
}

// TestFullFleetRestartResumesFromLastCompletedStep kills the ENTIRE worker
// fleet (SIGKILL, both processes) mid-run and starts a fresh fleet: the
// run must freeze while no workers exist — lease expiry alone moves
// nothing — then resume from the last completed step. Fresh consumer
// names are ADR-005's crash model: the new fleet reclaims the dead
// consumers' PEL entries via XAUTOCLAIM, takes over the mid-flight step,
// and never re-runs completed work.
func TestFullFleetRestartResumesFromLastCompletedStep(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	counters := map[string]string{
		"c1": filepath.Join(dir, "c1.log"),
		"c2": filepath.Join(dir, "c2.log"),
		"c3": filepath.Join(dir, "c3.log"),
	}
	s, h, env, runID := setupRun(t, restartDef(t, counters))

	first := []*workerProc{
		spawnWorker(t, "first-a", env),
		spawnWorker(t, "first-b", env),
	}

	// Let the run make real progress (c2 done proves nap1 → c2 completed),
	// then catch it mid-second-sleep and kill the whole fleet.
	waitFor(t, "c2 succeeded and nap2 running", func() bool {
		return stepStatus(t, s, runID, "c2") == store.StepStatusSucceeded &&
			stepStatus(t, s, runID, "nap2") == store.StepStatusRunning
	})
	for _, w := range first {
		w.kill()
	}

	// With no workers alive, expiry alone must not move state: wait until
	// the orphaned entry's idle time provably exceeds the lease TTL, then
	// assert nothing changed — the step is still running under the dead
	// claim, with its single attempt open.
	waitFor(t, "nap2's orphaned lease to expire", func() bool {
		for _, p := range pendingEntries(t, h) {
			if p.StepID == "nap2" && p.Idle > leaseTTL {
				return true
			}
		}
		return false
	})
	if got := stepStatus(t, s, runID, "nap2"); got != store.StepStatusRunning {
		t.Fatalf("nap2 status = %q after fleet death, want still running (nothing recovers without a worker)", got)
	}
	requireAttempts(t, s, runID, "nap2", []string{""}) // one open attempt, no outcome yet

	// A fresh fleet — new consumer names, empty PELs — must pick the run
	// back up: reclaim nap2's entry from the dead consumers, take over,
	// re-execute, and finish the chain.
	spawnWorker(t, "second-a", env)
	spawnWorker(t, "second-b", env)
	waitRunSucceeded(t, s, runID)
	h.WaitQuiescent(t.Context())

	// Resume-from-last-completed, not re-run: pre-crash steps keep their
	// single attempt and single counter line; only the mid-flight step
	// shows the lost → succeeded reclaim pair.
	requireAttempts(t, s, runID, "c1", []string{store.StepStatusSucceeded})
	requireAttempts(t, s, runID, "nap1", []string{store.StepStatusSucceeded})
	requireAttempts(t, s, runID, "c2", []string{store.StepStatusSucceeded})
	requireAttempts(t, s, runID, "nap2", []string{store.AttemptOutcomeLost, store.StepStatusSucceeded})
	requireAttempts(t, s, runID, "c3", []string{store.StepStatusSucceeded})
	for step, path := range counters {
		requireCounterFiredOnce(t, path, step)
	}
	if got := countStepEvents(t, s, runID, store.EventStepReclaimed, "nap2"); got != 1 {
		t.Errorf("step_reclaimed(nap2) events = %d, want exactly 1", got)
	}
	for _, stepID := range []string{"c1", "nap1", "c2", "nap2", "c3"} {
		if got := countStepEvents(t, s, runID, store.EventStepReady, stepID); got != 1 {
			t.Errorf("step_ready(%s) events = %d, want exactly 1", stepID, got)
		}
	}
	requireOutboxEmpty(t, s)
}
