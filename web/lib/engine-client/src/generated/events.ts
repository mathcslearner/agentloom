// Code generated from docs/schema/events.v1.json — DO NOT EDIT.
//
// Regenerate with `pnpm generate` (web/lib/engine-client). The Go event
// structs are the source of truth (ADR-018): `make generate` emits the JSON
// Schema, this file is emitted from it, and CI fails if it is stale.

export interface ApprovalCancelled {
  approval_id: string;
  step_id: string;
  reason: string;
}

export interface ApprovalDecided {
  approval_id: string;
  step_id: string;
  attempt: number;
  decision: string;
  edited?: boolean;
  comment?: string;
  decided_by: string;
  source: string;
}

export type ApprovalDecision = "approve" | "reject";

export interface ApprovalExpired {
  approval_id: string;
  step_id: string;
  attempt: number;
  policy: string;
  decision?: string;
  action: string;
  timeout_at?: string;
}

export interface ApprovalNotificationFailed {
  approval_id: string;
  step_id: string;
  target_host: string;
  attempts: number;
  reason: string;
}

export interface ApprovalNotified {
  approval_id: string;
  step_id: string;
  target_host: string;
  attempts: number;
  status_code: number;
}

export interface ApprovalRequested {
  approval_id: string;
  step_id: string;
  attempt: number;
  title: string;
  allowed_decisions: string[];
  allow_edit?: boolean;
  timeout_at?: string;
}

export interface BackoffSpec {
  initial?: string;
  cap?: string;
  multiplier?: number;
}

export interface BlackboardPolicy {
  write?: BlackboardWrite[];
}

export interface BlackboardUpdated {
  key: string;
  version: number;
  tags: string[];
  token_count: number;
  author_step_id?: string;
  author_attempt?: number;
}

export interface BlackboardWrite {
  key: string;
  from?: string;
  tags?: string[];
  pinned?: boolean;
}

export interface BudgetExceeded {
  step_id: string;
  attempt: number;
  resource?: string;
  limit: string;
  action: string;
  spent_nano_usd?: number;
  estimate_nano_usd?: number;
  projected_nano_usd?: number;
  budget_nano_usd?: number;
  projected_tokens?: number;
  max_tokens?: number;
}

export type CacheMode = "off" | "read_write" | "read_only";

export interface CachePolicy {
  mode?: CacheMode;
  ttl?: string;
  scope?: CacheScope;
}

export type CacheScope = "global" | "run";

export interface CompactionStrategy {
  strategy: string;
  n?: number;
  min_tokens?: number;
  model?: string;
  key?: string;
  max_tokens?: number;
  timeout?: string;
}

export interface ContextAssembled {
  step_id: string;
  attempt: number;
  counter_id: string;
  sources: ContextSourceRecord[];
  context_tokens: number;
  preflight_tokens: number;
  budget_tokens?: number;
  budget_source?: string;
  context_window?: number;
  raw_context_tokens?: number;
  raw_preflight_tokens?: number;
  revisions?: number;
  summaries?: number;
}

export type ContextMissingPolicy = "error" | "skip";

export interface ContextRevision {
  step_id: string;
  attempt: number;
  index: number;
  strategy: string;
  n?: number;
  min_tokens?: number;
  budget: number;
  tokens_before: number;
  tokens_after: number;
  changed: boolean;
  actions?: ContextRevisionActionRecord[];
  summaries?: ContextRevisionSummaryRecord[];
  error?: string;
  kept?: string[];
}

export interface ContextRevisionActionRecord {
  source_index: number;
  name: string;
  action: string;
  tokens_before: number;
  tokens_after: number;
}

export interface ContextRevisionSummaryRecord {
  key: string;
  version: number;
  parent_version?: number;
  model: string;
  resource: string;
  span_names?: string[];
  span_tokens: number;
  summary_tokens: number;
  cache_hit?: boolean;
  input_tokens: number;
  output_tokens: number;
}

export interface ContextSource {
  kind: ContextSourceKind;
  name?: string;
  step?: string;
  path?: string;
  key?: string;
  role?: string;
  tags?: string[];
  retriever?: string;
  query?: string;
  top_k?: number;
  text?: string;
  max_tokens?: number;
  pinned?: boolean;
  priority?: number;
  on_missing?: ContextMissingPolicy;
}

export type ContextSourceKind = "step_output" | "blackboard" | "retrieval" | "literal" | "thread";

export interface ContextSourceRecord {
  index: number;
  kind: string;
  name: string;
  ref?: string;
  status: string;
  reason?: string;
  tokens: number;
  pinned?: boolean;
}

