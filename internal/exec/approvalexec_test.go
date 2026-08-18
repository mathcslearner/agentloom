package exec

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/mathcslearner/agentloom/internal/dag"
)

// approvalCtx builds a human_approval StepContext from a config map.
func approvalCtx(t *testing.T, cfg map[string]any) StepContext {
	t.Helper()
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshaling config: %v", err)
	}
	return StepContext{StepType: dag.StepHumanApproval, Config: raw, Attempt: 1}
}

// TestHumanApprovalIdentity pins the executor's ADR-009 identity and flags.
func TestHumanApprovalIdentity(t *testing.T) {
	t.Parallel()
	e := HumanApprovalExecutor{}
	if e.Type() != "human_approval" {
		t.Errorf("Type = %q, want human_approval", e.Type())
	}
	m := e.PluginManifest()
	if !m.Capabilities.SideEffectful {
		t.Error("human_approval should be side_effectful (uncacheable)")
	}
	if m.Capabilities.Cacheable || m.Capabilities.CostBearing {
		t.Errorf("human_approval caps = %+v, want side_effectful only", m.Capabilities)
	}
}

// TestHumanApprovalDefaults: an empty allowed_decisions resolves to the
// engine default [approve, reject], and the produced request round-trips the
// rendered content.
func TestHumanApprovalDefaults(t *testing.T) {
	t.Parallel()
	out, err := HumanApprovalExecutor{}.Execute(context.Background(), approvalCtx(t, map[string]any{
		"title":   "Publish?",
		"payload": map[string]any{"text": "body"},
	}))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	var req ApprovalRequest
	if err := json.Unmarshal(out.Data, &req); err != nil {
		t.Fatalf("decoding request: %v", err)
	}
	if req.Title != "Publish?" {
		t.Errorf("title = %q, want Publish?", req.Title)
	}
	if len(req.AllowedDecisions) != 2 || req.AllowedDecisions[0] != "approve" || req.AllowedDecisions[1] != "reject" {
		t.Errorf("allowed_decisions = %v, want default [approve reject]", req.AllowedDecisions)
	}
	if req.Timeout != 0 {
		t.Errorf("timeout = %v, want 0 (indefinite)", req.Timeout)
	}
}

// TestHumanApprovalTimeoutParsed: a valid timeout is parsed into a duration
// the engine adds to now.
func TestHumanApprovalTimeoutParsed(t *testing.T) {
	t.Parallel()
	out, err := HumanApprovalExecutor{}.Execute(context.Background(), approvalCtx(t, map[string]any{
		"title":   "Publish?",
		"timeout": "2h",
	}))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	var req ApprovalRequest
	if err := json.Unmarshal(out.Data, &req); err != nil {
		t.Fatalf("decoding request: %v", err)
	}
	if req.Timeout != 2*time.Hour {
		t.Errorf("timeout = %v, want 2h", req.Timeout)
	}
}

// TestHumanApprovalEditSchemaCompiled: an uncompilable edit schema fails
// permanently before the step parks (the claim-pre-flight gate).
func TestHumanApprovalEditSchema(t *testing.T) {
	t.Parallel()
	t.Run("valid schema passes", func(t *testing.T) {
		t.Parallel()
		_, err := HumanApprovalExecutor{}.Execute(context.Background(), approvalCtx(t, map[string]any{
			"title":       "Publish?",
			"allow_edit":  true,
			"edit_schema": map[string]any{"type": "object"},
		}))
		if err != nil {
			t.Errorf("valid edit_schema: %v", err)
		}
	})
	t.Run("uncompilable schema is permanent", func(t *testing.T) {
		t.Parallel()
		// A non-object "type" makes the schema invalid to compile.
		_, err := HumanApprovalExecutor{}.Execute(context.Background(), approvalCtx(t, map[string]any{
			"title":       "Publish?",
			"allow_edit":  true,
			"edit_schema": map[string]any{"type": "not-a-real-type"},
		}))
		if err == nil {
			t.Fatal("uncompilable edit_schema accepted, want a permanent error")
		}
		var ce *ClassifiedError
		if !errors.As(err, &ce) || ce.Class != dag.ClassPermanent {
			t.Errorf("error = %v, want a permanent ClassifiedError", err)
		}
	})
}

// TestHumanApprovalMissingTitle: a config without a title fails permanent
// (defensive — 15.1 validation already requires it at submit time).
func TestHumanApprovalMissingTitle(t *testing.T) {
	t.Parallel()
	_, err := HumanApprovalExecutor{}.Execute(context.Background(), approvalCtx(t, map[string]any{
		"description": "no title here",
	}))
	if err == nil {
		t.Fatal("missing title accepted, want a permanent error")
	}
	var ce *ClassifiedError
	if !errors.As(err, &ce) || ce.Class != dag.ClassPermanent {
		t.Errorf("error = %v, want a permanent ClassifiedError", err)
	}
}
