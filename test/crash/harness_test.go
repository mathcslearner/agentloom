//go:build integration

// Package crash holds the flagship crash-recovery suite (ticket 4.7): the
// scenarios here run REAL `cmd/worker` processes — built once per test run
// by TestMain — and kill them with SIGKILL mid-step. Everything the
// in-process engine suites cannot prove lives here: genuine process death
// (no deferred cleanup, no cooperative cancellation — heartbeats simply
// stop), recovery purely via ADR-005's reclaim → takeover path, and
// full-fleet restart resume.
//
// Isolation: each test gets its own migrated Postgres database
// (storetest.NewDBWithDSN) and its own Redis keys (queuetest.New); both
// are handed to the worker subprocesses through the AGENTLOOM_* queue-name
// knobs this ticket added. Synchronization is always poll-until-state on
// Postgres or Redis — never a wall-clock sleep — and lease timings are
// tuned down via env (ADR-005's convention: chaos tests shrink TTLs
// rather than faking the Redis server's idle clock).
package crash

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	osexec "os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"

	"github.com/mathcslearner/agentloom/internal/dag"
	"github.com/mathcslearner/agentloom/internal/queue"
	"github.com/mathcslearner/agentloom/internal/queue/queuetest"
	"github.com/mathcslearner/agentloom/internal/store"
	"github.com/mathcslearner/agentloom/internal/store/storetest"
)

// leaseTTL is the tuned-down lease the worker subprocesses run with.
// Heartbeat (TTL/3) and reclaim (TTL/2) intervals derive from it, so a
// dead worker's step is reclaimed within ~1.5×TTL. 2s rather than the
// minimum viable 1s (post-M4 audit): the survivor must heartbeat a 4s
// sleep step to completion, so the TTL is also the stall budget a loaded
// CI runner gets before a *spurious* reclaim breaks the exactly-one-
// reclaim assertions — 2s doubles that margin for ~1s of extra test time.
const leaseTTL = 2 * time.Second

// waitTimeout bounds every poll loop. Generous relative to the scenario
// budget (a reclaim plus a full sleep re-execution is ~6s) so CI machines
// under load don't flake.
const waitTimeout = 45 * time.Second

// workerBin is the cmd/worker binary TestMain builds once for the suite.
var workerBin string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "agentloom-crash-")
	if err != nil {
		fmt.Fprintln(os.Stderr, "crash: creating build dir:", err)
		os.Exit(1)
	}
	workerBin = filepath.Join(dir, "worker")
	build := osexec.Command("go", "build", "-o", workerBin, "github.com/mathcslearner/agentloom/cmd/worker") // #nosec G204 -- fixed argv; workerBin is our own temp path
	build.Stdout, build.Stderr = os.Stdout, os.Stderr
	if err := build.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "crash: building cmd/worker:", err)
		os.RemoveAll(dir) //nolint:errcheck // best-effort cleanup
		os.Exit(1)
	}
	code := m.Run()
	os.RemoveAll(dir) //nolint:errcheck // best-effort cleanup
	os.Exit(code)
}

// testRedisAddr mirrors queuetest's convention: the deliberately separate
// AGENTLOOM_TEST_REDIS_ADDR with the compose-stack default, so re-pointing
// dev tools never redirects tests.
func testRedisAddr() string {
	if v, ok := os.LookupEnv("AGENTLOOM_TEST_REDIS_ADDR"); ok && v != "" {
		return v
	}
	return "localhost:6379"
}

// workerEnv is the subprocess environment for one test's isolated world:
// its database, its Redis keys, tuned-down lease timings, and a reconciler
// pushed out of the picture so the reclaim path is the single recovery
// mechanism under test (the reconciler heal has its own 4.5 suite).
func workerEnv(dsn string, h *queuetest.Harness) map[string]string {
	return map[string]string{
		"AGENTLOOM_POSTGRES_DSN":              dsn,
		"AGENTLOOM_REDIS_ADDR":                testRedisAddr(),
		"AGENTLOOM_QUEUE_STREAM":              h.Queue().Stream(),
		"AGENTLOOM_QUEUE_GROUP":               h.Queue().Group(),
		"AGENTLOOM_QUEUE_DELAYED_KEY":         h.Delayed().Key(),
		"AGENTLOOM_QUEUE_LEASE_TTL":           leaseTTL.String(),
		"AGENTLOOM_QUEUE_CONSUMER_BLOCK":      "100ms",
		"AGENTLOOM_QUEUE_CONSUMER_BATCH":      "1",
		"AGENTLOOM_WORKER_DISPATCH_INTERVAL":  "50ms",
		"AGENTLOOM_WORKER_RECONCILE_INTERVAL": "10m",
		"AGENTLOOM_WORKER_HEALTH_INTERVAL":    "10m",
		"AGENTLOOM_LOG_FORMAT":                "json",
		"AGENTLOOM_LOG_LEVEL":                 "info",
	}
}