export interface ContextSpec {
  sources?: ContextSource[];
  budget_tokens?: number;
  compaction?: CompactionStrategy[];
}

export interface CostUnknownModel {
  model: string;
  fallback: Rate;
}

export interface CostUpdated {
  step_id: string;
  attempt: number;
  entry: string;
  resource: string;
  cache_hit?: boolean;
  overhead?: boolean;
  cost_nano_usd?: number;
  saved_nano_usd?: number;
  run_spent_nano_usd: number;
  run_saved_nano_usd: number;
  budget_nano_usd?: number;
}

export interface Edge {
  from: string;
  to: string;
  when?: string;
  type?: EdgeType;
  condition?: string;
  max_iterations?: number;
  on_exhausted?: ExhaustPolicy;
  no_progress?: NoProgressPolicy;
  decision?: ApprovalDecision;
}

export type EdgeType = "normal" | "loop";

export type ErrorClass = "transient" | "permanent" | "timeout" | "cancelled" | "validation_failed";

export type ExhaustPolicy = "proceed" | "fail";

export interface FeedbackPolicy {
  template?: string;
  max_output_chars?: number;
}

export interface GraphExpanded {
  origin_step: string;
  origin_kind: string;
  from_version: number;
  to_version: number;
  depth: number;
  delta: PlanOutput;
  readied?: string[];
  widened?: string[];
}

export interface GuardTripped {
  guard: string;
  step_id?: string;
  current: number;
  cap: number;
  unit: string;
  action: string;
}

export type JitterMode = "full" | "none";

export interface LoopExhausted {
  loop_source_step: string;
  loop_source_instance: string;
  body_entry: string;
  iteration: number;
  max_iterations: number;
  condition: string;
  policy: string;
  action: string;
}

export interface LoopNoProgress {
  loop_source_step: string;
  loop_source_instance: string;
  compared_step: string;
  path?: string;
  iteration: number;
  prev_instance: string;
  cur_instance: string;
  hash: string;
  policy: string;
  action: string;
}

export interface ModelDowngraded {
  step_id: string;
  attempt: number;
  from_model: string;
  to_model: string;
  from_resource: string;
  to_resource: string;
  trigger: string;
  limit?: string;
  threshold_fraction?: number;
  spent_nano_usd?: number;
  budget_nano_usd?: number;
  from_estimate_nano_usd?: number;
  to_estimate_nano_usd?: number;
}

export interface NoProgressPolicy {
  step?: string;
  path?: string;
  policy?: ExhaustPolicy;
}

export interface PlanOutput {
  schema_version: number;
  steps: Step[];
  edges?: Edge[];
}

export interface Rate {
  input_per_mtok: number;
  output_per_mtok: number;
}

export interface RetryPolicy {
  max_attempts?: number;
  backoff?: BackoffSpec;
  jitter?: JitterMode;
  retry_on?: ErrorClass[];
}

export interface RunBudgetUpdated {
  previous_nano_usd: number;
  budget_nano_usd: number;
}

export interface RunCancelled {

}

export interface RunCancelling {
  reason: string;
}

export interface RunCreated {
  name: string;
  definition_id?: string;
  steps_total: number;
}

export interface RunFailed {

}

export interface RunParked {
  reason: string;
}

export interface RunResumed {

}

export interface RunSucceeded {

}

export interface RunUnparked {

}

export interface Step {
  id: string;
  type: StepType;
  config?: unknown;
  retry?: RetryPolicy;
  timeout?: string;
  cache?: CachePolicy;
  budget?: StepBudget;
  validation?: ValidationPolicy;
  blackboard?: BlackboardPolicy;
  context?: ContextSpec;
}

export interface StepBudget {
  max_usd?: number;
  max_tokens?: number;
}

export interface StepCancelled {
  step_id: string;
  reason: string;
}

export interface StepClaimed {
  step_id: string;
  claim_id: string;
  attempt: number;
}

export interface StepCollected {
  step_id: string;
  class?: string;
  attempts: number;
}

export interface StepDeadLettered {
  step_id: string;
  source: string;
  class?: string;
  attempts: number;
  seq: number;
}

export interface StepFailed {
  step_id: string;
  attempt: number;
}

export interface StepReady {
  step_id: string;
}

export interface StepReclaimed {
  step_id: string;
  claim_id: string;
  attempt: number;
}

export interface StepRequeued {
  step_id: string;
}

export interface StepRetryScheduled {
  step_id: string;
  attempt: number;
  class: string;
  next_attempt_at: string;
}

export interface StepRevived {
  step_id: string;
  reason: string;
}

