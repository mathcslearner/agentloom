// The canvas ⇄ Flow adapter (ticket 17.3). graphdef's FlowNode/FlowEdge are
// structurally React-Flow-compatible (ADR-019), so the canvas holds React
// Flow's own Node/Edge with the step/edge object carried in `data`. React Flow
// v12 constrains node/edge `data` to `Record<string, unknown>`, so the typed
// FlowNodeData/FlowEdgeData ride inside that record and are read back through
// the typed accessors below. This module lives beside the store (not in
// pure/) because it is canvas-coupled — it derives the render type and the
// port handles React Flow needs.

import type { Edge as RFEdge, Node as RFNode } from "@xyflow/react";
import type { Edge, Flow, FlowEdge, FlowEdgeData, FlowNode, FlowNodeData, StepType } from "@agentloom/graphdef";
import { deriveHandles } from "@/lib/pure/builder/ports";

/** The edge-renderer key: which registered edge component draws this edge. */
export type EdgeRenderType = "loop" | "step";

/** A canvas node — React Flow's Node with the step object inside `data`. */
export type CanvasNode = RFNode;
/** A canvas edge — React Flow's Edge with the edge object inside `data`. */
export type CanvasEdge = RFEdge;

/** The typed per-node canvas data carried inside a canvas node's `data`. */
export function nodeData(n: CanvasNode): FlowNodeData {
  return n.data as unknown as FlowNodeData;
}
/** The step object carried by a canvas node. */
export function nodeStep(n: CanvasNode) {
  return nodeData(n).step;
}
/** The typed per-edge canvas data carried inside a canvas edge's `data`. */
export function edgeData(e: CanvasEdge): FlowEdgeData {
  return e.data as unknown as FlowEdgeData;
}

/** Which edge component renders an edge, from its definition fields. */
export function edgeRenderType(edge: Edge): EdgeRenderType {
  return edge.type === "loop" ? "loop" : "step";
}

/** Seed canvas nodes from a Flow (React Flow adds its own runtime fields). */
export function seedNodes(flow: Flow): CanvasNode[] {
  return flow.nodes.map((n) => ({
    id: n.id,
    type: n.type,
    position: { x: n.position.x, y: n.position.y },
    data: n.data as unknown as Record<string, unknown>,
  }));
}

/** Seed canvas edges from a Flow, deriving each edge's handles + render type. */
export function seedEdges(flow: Flow): CanvasEdge[] {
  return flow.edges.map((e) => {
    const h = deriveHandles(e.data.edge);
    return {
      id: e.id,
      source: e.source,
      target: e.target,
      sourceHandle: h.sourceHandle,
      targetHandle: h.targetHandle,
      type: edgeRenderType(e.data.edge),
      data: e.data as unknown as Record<string, unknown>,
    };
  });
}

// Reduce a canvas node/edge to the exact graphdef Flow shape — dropping every
// React-Flow runtime field so the round-trip through toDefinition is clean.
function stripNode(n: CanvasNode): FlowNode {
  return {
    id: n.id,
    type: (n.type ?? "noop") as StepType,
    position: { x: n.position.x, y: n.position.y },
    data: nodeData(n),
  };
}

function stripEdge(e: CanvasEdge): FlowEdge {
  return {
    id: e.id,
    source: e.source,
    target: e.target,
    sourceHandle: e.sourceHandle ?? null,
    targetHandle: e.targetHandle ?? null,
    data: edgeData(e),
  };
}

/** Rebuild a graphdef Flow from canvas state (the inverse of seed*). */
export function canvasToFlow(args: {
  doc: Record<string, unknown>;
  ui: Record<string, unknown>;
  uiPresent: boolean;
  nodes: CanvasNode[];
  edges: CanvasEdge[];
}): Flow {
  return {
    doc: args.doc,
    ui: args.ui,
    uiPresent: args.uiPresent,
    nodes: args.nodes.map(stripNode),
    edges: args.edges.map(stripEdge),
  };
}
