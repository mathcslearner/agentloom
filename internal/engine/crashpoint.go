package engine

import (
	"os"
	"strings"
	"sync/atomic"
)

// Crash-injection seam (ticket 13.5, ADR-015 / ADR-005).
//
// The completion-transaction boundaries the expansion crash matrix must
// exercise — after ExpandRun but before commit (E3), after commit but before
// the ACK/dispatch (E5) — live INSIDE the engine and cannot be provoked from
// outside the process: a SIGKILL from a parent test lands at an arbitrary
// instruction, never deterministically at "the tx is about to commit". This is
// the same gap the queue's ConsumerConfig.PhaseHook (ticket 3.6) filled for the
// pre-handle / pre-ack crash points, and the in-process completeFailpoint fills
// for the single-transaction atomicity tests.
//
// Unlike those two — which return an error (redeliver) so an in-process test
// can assert and continue — a crash point HARD-EXITS the process with no
// deferred cleanup, no cooperative cancellation, and no completion write. That
// is the faithful SIGKILL analogue: heartbeats simply stop, the pgx connection
// drops (any in-flight transaction rolls back under Postgres atomicity), the
// PEL entry lingers un-acked, and recovery flows entirely through ADR-005's
// reclaim → takeover path. It exists so the 13.5 subprocess matrix can drive a
// REAL cmd/worker to die at a named boundary on a named step, deterministically.
//
// It is armed only by AGENTLOOM_WORKER_CRASH_POINT via InstallCrashPointFromEnv,
// which cmd/worker calls only when test executors are enabled — so it is inert
// in every real deployment (a nil-pointer load per boundary, the same
// negligible cost as the completeFailpoint check already on these paths). When
// unset, maybeCrash is a no-op.

// Crash-point stages, named to match the ADR-015 crash-matrix boundaries the
// 13.5 kill-at-boundary matrix targets. Exported so the subprocess matrix
// (test/crash) references one vocabulary rather than duplicating the strings —
// a rename there is a compile error, not a silently-never-firing crash.
// CrashStageAfterExpand (E3) is the same boundary as the in-process failpoint
// stageAfterExpand in complete.go, so it aliases that constant.
const (
	// CrashStagePreClaim (E1): before the claim CAS — step still ready, graph
	// unchanged; recovery is ordinary redelivery/reclaim.
	CrashStagePreClaim = "pre_claim"
	// CrashStagePreCompletion (E2): the executor produced its result but the
	// completion transaction has not begun — step running under a dead claim,
	// graph unchanged; recovery re-executes the step (a planner re-plans).
	CrashStagePreCompletion = "pre_completion"
	// CrashStageAfterExpand (E3): ExpandRun ran inside the completion tx but the
	// tx has not committed — the rollback drops the expansion, leaving the graph
	// at its pre-expansion version; recovery re-executes and expands once.
	CrashStageAfterExpand = stageAfterExpand
	// CrashStagePostCommit (E5): the completion transaction committed (the origin
	// succeeded, any expansion applied, outbox rows written) but the entry is
	// not yet acked and this worker's dispatcher has not drained the injected
	// steps' rows — recovery ACK-drops the terminal origin (E4) and the
	// transactional outbox dispatches the injected steps.
	CrashStagePostCommit = "post_commit"
)

// crashDirective is the armed crash point: fire at stage when the step id
// matches stepID. Matching on step id (not type) keeps every boundary uniform —
// the pre-claim boundary has only the envelope's step id in hand — and lets the
// 13.5 fixture target its planner precisely.
type crashDirective struct {
	stage  string
	stepID string
}

// crashPoint is nil in every real deployment; InstallCrashPointFromEnv sets it.
// An armed process crashes exactly once (the first match exits it), so there is
// no re-arm concern.
var crashPoint atomic.Pointer[crashDirective]

// maybeCrash hard-exits the process when the armed directive matches stage and
// stepID. The nil-pointer fast path makes it a no-op when unarmed.
func maybeCrash(stage, stepID string) {
	d := crashPoint.Load()
	if d == nil || d.stage != stage || d.stepID != stepID {
		return
	}
	// os.Exit runs no deferred cleanup and flushes nothing — deliberately, so
	// this mimics a SIGKILL and not a graceful shutdown. 137 = 128 + SIGKILL(9),
	// the conventional "killed" exit status, so the test can distinguish an
	// injected crash from a clean or panicking exit.
	os.Stderr.WriteString("engine: CRASH POINT " + stage + " on step " + stepID + " — hard-exiting to simulate a crash\n") //nolint:errcheck // best-effort diagnostic on the way out
	os.Exit(137)
}

// InstallCrashPointFromEnv arms the crash seam from AGENTLOOM_WORKER_CRASH_POINT,
// whose value is "<stage>:<step_id>" (e.g. "post_commit:plan"). An empty value
// leaves the seam disarmed (the production default). A malformed value is
// ignored (disarmed) rather than failing boot: this is a test-only knob and a
// worker must never refuse to start over it. getenv is os.Getenv in production
// and injectable in unit tests.
func InstallCrashPointFromEnv(getenv func(string) string) {
	v := strings.TrimSpace(getenv(EnvWorkerCrashPoint))
	if v == "" {
		return
	}
	stage, stepID, ok := strings.Cut(v, ":")
	if !ok || stage == "" || stepID == "" {
		return
	}
	crashPoint.Store(&crashDirective{stage: stage, stepID: stepID})
}

// EnvWorkerCrashPoint names the crash-seam env knob. Exported so cmd/worker and
// the 13.5 subprocess matrix reference one constant.
const EnvWorkerCrashPoint = "AGENTLOOM_WORKER_CRASH_POINT"
