// @agentloom/graphdef — the serialization boundary (ADR-019).
//
// A pure, isolated module mapping the workflow definition JSON to and from the
// builder's canvas state. Zero React/UI imports (enforced by eslint.config.mjs
// and test/boundary.test.ts). The definition types are generated from
// docs/schema/workflow-definition.v1.json (scripts/gen-definition-types.ts,
// CI-diffed).

export * from "./generated/definition.js";
export {
  GraphdefError,
  type DocumentMeta,
  type Flow,
  type FlowEdge,
  type FlowEdgeData,
  type FlowNode,
  type FlowNodeData,
  type Position,
  type ToFlowOptions,
} from "./types.js";
export { edgeId, assignEdgeIds } from "./edge-id.js";
export { toFlow } from "./to-flow.js";
export { toDefinition } from "./to-definition.js";
export { parseDefinition } from "./parse.js";

// Validation (17.4) — the client-side config validator and its supporting
// schema-subset shape checker (ADR-019 §"Validation parity").
export { DEFINITION_SCHEMA } from "./generated/definition.schema.js";
export { type Issue, type Severity, Code, hasErrors, err, warn } from "./validate/issue.js";
export { validateStepConfigs, type ConfigSchemas, type StepConfigSchema } from "./validate/config.js";
export { fallbackConfigSchemas, pluginConfigSchema } from "./validate/schemas.js";
export { checkShape, resolveRef, type JsonSchema, type SchemaNode, type Defs } from "./schema/schema.js";
export { parseGoDuration, MAX_APPROVAL_TIMEOUT_SECONDS } from "./validate/duration.js";
