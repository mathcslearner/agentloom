package api

// Contract golden for the approval inbox + decision UI (ticket 18.5). Like the
// run-detail fixtures (18.3) this builds ApprovalView rows over fixed data — no
// database, deterministic timestamps — and pins the wire shape against
// committed JSON the frontend approvals tests read as ground truth. Regenerate
// with UPDATE_GOLDEN=1.
//
//   TestApprovalListFixtureGolden -> testdata/approval_list_fixture.json
//
// The fixture exercises every inbox/decision case in one page:
//   - a pending gate with an edit_schema + a timeout (the decidable case);
//   - an approved-with-edit gate (edited_payload + comment + decided_by);
//   - a rejected gate (routed or failed downstream);
//   - an expired gate (a timeout policy fired: status expired);
//   - a park-expired gate (on_timeout: park — expired_at is stamped but the
//     status stays pending, so it is STILL decidable — the inbox must render
//     this distinctly from a plain expired row).

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mathcslearner/agentloom/internal/store/gen"
)

func TestApprovalListFixtureGolden(t *testing.T) {
	t.Parallel()

	runID := uuid.MustParse("33333333-3333-3333-3333-333333333333")
	t0 := time.Date(2026, 8, 18, 9, 0, 0, 0, time.UTC)
	at := func(sec int) *time.Time { u := t0.Add(time.Duration(sec) * time.Second); return &u }
	sptr := func(s string) *string { return &s }
	id := func(n string) uuid.UUID {
		return uuid.MustParse("a0000000-0000-0000-0000-00000000000" + n)
	}

	editSchema := json.RawMessage(`{"type":"object","properties":{"text":{"type":"string"}},"required":["text"]}`)

	rows := []gen.Approval{
		// pending: the decidable case, with an edit schema and a timeout.
		{
			ID: id("1"), RunID: runID, StepID: "approve_publish", Attempt: 1,
			Status: "pending", Title: "Publish the article?",
			Description:      "Review the draft before it goes live.",
			Payload:          json.RawMessage(`{"text":"Launch blurb. APPROVED"}`),
			AllowedDecisions: []string{"approve", "reject"},
			AllowEdit:        true, EditSchema: editSchema,
			TimeoutAt: at(86400), CreatedAt: t0,
		},
		// approved with an edit: full decision fields.
		{
			ID: id("2"), RunID: runID, StepID: "approve_a", Attempt: 1,
			Status: "approved", Title: "Approve step a",
			Payload:          json.RawMessage(`{"text":"draft"}`),
			AllowedDecisions: []string{"approve", "reject"},
			AllowEdit:        true,
			Decision:         sptr("approve"),
			EditedPayload:    json.RawMessage(`{"text":"final edited"}`),
			Comment:          sptr("looks good with a tweak"),
			DecidedBy:        sptr("key_admin"), DecidedAt: at(60),
			DecisionSource: sptr("human"),
			CreatedAt:      t0.Add(1 * time.Second),
		},
		// rejected.
		{
			ID: id("3"), RunID: runID, StepID: "approve_b", Attempt: 1,
			Status: "rejected", Title: "Approve step b",
			AllowedDecisions: []string{"approve", "reject"},
			Decision:         sptr("reject"), Comment: sptr("not ready"),
			DecidedBy: sptr("key_admin"), DecidedAt: at(70),
			DecisionSource: sptr("human"),
			CreatedAt:      t0.Add(2 * time.Second),
		},
		// expired: a timeout policy fired (on_timeout: reject); status expired.
		{
			ID: id("4"), RunID: runID, StepID: "approve_c", Attempt: 1,
			Status: "expired", Title: "Approve step c",
			AllowedDecisions: []string{"approve", "reject"},
			Decision:         sptr("reject"),
			DecidedBy:        sptr("system:timeout"), DecidedAt: at(80),
			DecisionSource: sptr("timeout"),
			TimeoutAt:      at(80), ExpiredAt: at(80),
			CreatedAt: t0.Add(3 * time.Second),
		},
		// park-expired: on_timeout: park — expired_at is stamped but status
		// stays pending, so this row is STILL decidable.
		{
			ID: id("5"), RunID: runID, StepID: "approve_d", Attempt: 1,
			Status: "pending", Title: "Approve step d",
			AllowedDecisions: []string{"approve", "reject"},
			TimeoutAt:        at(50), ExpiredAt: at(50),
			CreatedAt: t0.Add(4 * time.Second),
		},
	}

	resp := ApprovalListResponse{Approvals: []ApprovalView{}}
	for _, a := range rows {
		v := buildApprovalView(a)
		v.RunID = a.RunID.String()
		resp.Approvals = append(resp.Approvals, v)
	}
	assertGolden(t, "approval_list_fixture.json", resp)
}
