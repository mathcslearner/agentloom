package event

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
)

// TestDecodeGraphExpandedWithConfig is the regression for the run-WS / firehose
// live-tail stall (ticket 18.2): a graph_expanded delta carries steps whose
// Config is the dag.StepConfig interface, which a plain json.Unmarshal cannot
// populate. The custom GraphExpanded.UnmarshalJSON routes the delta through the
// canonical plan decoder, so re-decoding a published/stored envelope succeeds.
func TestDecodeGraphExpandedWithConfig(t *testing.T) {
	t.Parallel()
	// A realistic planner delta (an llm step carries a full config object).
	raw := json.RawMessage(`{
		"origin_step":"plan","origin_kind":"planner","from_version":1,"to_version":2,"depth":1,
		"delta":{"schema_version":1,
			"steps":[{"id":"work_a","type":"llm","config":{"model":"mock/sim-1","prompt":"A","max_tokens":32,"temperature":0}}],
			"edges":[{"from":"plan","to":"work_a"},{"from":"work_a","to":"gather"}]},
		"widened":["gather"]}`)

	p, err := Decode(TypeGraphExpanded, raw)
	if err != nil {
		t.Fatalf("Decode(graph_expanded) failed (the live-tail stall): %v", err)
	}
	ge, ok := p.(*GraphExpanded)
	if !ok {
		t.Fatalf("decoded type = %T, want *GraphExpanded", p)
	}
	if len(ge.Delta.Steps) != 1 || ge.Delta.Steps[0].ID != "work_a" {
		t.Fatalf("delta steps = %+v, want one work_a", ge.Delta.Steps)
	}
	if ge.Delta.Steps[0].Config == nil {
		t.Fatal("step config is nil — the plan decoder did not populate the interface")
	}
	// The whole envelope round-trips through ParseEnvelope (the pub/sub path).
	env := NewEnvelope(uuid.New(), 9, time.Unix(1_700_000_000, 0).UTC(), ge)
	wire, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}
	if _, err := ParseEnvelope(wire); err != nil {
		t.Fatalf("ParseEnvelope round-trip failed: %v", err)
	}
}
