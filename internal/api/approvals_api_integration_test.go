//go:build integration

package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mathcslearner/agentloom/internal/api"
	"github.com/mathcslearner/agentloom/internal/dag"
	"github.com/mathcslearner/agentloom/internal/store"
	"github.com/mathcslearner/agentloom/internal/store/storetest"
)

// gateEditDef: a human_approval gate (allow_edit + edit schema) feeding an
// echo publish — enough to decide over HTTP and observe a readied successor.
const gateEditDef = `{
  "schema_version": 1,
  "name": "gate-edit",
  "steps": [
    {"id": "gate", "type": "human_approval",
     "config": {"title": "Approve?", "payload": {"text": "proposed"},
       "allowed_decisions": ["approve", "reject"], "allow_edit": true,
       "edit_schema": {"type": "object", "properties": {"text": {"type": "string"}}}}},
    {"id": "publish", "type": "echo", "config": {"input": {"ok": true}}}
  ],
  "edges": [{"from": "gate", "to": "publish"}]
}`

// approvalsServer boots the API with a known root key, then mints an
// approve+read key and a read-only key — the credentials the decision API's
// authz needs.
func approvalsServer(t *testing.T) (*store.Store, *httptest.Server, string, string) {
	t.Helper()
	pool := storetest.NewDB(t)
	s := store.NewFromPool(pool)
	root := mintTestKey(t)
	h, err := api.New(s, func() time.Time { return testNow }, nil, root, api.RateLimitOptions{})
	if err != nil {
		t.Fatalf("api.New: %v", err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	approver := createKey(t, srv, root, api.CreateKeyRequest{Name: "approver", Scopes: []string{"read", "approve"}})
	readonly := createKey(t, srv, root, api.CreateKeyRequest{Name: "readonly", Scopes: []string{"read"}})
	return s, srv, approver.Key, readonly.Key
}

// parkGateViaStore claims the gate and parks it (the store half of the
// executor path), returning the run id and the pending approval id.
func parkGateViaStore(t *testing.T, s *store.Store) (uuid.UUID, uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC()
	d, err := dag.Decode([]byte(gateEditDef))
	if err != nil {
		t.Fatalf("decode def: %v", err)
	}
	res, err := s.CreateRun(ctx, store.CreateRunArgs{Definition: d, Now: now})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	runID := res.Run.ID
	approvalID := uuid.New()
	if err := s.WithTx(ctx, func(ctx context.Context, q store.Querier) error {
		step, cerr := store.ClaimStep(ctx, q, store.ClaimStepArgs{RunID: runID, StepID: "gate", Now: now})
		if cerr != nil {
			return cerr
		}
		_, perr := store.AwaitHumanStep(ctx, q, store.AwaitHumanStepArgs{
			RunID: runID, StepID: "gate", ClaimID: *step.ClaimID, Now: now,
			Approval: store.ApprovalRow{
				ID: approvalID, Title: "Approve?", Payload: []byte(`{"text":"proposed"}`),
				AllowedDecisions: []string{"approve", "reject"}, AllowEdit: true,
				EditSchema: []byte(`{"type":"object","properties":{"text":{"type":"string"}}}`),
			},
		})
		return perr
	}); err != nil {
		t.Fatalf("park gate: %v", err)
	}
	return runID, approvalID
}

func decidePath(id uuid.UUID) string { return "/v1/approvals/" + id.String() + ":decide" }

// TestListApprovalsInbox: the pending-approval inbox surfaces the parked gate
// with its run id and filters by status.
func TestListApprovalsInbox(t *testing.T) {
	t.Parallel()
	s, srv, approver, _ := approvalsServer(t)
	runID, approvalID := parkGateViaStore(t, s)

	var list api.ApprovalListResponse
	if code := getJSON(t, srv, approver, "/v1/approvals?status=pending", &list); code != http.StatusOK {
		t.Fatalf("GET /v1/approvals = %d, want 200", code)
	}
	if len(list.Approvals) != 1 {
		t.Fatalf("approvals = %+v, want one pending", list.Approvals)
	}
	a := list.Approvals[0]
	if a.ID != approvalID.String() || a.RunID != runID.String() || a.StepID != "gate" {
		t.Errorf("approval = %+v, want the parked gate with run id", a)
	}

	// A filter that excludes it returns an empty page.
	var none api.ApprovalListResponse
	getJSON(t, srv, approver, "/v1/approvals?status=approved", &none)
	if len(none.Approvals) != 0 {
		t.Errorf("status=approved returned %d, want 0", len(none.Approvals))
	}
}

// TestDecideRequiresApproveScope: a read-only key is forbidden from deciding.
func TestDecideRequiresApproveScope(t *testing.T) {
	t.Parallel()
	s, srv, _, readonly := approvalsServer(t)
	_, approvalID := parkGateViaStore(t, s)

	body, _ := json.Marshal(api.DecideApprovalRequest{Decision: "approve"})
	res := doAuth(t, srv, http.MethodPost, decidePath(approvalID), readonly, body, nil)
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("decide with read-only key = %d, want 403", res.StatusCode)
	}
}

// TestDecideApproveOverHTTP: an approve decision returns 200 with the decided
// approval (attributed to the key) and the readied successor.
func TestDecideApproveOverHTTP(t *testing.T) {
	t.Parallel()
	s, srv, approver, _ := approvalsServer(t)
	runID, approvalID := parkGateViaStore(t, s)

	body, _ := json.Marshal(api.DecideApprovalRequest{Decision: "approve", Comment: "lgtm"})
	var resp api.DecideApprovalResponse
	res := doAuth(t, srv, http.MethodPost, decidePath(approvalID), approver, body, &resp)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("decide approve = %d, want 200", res.StatusCode)
	}
	if resp.Approval.Status != store.ApprovalStatusApproved || resp.Approval.Decision != "approve" {
		t.Errorf("decided approval = %+v, want approved/approve", resp.Approval)
	}
	if resp.Approval.DecidedBy == "" || resp.Approval.DecisionSource != store.ApprovalSourceHuman {
		t.Errorf("decision attribution = %+v, want a key id + human source", resp.Approval)
	}
	if len(resp.ReadiedSteps) != 1 || resp.ReadiedSteps[0] != "publish" {
		t.Errorf("readied = %v, want [publish]", resp.ReadiedSteps)
	}

	// Audit: the decision is immutable in the run view.
	var run api.RunResponse
	getJSON(t, srv, approver, "/v1/runs/"+runID.String(), &run)
	if len(run.Approvals) != 1 || run.Approvals[0].Status != store.ApprovalStatusApproved {
		t.Errorf("run view approvals = %+v, want one approved", run.Approvals)
	}
	if run.Approvals[0].Comment != "lgtm" {
		t.Errorf("comment = %q, want lgtm", run.Approvals[0].Comment)
	}
}

