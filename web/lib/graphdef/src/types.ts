// The canvas state model (ADR-019). These are the shapes the mapping produces
// and consumes; the builder store (17.3) adapts them to React Flow's runtime
// Node/Edge (structurally compatible — id/type/position/data and source/target/
// handles — with no dependency on @xyflow/*).

import type { Definition, Edge, Step, StepType } from "./generated/definition.js";

/** A position on the canvas, in React Flow coordinates. */
export interface Position {
  x: number;
  y: number;
}

/** Per-node canvas data. `step` is the full step object, carried verbatim. */
export interface FlowNodeData {
  /**
   * The entire step object — config, every envelope block, and any unknown
   * key — carried verbatim so nothing the builder did not touch is lost. Its
   * `id`/`type` mirror the enclosing node's; `toDefinition` re-imposes them.
   */
  step: Step;
  /**
   * True when the position was lifted from `ui.nodes[id].position`; false when
   * it was synthesized (the source carried no layout for this step). Only
   * positioned nodes write a position back into the `ui` block.
   */
  positioned: boolean;
  /**
   * The rest of `ui.nodes[id]` (everything other than `position`), preserved so
   * a builder-written per-node hint the engine ignores round-trips. `undefined`
   * when the source had no `ui.nodes` entry for this step; an object (possibly
   * empty) when it did.
   */
  ui?: Record<string, unknown>;
}

/**
 * A canvas node — one per definition step, in `steps` order. Structurally
 * compatible with React Flow's `Node`.
 */
export interface FlowNode {
  id: string;
  type: StepType;
  position: Position;
  data: FlowNodeData;
}

/** Per-edge canvas data. `edge` is the full edge object, carried verbatim. */
export interface FlowEdgeData {
  edge: Edge;
}

/**
 * A canvas edge — one per definition edge, in `edges` order. `id` is
 * synthesized deterministically from `(from, to, duplicate-ordinal)` (the
 * definition has no edge ids). `sourceHandle`/`targetHandle` are presentation
 * state the canvas (17.3) owns; the mapping never reads or persists them.
 * Structurally compatible with React Flow's `Edge`.
 */
export interface FlowEdge {
  id: string;
  source: string;
  target: string;
  sourceHandle?: string | null;
  targetHandle?: string | null;
  data: FlowEdgeData;
}

/**
 * The definition document minus `steps`, `edges`, and `ui` — schema_version,
 * name, the run-level policies, the `templates`/`agents` libraries, `params`,
 * and any unknown top-level key — copied whole and edited through document-level
 * panels. Kept as an opaque record so unknown keys survive.
 */
export type DocumentMeta = Record<string, unknown>;

/**
 * The complete canvas state: the document meta, the nodes, the edges, and the
 * residual `ui` block. `toFlow` produces it; `toDefinition` consumes it back
 * into a definition value losslessly.
 */
export interface Flow {
  doc: DocumentMeta;
  nodes: FlowNode[];
  edges: FlowEdge[];
  /**
   * The `ui` block with each existing step's per-node entry lifted out: the
   * viewport, any `ui.version`, unknown keys, and orphan `ui.nodes` entries
   * (ids that name no step) — all preserved verbatim. The `nodes` key is
   * retained when the source had one (even if now empty), so it re-emits.
   */
  ui: Record<string, unknown>;
  /**
   * Whether the source document carried a `ui` key at all, so `toDefinition`
   * does not synthesize a `ui: {}` on a definition that had none (nor drop a
   * deliberately-empty one).
   */
  uiPresent: boolean;
}

/** Options for {@link toFlow}. */
export interface ToFlowOptions {
  /**
   * Positions steps that carry no `ui.nodes[id].position`. Defaults to a
   * deterministic grid. Called with the step and its index in `steps` order.
   */
  defaultPosition?: (step: Step, index: number) => Position;
}

/**
 * A shape or invariant violation the canvas cannot represent (a non-object
 * document, a step with no string id/type, a duplicate step id, a non-object
 * `ui`). Carries the backend-style `path` grammar (`steps[3].id`). Content
 * validation (configs, edge endpoints, cycles, CEL) is not done here — that is
 * the client validator (17.5); a `Flow` may hold an invalid-but-well-shaped
 * definition.
 */
export class GraphdefError extends Error {
  readonly code: string;
  readonly path: string;
  constructor(code: string, path: string, message: string) {
    super(message);
    this.name = "GraphdefError";
    this.code = code;
    this.path = path;
  }
}

export type { Definition };