export interface StepSemanticRetry {
  step_id: string;
  attempt: number;
  semantic_attempt: number;
  max_attempts: number;
  issue_count: number;
  next_attempt_at: string;
}

export interface StepSkipped {
  step_id: string;
}

export interface StepSucceeded {
  step_id: string;
  attempt: number;
}

export interface StepThrottled {
  step_id: string;
  attempt: number;
  resource: string;
  bucket: string;
  retry_after: string;
  next_attempt_at: string;
}

export type StepType = "llm" | "tool" | "retrieve" | "map" | "gather" | "planner" | "agent" | "human_approval" | "join" | "branch" | "noop" | "echo" | "sleep" | "fail_n_times" | "counter" | "effectful_echo" | "blackboard_write";

export type UUID = string;

export interface ValidationPolicy {
  validators?: ValidatorSpec[];
  max_attempts?: number;
  feedback?: FeedbackPolicy;
}

export interface ValidatorSpec {
  name: string;
  config?: unknown;
  target?: string;
}

/** Every event type in the feed vocabulary (ADR-018), in catalog order. */
export const EVENT_TYPES = [
  "run_created",
  "step_ready",
  "step_claimed",
  "step_succeeded",
  "step_failed",
  "step_skipped",
  "step_reclaimed",
  "step_retry_scheduled",
  "step_throttled",
  "step_semantic_retry_scheduled",
  "step_dead_lettered",
  "step_cancelled",
  "step_collected",
  "step_requeued",
  "step_revived",
  "run_succeeded",
  "run_failed",
  "run_resumed",
  "run_parked",
  "run_unparked",
  "run_cancelling",
  "run_cancelled",
  "cost_updated",
  "cost_unknown_model",
  "budget_exceeded",
  "run_budget_updated",
  "model_downgraded",
  "blackboard_updated",
  "context_assembled",
  "context_revision",
  "graph_expanded",
  "loop_exhausted",
  "loop_no_progress",
  "guard_tripped",
  "approval_requested",
  "approval_cancelled",
  "approval_decided",
  "approval_expired",
  "approval_notified",
  "approval_notification_failed",
] as const;

export type EventType = (typeof EVENT_TYPES)[number];

/** Maps each event type to its payload struct. */
export interface EventPayloadMap {
  run_created: RunCreated;
  step_ready: StepReady;
  step_claimed: StepClaimed;
  step_succeeded: StepSucceeded;
  step_failed: StepFailed;
  step_skipped: StepSkipped;
  step_reclaimed: StepReclaimed;
  step_retry_scheduled: StepRetryScheduled;
  step_throttled: StepThrottled;
  step_semantic_retry_scheduled: StepSemanticRetry;
  step_dead_lettered: StepDeadLettered;
  step_cancelled: StepCancelled;
  step_collected: StepCollected;
  step_requeued: StepRequeued;
  step_revived: StepRevived;
  run_succeeded: RunSucceeded;
  run_failed: RunFailed;
  run_resumed: RunResumed;
  run_parked: RunParked;
  run_unparked: RunUnparked;
  run_cancelling: RunCancelling;
  run_cancelled: RunCancelled;
  cost_updated: CostUpdated;
  cost_unknown_model: CostUnknownModel;
  budget_exceeded: BudgetExceeded;
  run_budget_updated: RunBudgetUpdated;
  model_downgraded: ModelDowngraded;
  blackboard_updated: BlackboardUpdated;
  context_assembled: ContextAssembled;
  context_revision: ContextRevision;
  graph_expanded: GraphExpanded;
  loop_exhausted: LoopExhausted;
  loop_no_progress: LoopNoProgress;
  guard_tripped: GuardTripped;
  approval_requested: ApprovalRequested;
  approval_cancelled: ApprovalCancelled;
  approval_decided: ApprovalDecided;
  approval_expired: ApprovalExpired;
  approval_notified: ApprovalNotified;
  approval_notification_failed: ApprovalNotificationFailed;
}

/** Envelope fields shared by every event, minus the discriminated pair. */
export interface EventEnvelopeBase {
  schema_version: number;
  run_id: UUID;
  seq: number;
  ts: string;
  step_id?: string;
}

type EnvelopeFor<T extends EventType> = EventEnvelopeBase & {
  type: T;
  payload: EventPayloadMap[T];
};

/**
 * One normalized event envelope. A union over every event type, discriminated
 * by `type` — `switch (env.type)` narrows `env.payload` to the matching struct.
 */
export type EventEnvelope = { [K in EventType]: EnvelopeFor<K> }[EventType];