// TestDecideInvalidEditReturns422: an edited payload violating the edit schema
// is 422 with issues; the approval stays pending.
func TestDecideInvalidEditReturns422(t *testing.T) {
	t.Parallel()
	s, srv, approver, _ := approvalsServer(t)
	_, approvalID := parkGateViaStore(t, s)

	body, _ := json.Marshal(api.DecideApprovalRequest{
		Decision: "approve", EditedPayload: json.RawMessage(`{"text": 123}`),
	})
	var errResp api.ErrorBody
	res := doAuth(t, srv, http.MethodPost, decidePath(approvalID), approver, body, &errResp)
	if res.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("invalid edit = %d, want 422", res.StatusCode)
	}
	if errResp.Error.Code != api.ErrCodeApprovalDecisionInvalid || len(errResp.Error.Issues) == 0 {
		t.Errorf("error = %+v, want approval_decision_invalid with issues", errResp.Error)
	}
}

// TestDecideDoubleDecideReturns409: a second decision on a decided approval is
// 409 approval_not_pending.
func TestDecideDoubleDecideReturns409(t *testing.T) {
	t.Parallel()
	s, srv, approver, _ := approvalsServer(t)
	_, approvalID := parkGateViaStore(t, s)

	body, _ := json.Marshal(api.DecideApprovalRequest{Decision: "approve"})
	if res := doAuth(t, srv, http.MethodPost, decidePath(approvalID), approver, body, nil); res.StatusCode != http.StatusOK {
		t.Fatalf("first decide = %d, want 200", res.StatusCode)
	}
	var errResp api.ErrorBody
	res := doAuth(t, srv, http.MethodPost, decidePath(approvalID), approver, body, &errResp)
	if res.StatusCode != http.StatusConflict || errResp.Error.Code != api.ErrCodeApprovalNotPending {
		t.Fatalf("second decide = %d code %q, want 409 approval_not_pending", res.StatusCode, errResp.Error.Code)
	}
}

// TestDecideUnknownReturns404: deciding a non-existent approval is 404.
func TestDecideUnknownReturns404(t *testing.T) {
	t.Parallel()
	_, srv, approver, _ := approvalsServer(t)

	body, _ := json.Marshal(api.DecideApprovalRequest{Decision: "approve"})
	var errResp api.ErrorBody
	res := doAuth(t, srv, http.MethodPost, decidePath(uuid.New()), approver, body, &errResp)
	if res.StatusCode != http.StatusNotFound || errResp.Error.Code != api.ErrCodeApprovalNotFound {
		t.Fatalf("unknown decide = %d code %q, want 404 approval_not_found", res.StatusCode, errResp.Error.Code)
	}
}

// TestExpiredApprovalSurfacedInInbox (ticket 15.4): a timeout-expired approval
// is filterable by status=expired and its view carries expired_at plus the
// timeout-sourced decision — the audit surface for an automatic expiry.
func TestExpiredApprovalSurfacedInInbox(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, srv, approver, _ := approvalsServer(t)
	runID, approvalID := parkGateViaStore(t, s)

	// Apply a reject timeout policy at the store level (the worker's CAS):
	// pending → expired with a timeout source.
	now := time.Now().UTC()
	if err := s.WithTx(ctx, func(ctx context.Context, q store.Querier) error {
		_, derr := store.DecideApproval(ctx, q, store.DecideApprovalArgs{
			ID: approvalID, RunID: runID, StepID: "gate", AttemptNo: 1,
			Status: store.ApprovalStatusExpired, Decision: "reject",
			DecidedBy: store.ApprovalActorTimeout, Source: store.ApprovalSourceTimeout,
			ExpiredAt: &now, Now: now,
		})
		return derr
	}); err != nil {
		t.Fatalf("expire approval: %v", err)
	}

	var list api.ApprovalListResponse
	if code := getJSON(t, srv, approver, "/v1/approvals?status=expired", &list); code != http.StatusOK {
		t.Fatalf("GET /v1/approvals?status=expired = %d, want 200", code)
	}
	if len(list.Approvals) != 1 {
		t.Fatalf("expired approvals = %+v, want one", list.Approvals)
	}
	a := list.Approvals[0]
	if a.ID != approvalID.String() || a.Status != store.ApprovalStatusExpired {
		t.Errorf("approval = %+v, want the expired gate", a)
	}
	if a.ExpiredAt == nil {
		t.Error("view is missing expired_at")
	}
	if a.DecisionSource != store.ApprovalSourceTimeout || a.DecidedBy != store.ApprovalActorTimeout {
		t.Errorf("decision fields = source %q by %q, want timeout / system:timeout", a.DecisionSource, a.DecidedBy)
	}
}
