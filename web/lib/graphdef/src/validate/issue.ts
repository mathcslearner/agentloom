// The client-side validation issue model (ADR-019 §"Validation parity"). Wire-
// compatible with the backend's `Issue` (internal/api/types.go): a stable
// machine `code` (empty for shape/decode-level findings, matching the backend's
// codeless *dag.DecodeError), a `severity`, a JSON `path` in the backend's
// grammar (`steps[3].config.model`), and a human `msg`. So a client verdict and
// a server verdict name the same problem in the same place, and a server 400's
// issues[] map onto the same nodes by path.

/** Issue severity — `error` blocks submit, `warning` does not. */
export type Severity = "error" | "warning";

/** A single validation finding. */
export interface Issue {
  /** Stable machine code; "" for shape/decode-level findings (no ValidationCode). */
  code: string;
  severity: Severity;
  /** JSON path in the backend grammar, e.g. `steps[3].config.messages[0].content`. */
  path: string;
  msg: string;
  /**
   * Client-only: extra definition paths this issue implicates (e.g. every
   * `steps[i]`/`edges[i]` on a cycle), so the Problems panel can highlight the
   * whole offending shape. Never sent to or received from the backend.
   */
  related?: string[];
}

/** Construct an error-severity issue. */
export function err(code: string, path: string, msg: string, related?: string[]): Issue {
  return related ? { code, severity: "error", path, msg, related } : { code, severity: "error", path, msg };
}

/** Construct a warning-severity issue. */
export function warn(code: string, path: string, msg: string, related?: string[]): Issue {
  return related ? { code, severity: "warning", path, msg, related } : { code, severity: "warning", path, msg };
}

/** True if any issue is error-severity (the accept/reject signal). */
export function hasErrors(issues: readonly Issue[]): boolean {
  return issues.some((i) => i.severity === "error");
}

/**
 * The validation code vocabulary the client reproduces. It mirrors the backend
 * ValidationCode constants (internal/dag/validation.go and siblings), plus the
 * client-only advisory codes (prefixed `advisory_`) that surface warnings the
 * backend does not model. Decode-level shape findings carry the empty code,
 * matching the backend's codeless *dag.DecodeError.
 */
export const Code = {
  // Steps / identity.
  NoSteps: "no_steps",
  DuplicateStepID: "duplicate_step_id",
  InvalidStepID: "invalid_step_id",
  UnknownStepType: "unknown_step_type",
  // Config.
  ConfigFieldRequired: "config_field_required",
  ConfigFieldConflict: "config_field_conflict",
  ConfigFieldInvalid: "config_field_invalid",
  // Edges / graph.
  UnknownEdgeEndpoint: "unknown_edge_endpoint",
  NoEntryStep: "no_entry_step",
  IsolatedStep: "isolated_step",
  BranchNoOutEdges: "branch_no_out_edges",
  BranchEdgeUnconditioned: "branch_edge_unconditioned",
  LoopFieldRequired: "loop_field_required",
  LoopFieldForbidden: "loop_field_forbidden",
  Cycle: "cycle_detected",
  LoopEdgeNotAncestor: "loop_edge_not_ancestor",
  LimitExceeded: "limit_exceeded",
  // Envelope blocks.
  RetryFieldRequired: "retry_field_required",
  RetryFieldInvalid: "retry_field_invalid",
  TimeoutFieldInvalid: "timeout_field_invalid",
  MaxWallClockInvalid: "max_wall_clock_field_invalid",
  CacheFieldRequired: "cache_field_required",
  CacheFieldInvalid: "cache_field_invalid",
  BudgetFieldRequired: "budget_field_required",
  BudgetFieldInvalid: "budget_field_invalid",
  ValidationFieldRequired: "validation_field_required",
  ValidationFieldInvalid: "validation_field_invalid",
  BlackboardFieldRequired: "blackboard_field_required",
  BlackboardFieldInvalid: "blackboard_field_invalid",
  ContextFieldRequired: "context_field_required",
  ContextFieldInvalid: "context_field_invalid",
  // Expansion / templates / agents / approvals.
  ExpansionFieldInvalid: "expansion_field_invalid",
  TemplateSectionInvalid: "template_section_invalid",
  MapBodyUnknown: "map_body_unknown",
  AgentSectionInvalid: "agent_section_invalid",
  AgentRefUnknown: "agent_ref_unknown",
  ApprovalEdgeInvalid: "approval_edge_invalid",
  ApprovalRejectEdgeRequired: "approval_reject_edge_required",
  // Expressions.
  ExprInvalid: "invalid_expression",
  ExprNotBool: "expression_not_boolean",
  // Templating.
  TemplateInvalid: "template_invalid",
  TemplateRefInvalid: "template_ref_invalid",
  TemplateRefUnknownStep: "template_ref_unknown_step",
  TemplateRefNotUpstream: "template_ref_not_upstream",
  TemplateRefUnknownParam: "template_ref_unknown_param",
  // Client-only advisory warnings (never a backend code; excluded from parity).
  AdvisoryBudget: "advisory_budget",
} as const;
