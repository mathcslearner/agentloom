//go:build integration

package crash

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mathcslearner/agentloom/internal/engine"
	"github.com/mathcslearner/agentloom/internal/store"
)

// Expansion chaos & recovery matrix (ticket 13.5, ADR-015). These scenarios run
// REAL cmd/worker subprocesses and crash one at each 13.1 expansion boundary —
// pre-claim (E1), pre-completion / mid-LLM (E2), after-expand / pre-commit (E3),
// and post-commit / pre-dispatch (E5) — then let an unarmed survivor fleet
// recover the run purely through ADR-005's reclaim + transactional-outbox paths.
//
// The crash is deterministic, not timing-based: the armed worker carries
// AGENTLOOM_WORKER_CRASH_POINT=<boundary>:plan (the engine crash seam, ticket
// 13.5) and hard-exits — no drain, no deferred cleanup, heartbeats simply stop,
// any in-flight transaction rolls back with the dropped connection — the moment
// it reaches that boundary on the planner step. That is the faithful SIGKILL a
// parent-process kill can only land at a random instruction.
//
// The invariant proved across every boundary (ADR-015's crash matrix reduces
// each cell to "did the completion transaction commit?"): the run always
// completes, the expansion commits EXACTLY ONCE (graph_version 2, one
// graph_expanded stepping 1→2, steps_total 5), and no step runs twice
// (step_succeeded fires once per step, injected steps included) — a crash leaves
// either the pre-expansion graph (roll back / re-execute) or the fully-expanded
// graph (commit / ack-drop), never a partial one.

// planCrashPlan is the PlanOutput the offline planner returns: two llm workers
// spliced after the planner (plan → work_*) that fan into the pre-existing
// gather join (work_* → gather). Identical in shape to the 13.3 e2e plan.
const planCrashPlan = `{"schema_version":1,` +
	`"steps":[` +
	`{"id":"work_a","type":"llm","config":{"model":"mock/sim-1","prompt":"analyze A","max_tokens":64,"temperature":0}},` +
	`{"id":"work_b","type":"llm","config":{"model":"mock/sim-1","prompt":"analyze B","max_tokens":64,"temperature":0}}` +
	`],"edges":[` +
	`{"from":"plan","to":"work_a"},{"from":"plan","to":"work_b"},` +
	`{"from":"work_a","to":"gather"},{"from":"work_b","to":"gather"}]}`

// plannerCrashDef builds the offline planner definition: plan (planner) →
// gather (join all) → report (echo). The planner's prompt IS planCrashPlan, so
// the mock provider's structured-echo returns it verbatim and the whole run
// executes with no real model — the same trick examples/definitions/planner.json
// uses. The implicit json_schema validator (max_attempts 3) and the expansion
// caps gate the plan exactly as in production.
func plannerCrashDef(t *testing.T) string {
	t.Helper()
	prompt, err := json.Marshal(planCrashPlan) // escape the plan JSON into a prompt string literal
	if err != nil {
		t.Fatal(err)
	}
	return fmt.Sprintf(`{
		"schema_version": 1,
		"name": "expansion-crash",
		"steps": [
			{"id": "plan", "type": "planner",
			 "config": {"model": "mock/sim-1", "prompt": %s, "max_tokens": 512, "temperature": 0, "max_added_steps": 8},
			 "validation": {"max_attempts": 3}},
			{"id": "gather", "type": "join", "config": {"mode": "all"}},
			{"id": "report", "type": "echo", "config": {"input": {"status": "done"}}}
		],
		"edges": [
			{"from": "plan", "to": "gather"},
			{"from": "gather", "to": "report"}
		],
		"expansion": {"max_added_steps": 16, "max_expansions": 10, "max_depth": 2}
	}`, string(prompt))
}

// TestExpansionKillAtBoundaryMatrix crashes a worker at each expansion boundary
// across repeated runs and asserts single-expansion recovery + completion.
func TestExpansionKillAtBoundaryMatrix(t *testing.T) {
	t.Parallel()

	boundaries := []struct {
		name  string
		stage string
	}{
		{"pre_claim", engine.CrashStagePreClaim},           // E1: before the claim CAS
		{"pre_completion", engine.CrashStagePreCompletion}, // E2: after execute, before the completion tx
		{"after_expand", engine.CrashStageAfterExpand},     // E3: ExpandRun ran, tx not committed
		{"post_commit", engine.CrashStagePostCommit},       // E5: tx committed, not yet acked/dispatched
	}
	// A couple of runs per boundary — the recovery is deterministic, so this is
	// enough to catch a race without ballooning the CI short-mode budget.
	const repeat = 2

	for _, b := range boundaries {
		b := b
		t.Run(b.name, func(t *testing.T) {
			t.Parallel()
			for i := 0; i < repeat; i++ {
				i := i
				t.Run(fmt.Sprintf("run%d", i), func(t *testing.T) {
					runExpansionCrashScenario(t, b.stage)
				})
			}
		})
	}
}

