package engine

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mathcslearner/agentloom/internal/notify"
	"github.com/mathcslearner/agentloom/internal/store"
	"github.com/mathcslearner/agentloom/internal/store/gen"
)

// TestBuildApprovalNotificationGolden pins the wire shape of the approval
// notification — the contract the M18 inbox and any external receiver consume.
// Regenerate with UPDATE_GOLDEN=1.
func TestBuildApprovalNotificationGolden(t *testing.T) {
	runID := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	approvalID := uuid.MustParse("22222222-2222-2222-2222-222222222222")
	timeoutAt := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	createdAt := time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC)

	step := gen.RunStep{RunID: runID, StepID: "approve_publish", AttemptCount: 1}
	approval := store.ApprovalRow{
		ID:               approvalID,
		Title:            "Publish this article?",
		Description:      "Review the draft before it goes live.",
		Payload:          json.RawMessage(`{"text":"An article about turtles."}`),
		AllowedDecisions: []string{"approve", "reject"},
		AllowEdit:        true,
		EditSchema:       json.RawMessage(`{"type":"object","properties":{"text":{"type":"string"}},"required":["text"]}`),
		TimeoutAt:        &timeoutAt,
	}

	n := buildApprovalNotification(step, approval, "research-critic-writer", createdAt)

	// Structural invariants beyond the golden bytes.
	if n.SchemaVersion != notify.SchemaVersion || n.Event != notify.EventApprovalRequested {
		t.Errorf("envelope = {%d, %q}", n.SchemaVersion, n.Event)
	}
	if n.DeliveryID() != approvalID.String() {
		t.Errorf("delivery id = %q, want the approval id", n.DeliveryID())
	}
	if n.Links.Decide != "/v1/approvals/"+approvalID.String()+":decide" {
		t.Errorf("decide link = %q", n.Links.Decide)
	}

	got, err := json.MarshalIndent(n, "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	got = append(got, '\n')

	golden := filepath.Join("testdata", "approval_notification.json")
	if os.Getenv("UPDATE_GOLDEN") == "1" {
		if err := os.WriteFile(golden, got, 0o600); err != nil {
			t.Fatalf("writing golden: %v", err)
		}
	}
	want, err := os.ReadFile(golden) // #nosec G304 -- committed fixture path, test-only
	if err != nil {
		t.Fatalf("reading golden (run with UPDATE_GOLDEN=1 to create): %v", err)
	}
	if string(got) != string(want) {
		t.Errorf("notification does not match golden %s\n--- got ---\n%s\n--- want ---\n%s", golden, got, want)
	}
}
