package dag

import "testing"

// TestStepTypeCatalogsInSync pins the decoder's config registry
// (stepConfigTypes) to the documentation-order catalog (stepTypes) that
// drives JSON Schema generation and the kitchen-sink coverage test. The
// two are separate declarations: a type added only to the map would
// decode while staying silently absent from the generated schema and
// exempt from example coverage, so drift in either direction fails here.
func TestStepTypeCatalogsInSync(t *testing.T) {
	t.Parallel()

	seen := make(map[StepType]bool, len(stepTypes))
	for _, st := range stepTypes {
		if seen[st] {
			t.Errorf("stepTypes lists %q more than once", st)
		}
		seen[st] = true
		if _, ok := stepConfigTypes[st]; !ok {
			t.Errorf("step type %q is in stepTypes but has no config registered in stepConfigTypes", st)
		}
	}
	for st := range stepConfigTypes {
		if !seen[st] {
			t.Errorf("step type %q is registered in stepConfigTypes but missing from the stepTypes catalog", st)
		}
	}
}