// workerProc is one live cmd/worker subprocess with its captured output.
type workerProc struct {
	t    *testing.T
	name string
	cmd  *osexec.Cmd
	done chan struct{} // closed when Wait returns

	mu       sync.Mutex
	lines    []string
	workerID string
	idReady  chan struct{} // closed once workerID is parsed
	killed   bool
}

// spawnWorker starts one worker subprocess with the given env overrides
// (applied on top of the inherited environment, so they always win) and
// waits until its startup log line yields the worker_id — which is also
// the consumer name its PEL entries carry.
func spawnWorker(t *testing.T, name string, env map[string]string) *workerProc {
	t.Helper()
	w := &workerProc{
		t:       t,
		name:    name,
		cmd:     osexec.Command(workerBin),
		done:    make(chan struct{}),
		idReady: make(chan struct{}),
	}
	w.cmd.Env = os.Environ()
	for k, v := range env {
		w.cmd.Env = append(w.cmd.Env, k+"="+v)
	}
	pr, pw := io.Pipe()
	w.cmd.Stdout, w.cmd.Stderr = pw, pw
	if err := w.cmd.Start(); err != nil {
		t.Fatalf("starting worker %s: %v", name, err)
	}
	go w.scan(pr)
	go func() {
		defer close(w.done)
		w.cmd.Wait() //nolint:errcheck // exit status is asserted via test state, and SIGKILL is expected
		pw.Close()   //nolint:errcheck // io.Pipe close never fails
	}()
	t.Cleanup(func() {
		w.kill()
		if t.Failed() {
			w.dumpOutput()
		}
	})

	select {
	case <-w.idReady:
	case <-time.After(waitTimeout):
		w.dumpOutput()
		t.Fatalf("worker %s never logged its startup line", name)
	case <-w.done:
		w.dumpOutput()
		t.Fatalf("worker %s exited before logging its startup line", name)
	}
	return w
}

// scan splits the subprocess's combined output into lines, keeping them
// for diagnostics and extracting worker_id from the "worker started" line.
func (w *workerProc) scan(r io.Reader) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Text()
		w.mu.Lock()
		w.lines = append(w.lines, line)
		if w.workerID == "" {
			var rec struct {
				Msg      string `json:"msg"`
				WorkerID string `json:"worker_id"`
			}
			if err := json.Unmarshal([]byte(line), &rec); err == nil &&
				rec.Msg == "worker started" && rec.WorkerID != "" {
				w.workerID = rec.WorkerID
				close(w.idReady)
			}
		}
		w.mu.Unlock()
	}
}

// id returns the worker's parsed worker_id (== its consumer name).
func (w *workerProc) id() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.workerID
}

// kill delivers SIGKILL — genuine process death: no drain, no deferred
// cleanup, heartbeats simply stop — and waits for the process to be gone.
// Idempotent so the spawn-time cleanup can always call it.
func (w *workerProc) kill() {
	w.t.Helper()
	w.mu.Lock()
	alreadyKilled := w.killed
	w.killed = true
	w.mu.Unlock()
	if alreadyKilled {
		return
	}
	if err := w.cmd.Process.Kill(); err != nil && !strings.Contains(err.Error(), "already finished") {
		w.t.Errorf("SIGKILL worker %s: %v", w.name, err)
	}
	select {
	case <-w.done:
	case <-time.After(waitTimeout):
		w.t.Errorf("worker %s did not exit after SIGKILL", w.name)
	}
}

