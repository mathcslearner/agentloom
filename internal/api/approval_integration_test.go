//go:build integration

package api_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mathcslearner/agentloom/internal/api"
	"github.com/mathcslearner/agentloom/internal/dag"
	"github.com/mathcslearner/agentloom/internal/store"
)

// gateOnlyDef is a one-step definition whose only step is a human_approval
// gate — enough to park and surface an approval through the run-view API.
const gateOnlyDef = `{
  "schema_version": 1,
  "name": "gate-only",
  "steps": [
    {
      "id": "gate",
      "type": "human_approval",
      "config": {
        "title": "Approve this?",
        "payload": {"text": "proposed"},
        "allowed_decisions": ["approve", "reject"]
      }
    }
  ],
  "edges": []
}`

// TestGetRunSurfacesApproval is 15.2 criterion (c) at the API boundary: a
// parked human_approval step shows as awaiting_human and its pending approval
// appears in the run view's `approvals` array.
func TestGetRunSurfacesApproval(t *testing.T) {
	t.Parallel()
	s, srv, key := newServer(t)
	ctx := context.Background()
	now := time.Now().UTC()

	def, err := dag.Decode([]byte(gateOnlyDef))
	if err != nil {
		t.Fatalf("decoding def: %v", err)
	}
	res, err := s.CreateRun(ctx, store.CreateRunArgs{Definition: def, Now: now})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	runID := res.Run.ID

	// Claim the entry gate, then park it — the store half of the executor path.
	var claim uuid.UUID
	if err := s.WithTx(ctx, func(ctx context.Context, q store.Querier) error {
		step, cerr := store.ClaimStep(ctx, q, store.ClaimStepArgs{RunID: runID, StepID: "gate", Now: now})
		if cerr != nil {
			return cerr
		}
		claim = *step.ClaimID
		return nil
	}); err != nil {
		t.Fatalf("claim: %v", err)
	}
	timeoutAt := now.Add(24 * time.Hour)
	if err := s.WithTx(ctx, func(ctx context.Context, q store.Querier) error {
		_, perr := store.AwaitHumanStep(ctx, q, store.AwaitHumanStepArgs{
			RunID: runID, StepID: "gate", ClaimID: claim, Now: now,
			Approval: store.ApprovalRow{
				ID:               uuid.New(),
				Title:            "Approve this?",
				Payload:          []byte(`{"text":"proposed"}`),
				AllowedDecisions: []string{"approve", "reject"},
				TimeoutAt:        &timeoutAt,
			},
		})
		return perr
	}); err != nil {
		t.Fatalf("AwaitHumanStep: %v", err)
	}

	var run api.RunResponse
	if status := getJSON(t, srv, key, "/v1/runs/"+runID.String(), &run); status != http.StatusOK {
		t.Fatalf("GET /v1/runs/%s = %d, want 200", runID, status)
	}

	// The gate shows as awaiting_human, the run is still running.
	if run.Run.Status != store.RunStatusRunning {
		t.Errorf("run status = %q, want running", run.Run.Status)
	}
	if len(run.Steps) != 1 || run.Steps[0].Status != store.StepStatusAwaitingHuman {
		t.Errorf("steps = %+v, want one awaiting_human", run.Steps)
	}

	// The approval is in the run view.
	if len(run.Approvals) != 1 {
		t.Fatalf("approvals = %+v, want one", run.Approvals)
	}
	a := run.Approvals[0]
	if a.StepID != "gate" || a.Status != store.ApprovalStatusPending {
		t.Errorf("approval = %+v, want pending gate", a)
	}
	if a.Title != "Approve this?" || len(a.AllowedDecisions) != 2 {
		t.Errorf("approval content = %+v, want the rendered gate", a)
	}
	if a.TimeoutAt == nil {
		t.Errorf("approval timeout_at is nil, want the persisted deadline")
	}
}
