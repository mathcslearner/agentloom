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