// dumpOutput logs the captured subprocess output for post-mortem.
func (w *workerProc) dumpOutput() {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, line := range w.lines {
		w.t.Logf("[%s] %s", w.name, line)
	}
}

// byID returns the spawned worker whose worker_id matches, failing the
// test when none does.
func byID(t *testing.T, workers []*workerProc, id string) *workerProc {
	t.Helper()
	for _, w := range workers {
		if w.id() == id {
			return w
		}
	}
	t.Fatalf("no spawned worker has worker_id %q", id)
	return nil
}

// waitFor polls cond until it returns true, failing the test with desc on
// timeout. The suite's only synchronization primitive.
func waitFor(t *testing.T, desc string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(waitTimeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", desc)
}

// stepStatus reads one step's current status.
func stepStatus(t *testing.T, s *store.Store, runID uuid.UUID, stepID string) string {
	t.Helper()
	step, err := s.Steps().Get(context.Background(), runID, stepID)
	if err != nil {
		t.Fatalf("reading step %s: %v", stepID, err)
	}
	return step.Status
}

// waitRunSucceeded polls until the run succeeds, failing fast if it fails
// instead, with a step-state dump on timeout.
func waitRunSucceeded(t *testing.T, s *store.Store, runID uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	deadline := time.Now().Add(waitTimeout)
	var status string
	for time.Now().Before(deadline) {
		run, err := s.Runs().Get(ctx, runID)
		if err != nil {
			t.Fatalf("reading run: %v", err)
		}
		status = run.Status
		if status == store.RunStatusSucceeded {
			return
		}
		if status != store.RunStatusRunning {
			t.Fatalf("run reached %q, want %q:\n%s", status, store.RunStatusSucceeded, dumpSteps(t, s, runID))
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("run never succeeded (stuck at %q):\n%s", status, dumpSteps(t, s, runID))
}

// dumpSteps renders the run's step states for stuck-run diagnostics.
func dumpSteps(t *testing.T, s *store.Store, runID uuid.UUID) string {
	t.Helper()
	steps, err := s.Steps().ListByRun(context.Background(), runID)
	if err != nil {
		return fmt.Sprintf("(listing steps: %v)", err)
	}
	var b strings.Builder
	for _, st := range steps {
		fmt.Fprintf(&b, "  %s [%s] status=%s\n", st.StepID, st.StepType, st.Status)
	}
	return b.String()
}

// pendingEntry is one PEL entry resolved to the step it points at.
type pendingEntry struct {
	Consumer string
	StepID   string
	Idle     time.Duration
}

// pendingEntries lists the group's PEL with each entry resolved to its
// step ID (via XRANGE + envelope decode) — how a test learns which
// consumer, and therefore which subprocess, holds a step's lease.
func pendingEntries(t *testing.T, h *queuetest.Harness) []pendingEntry {
	t.Helper()
	ctx := context.Background()
	pending, err := h.Client().XPendingExt(ctx, &redis.XPendingExtArgs{
		Stream: h.Queue().Stream(), Group: h.Queue().Group(), Start: "-", End: "+", Count: 64,
	}).Result()
	if err != nil {
		t.Fatalf("XPENDING: %v", err)
	}
	out := make([]pendingEntry, 0, len(pending))
	for _, p := range pending {
		msgs, err := h.Client().XRange(ctx, h.Queue().Stream(), p.ID, p.ID).Result()
		if err != nil {
			t.Fatalf("XRANGE %s: %v", p.ID, err)
		}
		if len(msgs) == 0 {
			continue // trimmed between XPENDING and XRANGE; not the entry we want anyway
		}
		env, err := queue.DecodeEnvelope(msgs[0].Values)
		if err != nil {
			t.Fatalf("decoding entry %s: %v", p.ID, err)
		}
		out = append(out, pendingEntry{Consumer: p.Consumer, StepID: env.StepID, Idle: p.Idle})
	}
	return out
}

// leaseHolder returns the consumer currently holding stepID's PEL entry,
// or "" when no such entry is pending.
func leaseHolder(t *testing.T, h *queuetest.Harness, stepID string) string {
	t.Helper()
	for _, p := range pendingEntries(t, h) {
		if p.StepID == stepID {
			return p.Consumer
		}
	}
	return ""
}

// requireAttempts asserts a step's attempt history: the (outcome, distinct
// claim) sequence that proves a reclaim — or proves a step ran untouched.
// A want of "" means the attempt must still be open (no outcome yet).
func requireAttempts(t *testing.T, s *store.Store, runID uuid.UUID, stepID string, wantOutcomes []string) {
	t.Helper()
	attempts, err := s.Attempts().ListByStep(context.Background(), runID, stepID)
	if err != nil {
		t.Fatalf("listing attempts of %s: %v", stepID, err)
	}
	if len(attempts) != len(wantOutcomes) {
		t.Fatalf("step %s has %d attempts, want %d: %+v", stepID, len(attempts), len(wantOutcomes), attempts)
	}
	claims := make(map[uuid.UUID]bool)
	for i, want := range wantOutcomes {
		a := attempts[i]
		if a.AttemptNo != int32(i+1) {
			t.Errorf("step %s attempt[%d] number = %d, want %d", stepID, i, a.AttemptNo, i+1)
		}
		switch {
		case want == "":
			if a.Outcome != nil {
				t.Errorf("step %s attempt %d outcome = %q, want still open", stepID, i+1, *a.Outcome)
			}
		case a.Outcome == nil:
			t.Errorf("step %s attempt %d has no outcome, want %q", stepID, i+1, want)
		case *a.Outcome != want:
			t.Errorf("step %s attempt %d outcome = %q, want %q", stepID, i+1, *a.Outcome, want)
		}
		if claims[a.ClaimID] {
			t.Errorf("step %s attempt %d reuses claim %s; every attempt must carry a fresh fence", stepID, i+1, a.ClaimID)
		}
		claims[a.ClaimID] = true
	}
}

// countStepEvents counts events of the given type whose payload step_id
// matches — the per-step dispatch/completion counters.
func countStepEvents(t *testing.T, s *store.Store, runID uuid.UUID, eventType, stepID string) int {
	t.Helper()
	events, err := s.Events().List(context.Background(), runID, 0, 1000)
	if err != nil {
		t.Fatalf("listing events: %v", err)
	}
	n := 0
	for _, ev := range events {
		if ev.Type != eventType {
			continue
		}
		var payload struct {
			StepID string `json:"step_id"`
		}
		if err := json.Unmarshal(ev.Payload, &payload); err != nil {
			t.Fatalf("decoding %s payload %s: %v", ev.Type, ev.Payload, err)
		}
		if payload.StepID == stepID {
			n++
		}
	}
	return n
}

// requireCounterFiredOnce asserts the counter step's side-effect file
// holds exactly one line — the external effects-fired-once proof.
func requireCounterFiredOnce(t *testing.T, path, stepID string) {
	t.Helper()
	data, err := os.ReadFile(path) // #nosec G304 -- t.TempDir path, test-only
	if err != nil {
		t.Fatalf("reading counter file of %s: %v", stepID, err)
	}
	lines := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	if len(lines) != 1 {
		t.Errorf("counter %s fired %d times, want exactly once: %q", stepID, len(lines), string(data))
	}
}

// requireOutboxEmpty asserts every outbox row was drained.
func requireOutboxEmpty(t *testing.T, s *store.Store) {
	t.Helper()
	rows, err := s.Outbox().List(context.Background(), 100)
	if err != nil {
		t.Fatalf("listing outbox: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("outbox not drained: %d rows remain (%+v)", len(rows), rows)
	}
}

// setupRun builds one test's isolated world — migrated database, isolated
// queue keys, instantiated run — and returns the store, harness, worker
// env, and run ID.
func setupRun(t *testing.T, defJSON string) (*store.Store, *queuetest.Harness, map[string]string, uuid.UUID) {
	t.Helper()
	pool, dsn := storetest.NewDBWithDSN(t)
	s := store.NewFromPool(pool)
	h := queuetest.New(t)
	h.EnsureGroup(context.Background())
	def, err := dag.Decode([]byte(defJSON))
	if err != nil {
		t.Fatalf("decoding definition: %v", err)
	}
	res, err := s.CreateRun(context.Background(), store.CreateRunArgs{Definition: def, Now: time.Now().UTC()})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	return s, h, workerEnv(dsn, h), res.Run.ID
}
