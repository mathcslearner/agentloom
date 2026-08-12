//go:build integration

package crash

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mathcslearner/agentloom/internal/exec/effects"
	"github.com/mathcslearner/agentloom/internal/queue/queuetest"
	"github.com/mathcslearner/agentloom/internal/store"
	"github.com/mathcslearner/agentloom/internal/store/storetest"
)

// Sustained chaos suite (ticket 5.8): a continuous submitter cycles mixed
// fixtures — plain chains, retrying steps, journaled effectful steps,
// fan-out/join, and a deliberate dead-letter — while random SIGKILLs land
// every few seconds and the Redis instance is restarted once mid-run. At
// quiescence every run must sit at exactly its fixture's expected terminal
// state (succeeded, or explainably dead-lettered), journaled side effects
// must be exactly-once by idempotency key, and the queue must be fully
// quiescent (stream drained, PEL empty, delayed empty — the 3.6 helpers)
// with the outbox empty.
//
// Unlike 4.7 (which exiled the reconciler so reclaim was the single
// mechanism under test), this suite runs the reconciler HOT: the Redis
// restart can lose the AOF tail (dispatched-but-lost XADDs, lost delayed
// ZADDs, vanished PEL entries), and those gaps are healable only by the
// reconciler's scans. Convergence to quiescence is the proof it healed
// every one.
//
// Every assertion here is deliberately stall- and chaos-tolerant: expected
// terminal states are kill-proof by construction (`lost` attempts are
// excluded from the retry budget, and fail_n_times keys off the durable
// attempt number), and effect exactness is asserted after dedup by
// idempotency key — never as raw attempt histories or churn counts, which
// random kill timing makes nondeterministic.

// EnvChaosDuration scales the chaos window (submitter + kill phases). The
// default is the CI short mode (~1 min wall including convergence);
// `make test-chaos-long` runs minutes of sustained chaos locally.
const EnvChaosDuration = "AGENTLOOM_CHAOS_DURATION"

const defaultChaosWindow = 24 * time.Second

// chaosRedisAddr is the dedicated redis-chaos compose service the suite
// connects to and restarts. Deliberately separate from the shared test
// Redis (AGENTLOOM_TEST_REDIS_ADDR) so the blip never disturbs the other
// integration tests running in parallel.
func chaosRedisAddr() string {
	if v, ok := os.LookupEnv("AGENTLOOM_TEST_CHAOS_REDIS_ADDR"); ok && v != "" {
		return v
	}
	return "localhost:6380"
}

func chaosWindow(tb testing.TB) time.Duration {
	v, ok := os.LookupEnv(EnvChaosDuration)
	if !ok || v == "" {
		return defaultChaosWindow
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		tb.Fatalf("%s=%q is not a positive Go duration", EnvChaosDuration, v)
	}
	return d
}

// chaosFixture is one submitted run with its expected terminal state and
// its externally observable effect files.
type chaosFixture struct {
	kind  string
	runID uuid.UUID
	// counters maps step ID -> file for plain `counter` steps: unjournaled,
	// so at-least-once under kills (asserted 1 ≤ lines ≤ attempts).
	counters map[string]string
	// effect maps step ID -> file for `effectful_echo` steps: journaled,
	// so exactly-once by idempotency key no matter the chaos.
	effect map[string]string
	// wantDeadLetter: the run must FAIL with exactly one dead_letters row
	// (retries_exhausted on the named step); otherwise it must succeed
	// with no dead_letters at all.
	wantDeadLetter string
}

// chaosFixtureKinds is the deterministic round-robin mix the submitter
// cycles through. Randomness lives only in kill timing and victim choice.
var chaosFixtureKinds = []string{"chain", "retry", "effectful", "fanout", "deadletter"}

