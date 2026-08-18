//go:build integration

package engine_test

import (
	"testing"

	"github.com/mathcslearner/agentloom/internal/event"
	"github.com/mathcslearner/agentloom/internal/store"
)

// TestEventFeedNormalized is the 16.1 "all engine writers emit normalized
// envelopes" acceptance (ADR-018): a real workflow run to completion produces a
// feed where every stored row decodes to a typed catalog payload, seq is
// contiguous and gap-free from 1, and every step-scoped event carries a lifted
// step id in its envelope. This proves the engine's completion/fan-out writers
// were all migrated onto the typed append — a row the catalog can't decode is a
// regression the unit-level fence can't see.
func TestEventFeedNormalized(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s, h, runID := setup(t, fanoutDef)
	d := startDispatcher(t, s, h.Queue())
	spawnWorkers(t, s, h, d)

	waitRun(t, s, runID, store.RunStatusSucceeded)
	h.WaitQuiescent(ctx)

	rows, err := s.Events().List(ctx, runID, 0, 10000)
	if err != nil {
		t.Fatalf("listing events: %v", err)
	}
	if len(rows) == 0 {
		t.Fatal("run produced no events")
	}

	sawRunCreated, sawStepSucceeded, sawRunSucceeded := false, false, false
	for i, row := range rows {
		// Seq contiguous, gap-free, ascending from 1.
		if wantSeq := int64(i + 1); row.Seq != wantSeq {
			t.Fatalf("event %d seq = %d, want %d (feed not gap-free)", i, row.Seq, wantSeq)
		}
		// Every row decodes to a typed envelope via the catalog.
		env, err := store.EventEnvelope(row)
		if err != nil {
			t.Fatalf("event seq %d type %q does not decode (unnormalized writer?): %v", row.Seq, row.Type, err)
		}
		if env.SchemaVersion != event.SchemaVersion || env.RunID != runID || env.Seq != row.Seq {
			t.Fatalf("event seq %d envelope mismatch: %+v", row.Seq, env)
		}
		// A step-scoped payload must project a non-empty step id into the envelope.
		if _, scoped := env.Payload.(event.StepScoped); scoped {
			if env.StepID == "" {
				t.Errorf("event seq %d (%s) is step-scoped but has empty envelope StepID", row.Seq, row.Type)
			}
		}
		switch env.Type {
		case event.TypeRunCreated:
			sawRunCreated = true
		case event.TypeStepSucceeded:
			sawStepSucceeded = true
			if env.StepID == "" {
				t.Errorf("step_succeeded seq %d has empty step id", row.Seq)
			}
		case event.TypeRunSucceeded:
			sawRunSucceeded = true
		}
	}
	if !sawRunCreated || !sawStepSucceeded || !sawRunSucceeded {
		t.Errorf("missing expected events: run_created=%v step_succeeded=%v run_succeeded=%v",
			sawRunCreated, sawStepSucceeded, sawRunSucceeded)
	}
}