// runExpansionCrashScenario submits the offline planner run, crashes a single
// armed worker at stage on the planner step, then spawns an unarmed survivor
// fleet and asserts the run recovers to a single committed expansion.
func runExpansionCrashScenario(t *testing.T, stage string) {
	t.Helper()
	ctx := context.Background()

	s, h, env := setupWorld(t)
	env["AGENTLOOM_LLM_MOCK_ENABLED"] = "true" // register the offline mock provider the planner routes to
	// Disable the response cache: it is global-scoped and shares the dev Redis
	// with every other integration test, so the planner's (identical) request
	// would hit a cache entry left by a prior run — short-circuiting the provider
	// call and the pre_completion boundary. The crash matrix is about expansion
	// recovery, not caching (ADR-015 E2 treats re-execution as cache-independent),
	// so every planner execution must actually run to hit its boundary.
	env["AGENTLOOM_CACHE_ENABLED"] = "false"

	runID, err := newRun(s, plannerCrashDef(t))
	if err != nil {
		t.Fatal(err)
	}

	// The armed worker hard-exits at `stage` on step "plan". It is the only
	// worker alive when the run is submitted, so it is provably the one that
	// claims and executes the planner (plan is the sole ready entry step).
	armedEnv := cloneEnv(env)
	armedEnv[engine.EnvWorkerCrashPoint] = stage + ":plan"
	armed := spawnWorker(t, "armed", armedEnv)

	// Wait for the crash: the process must exit on its own (the seam's os.Exit),
	// never be reaped by cleanup. A worker that keeps running means the crash
	// never fired — a broken seam or a mismatched boundary/step.
	select {
	case <-armed.done:
	case <-time.After(waitTimeout):
		armed.dumpOutput()
		t.Fatalf("armed worker never crashed at boundary %q on step plan", stage)
	}
	if code := armed.cmd.ProcessState.ExitCode(); code != 137 {
		armed.dumpOutput()
		t.Fatalf("armed worker exited with status %d at boundary %q, want 137 (the injected crash)", code, stage)
	}
	t.Logf("armed worker crashed at boundary %q (exit 137)", stage)

	// The unarmed survivor fleet recovers via reclaim (the orphaned planner
	// entry) and the transactional outbox (injected steps' dispatch rows). The
	// reconciler stays out of the picture (workerEnv pins a 10m interval), so
	// reclaim is the recovery mechanism under test — exactly as ticket 4.7.
	spawnWorker(t, "survivor-a", env)
	spawnWorker(t, "survivor-b", env)

	waitRunSucceeded(t, s, runID)
	h.WaitQuiescent(ctx)

	// Exactly one committed expansion, whichever side of the boundary the crash
	// landed on: graph_version 2, steps_total 5 (3 authored + 2 injected).
	run, err := s.Runs().Get(ctx, runID)
	if err != nil {
		t.Fatalf("reading run: %v", err)
	}
	if run.GraphVersion != 2 {
		t.Errorf("boundary %q: graph_version = %d, want 2 (exactly one expansion)", stage, run.GraphVersion)
	}
	if run.StepsTotal != 5 {
		t.Errorf("boundary %q: steps_total = %d, want 5 (3 authored + 2 injected)", stage, run.StepsTotal)
	}

	// The graph_version history is linear: one graph_expanded event stepping
	// 1 → 2, no gaps, no duplicate expansions.
	requireGraphVersionLinear(t, s, runID, stage)

	// No orphan steps and no double execution: every step (injected included)
	// is terminal succeeded and fired step_succeeded exactly once — a re-executed
	// planner (E2/E3) or a re-dispatched injected step (E5) must still land once.
	for _, id := range []string{"plan", "gather", "report", "work_a", "work_b"} {
		if got := stepStatus(t, s, runID, id); got != store.StepStatusSucceeded {
			t.Errorf("boundary %q: step %s status = %q, want succeeded:\n%s", stage, id, got, dumpSteps(t, s, runID))
		}
		if got := countStepEvents(t, s, runID, store.EventStepSucceeded, id); got != 1 {
			t.Errorf("boundary %q: step_succeeded(%s) events = %d, want exactly 1", stage, id, got)
		}
	}
	requireOutboxEmpty(t, s)
}

// cloneEnv copies a worker-env map so a per-worker override (the crash point)
// never mutates the shared survivor env.
func cloneEnv(env map[string]string) map[string]string {
	out := make(map[string]string, len(env)+1)
	for k, v := range env {
		out[k] = v
	}
	return out
}

// requireGraphVersionLinear asserts the run's graph_expanded events form a
// linear version history with no gaps or duplicates: exactly one expansion here,
// stepping FromVersion→ToVersion 1→2. This is the 13.5 "graph_version history
// linear" post-chaos quiescence check.
func requireGraphVersionLinear(t *testing.T, s *store.Store, runID uuid.UUID, stage string) {
	t.Helper()
	events, err := s.Events().List(context.Background(), runID, 0, 1000)
	if err != nil {
		t.Fatalf("listing events: %v", err)
	}
	prev := int32(1) // the run starts at graph_version 1 (instantiation)
	n := 0
	for _, ev := range events {
		if ev.Type != store.EventGraphExpanded {
			continue
		}
		var payload struct {
			OriginStep  string `json:"origin_step"`
			FromVersion int32  `json:"from_version"`
			ToVersion   int32  `json:"to_version"`
		}
		if err := json.Unmarshal(ev.Payload, &payload); err != nil {
			t.Fatalf("decoding graph_expanded payload %s: %v", ev.Payload, err)
		}
		n++
		if payload.FromVersion != prev {
			t.Errorf("boundary %q: graph_expanded #%d from_version = %d, want %d (non-linear history)", stage, n, payload.FromVersion, prev)
		}
		if payload.ToVersion != prev+1 {
			t.Errorf("boundary %q: graph_expanded #%d to_version = %d, want %d", stage, n, payload.ToVersion, prev+1)
		}
		if payload.OriginStep != "plan" {
			t.Errorf("boundary %q: graph_expanded #%d origin = %q, want plan", stage, n, payload.OriginStep)
		}
		prev = payload.ToVersion
	}
	if n != 1 {
		t.Errorf("boundary %q: %d graph_expanded events, want exactly 1 (single expansion)", stage, n)
	}
}