// buildChaosFixture renders one fixture's definition JSON with per-run
// effect-file paths under dir. The retry policies use short backoffs so a
// dead-letter budget exhausts (and a flaky step recovers) in ~1s — chaos
// throughput, not backoff realism, is what this suite measures.
func buildChaosFixture(t *testing.T, dir string, seq int, kind string) (chaosFixture, string) {
	t.Helper()
	fx := chaosFixture{kind: kind, counters: map[string]string{}, effect: map[string]string{}}
	file := func(step string) string {
		p := filepath.Join(dir, fmt.Sprintf("run%d-%s-%s.log", seq, kind, step))
		b, err := json.Marshal(p)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	retry := `{"max_attempts":5,"backoff":{"initial":"100ms","cap":"500ms","multiplier":2},"jitter":"full","retry_on":["transient"]}`

	var def string
	switch kind {
	case "chain":
		before, after := file("before"), file("after")
		fx.counters["before"], fx.counters["after"] = unquote(t, before), unquote(t, after)
		def = fmt.Sprintf(`{
			"schema_version": 1,
			"name": "chaos-chain",
			"steps": [
				{"id": "before", "type": "counter", "config": {"path": %s}},
				{"id": "nap", "type": "sleep", "config": {"duration": "300ms"}},
				{"id": "after", "type": "counter", "config": {"path": %s}}
			],
			"edges": [
				{"from": "before", "to": "nap"},
				{"from": "nap", "to": "after"}
			]
		}`, before, after)
	case "retry":
		done := file("done")
		fx.counters["done"] = unquote(t, done)
		def = fmt.Sprintf(`{
			"schema_version": 1,
			"name": "chaos-retry",
			"steps": [
				{"id": "flaky", "type": "fail_n_times", "config": {"n": 2}, "retry": %s},
				{"id": "done", "type": "counter", "config": {"path": %s}}
			],
			"edges": [{"from": "flaky", "to": "done"}]
		}`, retry, done)
	case "effectful":
		notify := file("notify")
		fx.effect["notify"] = unquote(t, notify)
		def = fmt.Sprintf(`{
			"schema_version": 1,
			"name": "chaos-effectful",
			"steps": [
				{"id": "notify", "type": "effectful_echo",
				 "config": {"path": %s, "fail_times": 1, "input": {"notified": true}},
				 "retry": %s},
				{"id": "confirm", "type": "noop"}
			],
			"edges": [{"from": "notify", "to": "confirm"}]
		}`, notify, retry)
	case "fanout":
		tally := file("tally")
		fx.counters["tally"] = unquote(t, tally)
		def = fmt.Sprintf(`{
			"schema_version": 1,
			"name": "chaos-fanout",
			"steps": [
				{"id": "start", "type": "noop"},
				{"id": "left", "type": "sleep", "config": {"duration": "300ms"}},
				{"id": "right", "type": "sleep", "config": {"duration": "100ms"}},
				{"id": "gather", "type": "join", "config": {"mode": "all"}},
				{"id": "tally", "type": "counter", "config": {"path": %s}}
			],
			"edges": [
				{"from": "start", "to": "left"},
				{"from": "start", "to": "right"},
				{"from": "left", "to": "gather"},
				{"from": "right", "to": "gather"},
				{"from": "gather", "to": "tally"}
			]
		}`, tally)
	case "deadletter":
		// n far above any attempt count the run can reach: the step fails
		// every real attempt, exhausts the 2-attempt budget, and lands in
		// the DLQ as retries_exhausted — regardless of how many `lost`
		// attempts kills interleave (lost is excluded from the budget).
		fx.wantDeadLetter = "doomed"
		def = `{
			"schema_version": 1,
			"name": "chaos-deadletter",
			"on_failure": "fail_fast",
			"steps": [
				{"id": "doomed", "type": "fail_n_times", "config": {"n": 1000},
				 "retry": {"max_attempts": 2, "backoff": {"initial": "100ms", "cap": "300ms", "multiplier": 2}, "jitter": "full", "retry_on": ["transient"]}}
			],
			"edges": []
		}`
	default:
		t.Fatalf("unknown fixture kind %q", kind)
	}
	return fx, def
}

func unquote(t *testing.T, jsonStr string) string {
	t.Helper()
	var s string
	if err := json.Unmarshal([]byte(jsonStr), &s); err != nil {
		t.Fatal(err)
	}
	return s
}

// blipChaosRedis restarts the dedicated chaos Redis: SHUTDOWN NOSAVE kills
// the server, and the compose service's `restart: always` brings it back —
// the blip is Docker's restart latency. NOSAVE skips the RDB snapshot; AOF
// (appendfsync everysec) preserves everything but at most ~1s of tail, and
// that tail loss is deliberate: it is the gap only the reconciler can heal.
//
// The restart is proven by the server run_id changing, not by observing
// downtime: the blip is sub-second and go-redis's client-level retries mask
// it from a Ping poll entirely (SHUTDOWN itself returns nil — go-redis maps
// the connection dropping to success).
func blipChaosRedis(t *testing.T, h *queuetest.Harness) {
	t.Helper()
	ctx := context.Background()
	before, err := redisRunID(ctx, h)
	if err != nil {
		t.Fatalf("chaos redis: reading run_id before the blip: %v", err)
	}
	start := time.Now()
	_ = h.Client().ShutdownNoSave(ctx).Err()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if id, err := redisRunID(ctx, h); err == nil && id != before {
			t.Logf("chaos redis: restarted after %v (run_id %s -> %s)",
				time.Since(start).Round(time.Millisecond), before, id)
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("chaos redis run_id never changed after SHUTDOWN NOSAVE — does the redis-chaos service have restart: always, and is AGENTLOOM_TEST_CHAOS_REDIS_ADDR pointing at it? (make up)")
}

// redisRunID reads the server's run_id — regenerated on every server start,
// so a change is the definitive restart signal.
func redisRunID(ctx context.Context, h *queuetest.Harness) (string, error) {
	info, err := h.Client().Info(ctx, "server").Result()
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(info, "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), "run_id:"); ok {
			return v, nil
		}
	}
	return "", fmt.Errorf("no run_id in INFO server reply")
}

// TestSustainedChaos is ticket 5.8's headline: sustained mixed load,
// random worker SIGKILLs every few seconds, one Redis restart blip, and a
// full convergence + quiescence audit at the end.
func TestSustainedChaos(t *testing.T) {
	t.Parallel()

	window := chaosWindow(t)
	seed := time.Now().UnixNano()
	rng := rand.New(rand.NewSource(seed)) // #nosec G404 -- chaos scheduling, not crypto
	t.Logf("chaos: window %v, rng seed %d", window, seed)

	// The isolated world lives on the dedicated chaos Redis; Postgres is
	// the usual per-test database. Everything else mirrors setupWorld.
	pool, dsn := storetest.NewDBWithDSN(t)
	s := store.NewFromPool(pool)
	h := queuetest.NewAt(t, chaosRedisAddr())
	h.EnsureGroup(context.Background())
	env := workerEnv(dsn, h)
	env["AGENTLOOM_REDIS_ADDR"] = chaosRedisAddr()
	env["AGENTLOOM_QUEUE_CONSUMER_BATCH"] = "4"
	// The reconciler runs HOT — it is a mechanism under test here (the
	// blip's AOF tail loss is healable only by its scans). Staleness
	// thresholds sit just above the dispatch/promoter cadences so heals
	// land within a couple of sweeps.
	env["AGENTLOOM_WORKER_RECONCILE_INTERVAL"] = "1s"
	env["AGENTLOOM_WORKER_RECONCILE_READY_STALE"] = "2s"
	env["AGENTLOOM_WORKER_RECONCILE_RETRY_STALE"] = "2s"
	env["AGENTLOOM_WORKER_RECONCILE_RUNNING_STALE"] = "8s"
	// Random kills bump PEL delivery counts on innocent steps; the default
	// poison threshold (5) could divert a healthy step to the DLQ and turn
	// a should-succeed fixture into a nondeterministic failure. Poison has
	// its own 5.4 suite; here the threshold moves out of kill range.
	env["AGENTLOOM_QUEUE_POISON_THRESHOLD"] = "50"

	// Continuous submitter: one run every 250ms, cycling the fixture mix.
	// It runs on its own goroutine (Errorf is goroutine-safe, Fatalf is
	// not); the chaos choreography below stays on the test goroutine.
	dir := t.TempDir()
	var (
		mu       sync.Mutex
		fixtures []chaosFixture
	)
	snapshot := func() []chaosFixture {
		mu.Lock()
		defer mu.Unlock()
		return append([]chaosFixture(nil), fixtures...)
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
			fx, def := buildChaosFixture(t, dir, i, chaosFixtureKinds[i%len(chaosFixtureKinds)])
			id, err := newRun(s, def)
			if err != nil {
				t.Errorf("submitter: %v", err)
				return
			}
			fx.runID = id
			mu.Lock()
			fixtures = append(fixtures, fx)
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

	const fleetSize = 3
	workers := make([]*workerProc, fleetSize)
	for i := range workers {
		workers[i] = spawnWorker(t, fmt.Sprintf("worker-%d", i), env)
	}
	spawnSeq := fleetSize

	// killPhase SIGKILLs a random worker roughly every killEvery for the
	// given span, immediately replacing it — sequentially, on the test
	// goroutine, so spawnWorker's Fatalf stays legal and no spawn can race
	// the Redis outage (worker bootstrap pings Redis).
	const killEvery = 3 * time.Second
	killPhase := func(span time.Duration) {
		deadline := time.Now().Add(span)
		for time.Now().Before(deadline) {
			wait := killEvery/2 + time.Duration(rng.Int63n(int64(killEvery)))
			if remaining := time.Until(deadline); wait > remaining {
				time.Sleep(remaining)
				return
			}
			time.Sleep(wait)
			victim := rng.Intn(len(workers))
			t.Logf("chaos: SIGKILL %s (worker_id %s)", workers[victim].name, workers[victim].id())
			workers[victim].kill()
			workers[victim] = spawnWorker(t, fmt.Sprintf("worker-%d", spawnSeq), env)
			spawnSeq++
		}
	}

	// Phase 1: random kills. Blip: one Redis restart (kills paused so no
	// spawn races the outage). Phase 2: more kills. Then the chaos stops
	// and the surviving fleet converges the backlog.
	killPhase(window * 2 / 5)
	t.Log("chaos: restarting the chaos Redis (SHUTDOWN NOSAVE + restart: always)")
	blipChaosRedis(t, h)
	killPhase(window * 3 / 5)

	close(stopSubmit)
	<-submitDone
	all := snapshot()
	if len(all) < 20 {
		t.Fatalf("only %d runs were submitted; the scenario needs sustained load", len(all))
	}
	t.Logf("chaos: complete — %d runs submitted across %d fixture kinds, %d workers spawned", len(all), len(chaosFixtureKinds), spawnSeq)

	// Convergence: every run reaches a terminal state. The deadline covers
	// the post-chaos backlog plus reconciler heal latency; anything stuck
	// beyond it dumps full diagnostic state.
	waitAllTerminal(t, s, h, all, window+90*time.Second)

	// Expected terminal state, per fixture — and effect exactness.
	for _, fx := range all {
		requireExpectedTerminal(t, s, fx)
	}
	requireOutboxEmpty(t, s)
	h.WaitQuiescent(context.Background())
}

// waitAllTerminal polls until every run is terminal, dumping run states,
// stuck-run steps, and queue diagnostics on timeout. A run reaching any
// terminal state other than succeeded/failed (nothing in this suite cancels
// or parks) fails immediately.
func waitAllTerminal(t *testing.T, s *store.Store, h *queuetest.Harness, all []chaosFixture, timeout time.Duration) {
	t.Helper()
	ctx := context.Background()
	deadline := time.Now().Add(timeout)
	for {
		pending := 0
		var stuck []chaosFixture
		for _, fx := range all {
			run, err := s.Runs().Get(ctx, fx.runID)
			if err != nil {
				t.Fatalf("reading run %s: %v", fx.runID, err)
			}
			switch run.Status {
			case store.RunStatusSucceeded, store.RunStatusFailed:
			case store.RunStatusRunning:
				pending++
				stuck = append(stuck, fx)
			default:
				t.Fatalf("run %s (%s) reached %q — nothing in this suite cancels or parks:\n%s",
					fx.runID, fx.kind, run.Status, dumpSteps(t, s, fx.runID))
			}
		}
		if pending == 0 {
			return
		}
		if time.Now().After(deadline) {
			for i, fx := range stuck {
				if i >= 5 {
					t.Logf("chaos: ... and %d more stuck runs", len(stuck)-i)
					break
				}
				t.Logf("chaos: stuck run %s (%s):\n%s", fx.runID, fx.kind, dumpSteps(t, s, fx.runID))
			}
			h.DumpDiagnostics(ctx)
			t.Fatalf("%d of %d runs never reached a terminal state within %v", pending, len(all), timeout)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// requireExpectedTerminal asserts one run sits at exactly its fixture's
// expected terminal state, with its effect files at their expected values.
func requireExpectedTerminal(t *testing.T, s *store.Store, fx chaosFixture) {
	t.Helper()
	ctx := context.Background()
	run, err := s.Runs().Get(ctx, fx.runID)
	if err != nil {
		t.Fatalf("reading run %s: %v", fx.runID, err)
	}
	dls, err := s.DeadLetters().ListByRun(ctx, fx.runID)
	if err != nil {
		t.Fatalf("listing dead letters of %s: %v", fx.runID, err)
	}

	if fx.wantDeadLetter != "" {
		// Explainably dead-lettered: the run failed because its doomed step
		// exhausted the retry budget — the one legitimate DLQ route here.
		if run.Status != store.RunStatusFailed {
			t.Errorf("run %s (%s) status = %q, want %q:\n%s", fx.runID, fx.kind, run.Status, store.RunStatusFailed, dumpSteps(t, s, fx.runID))
		}
		if len(dls) != 1 {
			t.Errorf("run %s (%s) has %d dead_letters rows, want exactly 1: %+v", fx.runID, fx.kind, len(dls), dls)
		} else {
			dl := dls[0]
			if dl.StepID != fx.wantDeadLetter {
				t.Errorf("run %s (%s) dead-lettered step %q, want %q", fx.runID, fx.kind, dl.StepID, fx.wantDeadLetter)
			}
			if dl.Source != store.DeadLetterSourceRetriesExhausted {
				t.Errorf("run %s (%s) dead-letter source = %q, want %q (a poison row here means the raised threshold was breached)",
					fx.runID, fx.kind, dl.Source, store.DeadLetterSourceRetriesExhausted)
			}
		}
	} else {
		if run.Status != store.RunStatusSucceeded {
			t.Errorf("run %s (%s) status = %q, want %q:\n%s", fx.runID, fx.kind, run.Status, store.RunStatusSucceeded, dumpSteps(t, s, fx.runID))
		}
		if len(dls) != 0 {
			t.Errorf("run %s (%s) has unexpected dead_letters rows: %+v", fx.runID, fx.kind, dls)
		}
	}

	// Plain counters are UNJOURNALED: at-least-once under kills. A kill
	// between the file append and the completion commit re-runs the step,
	// so lines land anywhere in [1, attempts] — the contrast that motivates
	// the journal.
	for stepID, path := range fx.counters {
		lines := effectLines(t, fx, stepID, path)
		attempts, err := s.Attempts().ListByStep(ctx, fx.runID, stepID)
		if err != nil {
			t.Fatalf("listing attempts of %s/%s: %v", fx.runID, stepID, err)
		}
		if len(lines) < 1 || len(lines) > len(attempts) {
			t.Errorf("run %s (%s) counter %s fired %d times across %d attempts, want within [1, %d]",
				fx.runID, fx.kind, stepID, len(lines), len(attempts), len(attempts))
		}
	}

	// Journaled effects are EXACTLY-ONCE by idempotency key — the ticket's
	// "side-effect counters exactly at expected values". Raw line count is
	// ≥1, not ==1: a kill inside the intent→result window re-executes the
	// effect (the journal's documented residual at-least-once, ADR-006),
	// and the stable key is precisely what lets the external system — here,
	// this test — dedup it to one.
	for stepID, path := range fx.effect {
		lines := effectLines(t, fx, stepID, path)
		wantKey := "key=" + effects.Key(fx.runID, stepID)
		keys := map[string]bool{}
		for _, line := range lines {
			k, _, found := strings.Cut(line, " ")
			if !found {
				t.Errorf("run %s (%s) effect %s: malformed line %q", fx.runID, fx.kind, stepID, line)
				continue
			}
			keys[k] = true
			if k != wantKey {
				t.Errorf("run %s (%s) effect %s: line carries %q, want the derived key %q", fx.runID, fx.kind, stepID, k, wantKey)
			}
		}
		if len(lines) < 1 || len(keys) != 1 {
			t.Errorf("run %s (%s) effect %s: %d lines with %d distinct keys, want ≥1 line all under one key:\n%s",
				fx.runID, fx.kind, stepID, len(lines), len(keys), strings.Join(lines, "\n"))
		}
		rows, err := s.SideEffects().ListByStep(ctx, fx.runID, stepID)
		if err != nil {
			t.Fatalf("listing side_effects of %s/%s: %v", fx.runID, stepID, err)
		}
		if len(rows) != 1 || rows[0].Status != store.SideEffectStatusDone {
			t.Errorf("run %s (%s) effect %s: side_effects rows %+v, want exactly one done row", fx.runID, fx.kind, stepID, rows)
		}
	}
}

// effectLines reads one effect file's lines, failing with fixture context
// when the file is missing (the step never fired at all).
func effectLines(t *testing.T, fx chaosFixture, stepID, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path) // #nosec G304 -- t.TempDir path, test-only
	if err != nil {
		t.Fatalf("run %s (%s): reading effect file of %s: %v", fx.runID, fx.kind, stepID, err)
	}
	return strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
}
