// Code generated from docs/schema/workflow-definition.v1.json — DO NOT EDIT.
//
// Regenerate with `pnpm generate` (web/lib/graphdef). The Go dag structs are
// the source of truth (ADR-003): `make generate` emits the JSON Schema, this
// file is emitted from it, and CI fails if it is stale.

export interface AgentConfig {
  agent?: string;
  system?: string;
  model?: string;
  prompt?: string;
  messages?: LLMMessage[];
  max_tokens?: number;
  temperature?: number;
  model_fallbacks?: ModelFallback[];
  output_format?: OutputFormat;
  tools?: string[];
  role?: string;
}

export interface AgentDef {
  role?: string;
  system?: string;
  model?: string;
  model_fallbacks?: ModelFallback[];
  tools?: string[];
  max_tokens?: number;
  temperature?: number;
  output_format?: OutputFormat;
  validation?: ValidationPolicy;
  context?: ContextSpec;
}

export type ApprovalDecision = "approve" | "reject";

export type ApprovalRejectPolicy = "fail" | "route";

export type ApprovalTimeoutPolicy = "reject" | "approve" | "park";

export interface BackoffSpec {
  initial?: string;
  cap?: string;
  multiplier?: number;
}

export interface BlackboardPolicy {
  write?: BlackboardWrite[];
}

export interface BlackboardWrite {
  key: string;
  from?: string;
  tags?: string[];
  pinned?: boolean;
}

export interface BlackboardWriteConfig {
  key?: string;
  value?: unknown;
  tags?: string[];
  expected_version?: number;
  read_key?: string;
}

export interface BranchConfig {
  input?: unknown;
}

export type BudgetPolicy = "park" | "fail";

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

export type ContextMissingPolicy = "error" | "skip";

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

export interface ContextSpec {
  sources?: ContextSource[];
  budget_tokens?: number;
  compaction?: CompactionStrategy[];
}

export interface CounterConfig {
  path?: string;
}

export interface Definition {
  schema_version: number;
  name: string;
  description?: string;
  on_failure?: FailurePolicy;
  max_wall_clock?: string;
  budget_usd?: number;
  on_budget_exceeded?: BudgetPolicy;
  expansion?: ExpansionPolicy;
  templates?: Record<string, Template>;
  agents?: Record<string, AgentDef>;
  params?: Record<string, ParamSpec>;
  steps: Step[];
  edges: Edge[];
  ui?: Record<string, unknown>;
}

export interface EchoConfig {
  input?: unknown;
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

export interface EffectfulEchoConfig {
  path?: string;
  input?: unknown;
  fail_times?: number;
}

export type ErrorClass = "transient" | "permanent" | "timeout" | "cancelled" | "validation_failed";

export type ExhaustPolicy = "proceed" | "fail";

export interface ExpansionPolicy {
  max_added_steps?: number;
  max_total_steps?: number;
  max_expansions?: number;
  max_depth?: number;
}

export interface FailNTimesConfig {
  n?: number;
}

export type FailurePolicy = "fail_fast" | "continue_independent_branches";

export interface FeedbackPolicy {
  template?: string;
  max_output_chars?: number;
}

export interface GatherConfig {
  items?: unknown;
}

export interface HumanApprovalConfig {
  title?: string;
  description?: string;
  payload?: unknown;
  allowed_decisions?: ApprovalDecision[];
  allow_edit?: boolean;
  edit_schema?: unknown;
  timeout?: string;
  on_timeout?: ApprovalTimeoutPolicy;
  on_reject?: ApprovalRejectPolicy;
}

export type ItemFailurePolicy = "fail_fast" | "collect_errors";

export type JitterMode = "full" | "none";

export interface JoinConfig {
  mode?: JoinMode;
}

export type JoinMode = "all" | "any";

export interface LLMConfig {
  model?: string;
  system?: string;
  prompt?: string;
  messages?: LLMMessage[];
  max_tokens?: number;
  temperature?: number;
  model_fallbacks?: ModelFallback[];
  output_format?: OutputFormat;
}

export interface LLMMessage {
  role?: string;
  content?: string;
}

export interface MapConfig {
  items?: unknown;
  body?: string;
  max_items?: number;
  on_item_failure?: ItemFailurePolicy;
}

export interface ModelFallback {
  model?: string;
  at_budget_fraction?: number;
}

export interface NoProgressPolicy {
  step?: string;
  path?: string;
  policy?: ExhaustPolicy;
}

export interface NoopConfig {

}

export interface OutputFormat {
  type?: string;
  schema?: unknown;
  mode?: string;
}

export interface ParamSpec {
  type: ParamType;
  required?: boolean;
}

export type ParamType = "string" | "number" | "boolean" | "object" | "array";

export interface PlannerConfig {
  model?: string;
  prompt?: string;
  messages?: LLMMessage[];
  max_tokens?: number;
  temperature?: number;
  max_added_steps?: number;
}

export interface RetrieveConfig {
  retriever?: string;
  query?: string;
  top_k?: number;
}

export interface RetryPolicy {
  max_attempts?: number;
  backoff?: BackoffSpec;
  jitter?: JitterMode;
  retry_on?: ErrorClass[];
}

export interface SleepConfig {
  duration?: string;
}

export interface StepBudget {
  max_usd?: number;
  max_tokens?: number;
}

export type StepType = "llm" | "tool" | "retrieve" | "map" | "gather" | "planner" | "agent" | "human_approval" | "join" | "branch" | "noop" | "echo" | "sleep" | "fail_n_times" | "counter" | "effectful_echo" | "blackboard_write";

export interface Template {
  steps: Step[];
  edges?: Edge[];
}

export interface ToolConfig {
  tool?: string;
  input?: unknown;
}

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

export interface StepBase {
  id: string;
  retry?: RetryPolicy;
  timeout?: string;
  cache?: CachePolicy;
  budget?: StepBudget;
  validation?: ValidationPolicy;
  blackboard?: BlackboardPolicy;
  context?: ContextSpec;
}

/** Every step type in the catalog (ADR-003), in Step-oneOf order. */
export const STEP_TYPES: readonly StepType[] = [
  "llm",
  "tool",
  "retrieve",
  "map",
  "gather",
  "planner",
  "agent",
  "human_approval",
  "join",
  "branch",
  "noop",
  "echo",
  "sleep",
  "fail_n_times",
  "counter",
  "effectful_echo",
  "blackboard_write",
];

/** Maps each step type to its typed config shape. */
export interface StepConfigMap {
  llm: LLMConfig;
  tool: ToolConfig;
  retrieve: RetrieveConfig;
  map: MapConfig;
  gather: GatherConfig;
  planner: PlannerConfig;
  agent: AgentConfig;
  human_approval: HumanApprovalConfig;
  join: JoinConfig;
  branch: BranchConfig;
  noop: NoopConfig;
  echo: EchoConfig;
  sleep: SleepConfig;
  fail_n_times: FailNTimesConfig;
  counter: CounterConfig;
  effectful_echo: EffectfulEchoConfig;
  blackboard_write: BlackboardWriteConfig;
}

/**
 * One workflow step. A discriminated union over StepType: switching on
 * `step.type` narrows `step.config` to the matching config shape.
 */
export type Step = { [K in StepType]: StepBase & { type: K; config?: StepConfigMap[K] } }[StepType];
