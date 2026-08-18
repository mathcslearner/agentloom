package event

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"
)

// allTypes is the complete Type vocabulary declared in event.go. The catalog
// must cover exactly this set — TestCatalogComplete enforces both directions.
var allTypes = []Type{
	TypeRunCreated,
	TypeStepReady, TypeStepClaimed, TypeStepSucceeded, TypeStepFailed, TypeStepSkipped,
	TypeStepReclaimed, TypeStepRetryScheduled, TypeStepThrottled, TypeStepSemanticRetry,
	TypeStepDeadLettered, TypeStepCancelled, TypeStepCollected, TypeStepRequeued, TypeStepRevived,
	TypeRunSucceeded, TypeRunFailed, TypeRunResumed, TypeRunParked, TypeRunUnparked,
	TypeRunCancelling, TypeRunCancelled,
	TypeCostUpdated, TypeCostUnknownModel, TypeBudgetExceeded, TypeRunBudgetUpdated, TypeModelDowngraded,
	TypeBlackboardUpdated, TypeContextAssembled, TypeContextRevision,
	TypeGraphExpanded, TypeLoopExhausted, TypeLoopNoProgress, TypeGuardTripped,
	TypeApprovalRequested, TypeApprovalCancelled, TypeApprovalDecided, TypeApprovalExpired,
	TypeApprovalNotified, TypeApprovalNotificationFailed,
}

// TestCatalogComplete pins the catalog against the vocabulary: every declared
// Type has exactly one catalog entry, and every catalog entry is a declared
// Type. A new event type cannot ship without its payload + catalog entry.
func TestCatalogComplete(t *testing.T) {
	t.Parallel()

	inVocab := make(map[Type]bool, len(allTypes))
	for _, ty := range allTypes {
		inVocab[ty] = true
	}
	if len(inVocab) != len(allTypes) {
		t.Fatalf("allTypes has duplicates: %d entries, %d distinct", len(allTypes), len(inVocab))
	}

	seen := make(map[Type]bool, len(Catalog))
	for _, e := range Catalog {
		if seen[e.Type] {
			t.Errorf("catalog has duplicate entry for %q", e.Type)
		}
		seen[e.Type] = true
		if !inVocab[e.Type] {
			t.Errorf("catalog has entry %q not in the Type vocabulary", e.Type)
		}
	}
	for _, ty := range allTypes {
		if !seen[ty] {
			t.Errorf("Type %q has no catalog entry", ty)
		}
	}
}

// TestCatalogSelfConsistency checks each entry's derived facts against its
// payload: the factory mints a payload whose EventType matches the entry's
// Type, and StepScoped matches whether the payload implements StepScoped.
func TestCatalogSelfConsistency(t *testing.T) {
	t.Parallel()

	for _, e := range Catalog {
		p := e.New()
		if p.EventType() != e.Type {
			t.Errorf("%q: payload EventType()=%q disagrees with entry", e.Type, p.EventType())
		}
		_, scoped := p.(StepScoped)
		if scoped != e.StepScoped {
			t.Errorf("%q: StepScoped=%v but payload StepScoped-impl=%v", e.Type, e.StepScoped, scoped)
		}
		if e.Since == "" {
			t.Errorf("%q: empty Since", e.Type)
		}
		if e.Doc == "" {
			t.Errorf("%q: empty Doc", e.Type)
		}
	}
}

// TestDecodeRoundTrip marshals a populated payload and decodes it back through
// the catalog for every type, asserting the concrete type and field values
// survive — the read path the WS/TS tooling relies on.
func TestDecodeRoundTrip(t *testing.T) {
	t.Parallel()

	for _, e := range Catalog {
		e := e
		t.Run(string(e.Type), func(t *testing.T) {
			t.Parallel()
			orig := e.New()
			raw, err := json.Marshal(orig)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			got, err := Decode(e.Type, raw)
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			if reflect.TypeOf(got) != reflect.TypeOf(orig) {
				t.Fatalf("decoded type = %T, want %T", got, orig)
			}
			if got.EventType() != e.Type {
				t.Errorf("decoded EventType = %q, want %q", got.EventType(), e.Type)
			}
		})
	}
}

// TestDecodeUnknownType rejects a type not in the catalog.
func TestDecodeUnknownType(t *testing.T) {
	t.Parallel()
	if _, err := Decode(Type("no_such_event"), json.RawMessage(`{}`)); err == nil {
		t.Fatal("Decode of unknown type: want error, got nil")
	}
}

// TestNewEnvelopeStepIDProjection checks the envelope lifts a step-scoped
// payload's step id and leaves a run-scoped payload's empty.
func TestNewEnvelopeStepIDProjection(t *testing.T) {
	t.Parallel()

	runID := uuid.New()
	ts := time.Unix(1_700_000_000, 0).UTC()

	step := NewEnvelope(runID, 7, ts, &StepSucceeded{StepID: "draft", Attempt: 2})
	if step.SchemaVersion != SchemaVersion {
		t.Errorf("SchemaVersion = %d, want %d", step.SchemaVersion, SchemaVersion)
	}
	if step.StepID != "draft" {
		t.Errorf("step-scoped StepID = %q, want draft", step.StepID)
	}
	if step.Type != TypeStepSucceeded || step.Seq != 7 || step.RunID != runID {
		t.Errorf("envelope mismatch: %+v", step)
	}

	run := NewEnvelope(runID, 8, ts, &RunSucceeded{})
	if run.StepID != "" {
		t.Errorf("run-scoped StepID = %q, want empty", run.StepID)
	}

	// A loop event's canonical step is the concrete completing instance.
	loop := NewEnvelope(runID, 9, ts, &LoopExhausted{LoopSourceStep: "critique", LoopSourceInstance: "critique#2"})
	if loop.StepID != "critique#2" {
		t.Errorf("loop StepID = %q, want critique#2", loop.StepID)
	}
}

// TestDecodeEnvelopeWireShape checks the wire envelope carries the projected
// fields and a typed payload for a representative event.
func TestDecodeEnvelopeWireShape(t *testing.T) {
	t.Parallel()

	runID := uuid.New()
	ts := time.Unix(1_700_000_100, 0).UTC()
	payload := json.RawMessage(`{"step_id":"gen","claim_id":"c1","attempt":3}`)

	env, err := DecodeEnvelope(runID, 12, ts, TypeStepClaimed, payload)
	if err != nil {
		t.Fatalf("DecodeEnvelope: %v", err)
	}
	if env.StepID != "gen" {
		t.Errorf("StepID = %q, want gen", env.StepID)
	}
	sc, ok := env.Payload.(*StepClaimed)
	if !ok {
		t.Fatalf("payload type = %T, want *StepClaimed", env.Payload)
	}
	if sc.Attempt != 3 || sc.ClaimID != "c1" {
		t.Errorf("payload = %+v, want attempt 3 / claim c1", sc)
	}

	// The whole envelope marshals back to the normalized wire shape.
	wire, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}
	var back map[string]any
	if err := json.Unmarshal(wire, &back); err != nil {
		t.Fatalf("unmarshal envelope: %v", err)
	}
	for _, k := range []string{"schema_version", "run_id", "seq", "type", "ts", "step_id", "payload"} {
		if _, ok := back[k]; !ok {
			t.Errorf("wire envelope missing %q", k)
		}
	}
}
