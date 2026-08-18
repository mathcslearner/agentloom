package engine

import (
	"testing"

	"github.com/google/uuid"

	"github.com/mathcslearner/agentloom/internal/queue"
	"github.com/mathcslearner/agentloom/internal/store"
)

// TestApprovalTimeoutEnvelope covers the pure expiry-envelope builder (ticket
// 15.4): it names the step (a pointer, never a payload), carries the
// approval_timeout reason, and rides the run's durable root trace context —
// and it is built without an EnqueuedAt so re-scheduling the same (run, step)
// expiry encodes to a byte-identical delayed member (ZADD dedup).
func TestApprovalTimeoutEnvelope(t *testing.T) {
	runID := uuid.New()
	trace := store.TraceContext{Parent: "00-trace-span-01", State: "vendor=1"}

	env := approvalTimeoutEnvelope(runID, "gate#2", trace)
	if env.RunID != runID || env.StepID != "gate#2" {
		t.Errorf("envelope target = %s/%s, want %s/gate#2", env.RunID, env.StepID, runID)
	}
	if env.Reason != queue.ReasonApprovalTimeout {
		t.Errorf("reason = %q, want %q", env.Reason, queue.ReasonApprovalTimeout)
	}
	if env.TraceParent != trace.Parent || env.TraceState != trace.State {
		t.Errorf("trace = %q/%q, want %q/%q", env.TraceParent, env.TraceState, trace.Parent, trace.State)
	}
	if !env.EnqueuedAt.IsZero() {
		t.Errorf("EnqueuedAt = %v, want zero (byte-identical dedup)", env.EnqueuedAt)
	}

	// Two envelopes for the same (run, step, trace) are identical — the dedup
	// property the scheduler/reconciler/cancel paths rely on.
	if again := approvalTimeoutEnvelope(runID, "gate#2", trace); again != env {
		t.Errorf("rebuilt envelope = %+v, want identical to %+v", again, env)
	}
}
