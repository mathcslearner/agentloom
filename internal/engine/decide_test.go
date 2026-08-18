package engine

import (
	"testing"

	"github.com/mathcslearner/agentloom/internal/dag"
	"github.com/mathcslearner/agentloom/internal/store"
	"github.com/mathcslearner/agentloom/internal/store/gen"
)

func strptr(s string) *string { return &s }

// TestEdgeMatchesDecision covers the ADR-017 decision edge marker gate: an
// approve fires unmarked and approve-marked edges; a reject fires only
// reject-marked edges (an unmarked edge is the approve/success path).
func TestEdgeMatchesDecision(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		mark     *string
		decision dag.ApprovalDecision
		want     bool
	}{
		{"approve/unmarked", nil, dag.ApprovalApprove, true},
		{"approve/approve-marked", strptr("approve"), dag.ApprovalApprove, true},
		{"approve/reject-marked", strptr("reject"), dag.ApprovalApprove, false},
		{"reject/unmarked", nil, dag.ApprovalReject, false},
		{"reject/reject-marked", strptr("reject"), dag.ApprovalReject, true},
		{"reject/approve-marked", strptr("approve"), dag.ApprovalReject, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := edgeMatchesDecision(tc.mark, tc.decision); got != tc.want {
				t.Errorf("edgeMatchesDecision(%v, %q) = %v, want %v", tc.mark, tc.decision, got, tc.want)
			}
		})
	}
}

// TestFilterDecisionVerdicts asserts a fired verdict survives only when its
// edge marker matches the decision; a non-matching fired edge is forced to
// skipped, and an already-unfired verdict stays unfired.
func TestFilterDecisionVerdicts(t *testing.T) {
	t.Parallel()
	// Three edges: 0 unmarked (approve path), 1 reject-marked, 2 unmarked
	// (already not firing per its `when`).
	edges := []gen.RunEdge{
		{Ordinal: 0, EdgeType: store.EdgeTypeNormal, Decision: nil},
		{Ordinal: 1, EdgeType: store.EdgeTypeNormal, Decision: strptr("reject")},
		{Ordinal: 2, EdgeType: store.EdgeTypeNormal, Decision: nil},
	}
	verdicts := []edgeVerdict{
		{ordinal: 0, fired: true},
		{ordinal: 1, fired: true},
		{ordinal: 2, fired: false},
	}

	approve := filterDecisionVerdicts(edges, dag.ApprovalApprove, verdicts)
	wantApprove := map[int32]bool{0: true, 1: false, 2: false}
	for _, v := range approve {
		if v.fired != wantApprove[v.ordinal] {
			t.Errorf("approve: edge %d fired = %v, want %v", v.ordinal, v.fired, wantApprove[v.ordinal])
		}
	}

	reject := filterDecisionVerdicts(edges, dag.ApprovalReject, verdicts)
	wantReject := map[int32]bool{0: false, 1: true, 2: false}
	for _, v := range reject {
		if v.fired != wantReject[v.ordinal] {
			t.Errorf("reject: edge %d fired = %v, want %v", v.ordinal, v.fired, wantReject[v.ordinal])
		}
	}
}
