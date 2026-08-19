// The connection-handle contract (ticket 17.3) — "typed connection handles,
// edge rules by port". Handles are presentation state the canvas owns (ADR-019:
// graphdef never persists sourceHandle/targetHandle). This module is the single
// source of truth for which handles a node exposes, how an existing edge maps
// onto a source handle (render), and what edge fields a new connection from a
// handle seeds (author). Pure; no React, no @xyflow import.

import type { Edge, StepType } from "@agentloom/graphdef";

/** The sole target handle every node exposes. */
export const TARGET_HANDLE = "in" as const;

/** The source handles a node may expose. */
export type SourceHandle = "out" | "approve" | "reject" | "loop";

/**
 * The source handles a node of the given type exposes.
 *
 * Every node has a normal `out` handle and a `loop` handle (any step may be a
 * loop source — a completing step whose authored node has an outgoing marked
 * loop edge, 14.3). A `human_approval` step additionally exposes `approve` and
 * `reject` handles for its decision-routed edges (ADR-017): its unmarked `out`
 * edge is the default/approve path, `approve` marks the approve-only edge, and
 * `reject` routes rejects.
 */
export function sourceHandlesFor(type: StepType): SourceHandle[] {
  if (type === "human_approval") return ["out", "approve", "reject", "loop"];
  return ["out", "loop"];
}

/**
 * The source handle an existing edge renders from (the inverse of
 * {@link edgeSeedForHandle}). A marked loop edge attaches to `loop`; a decision
 * marker to its handle; everything else to `out`. The target is always `in`.
 */
export function deriveHandles(edge: Edge): { sourceHandle: SourceHandle; targetHandle: typeof TARGET_HANDLE } {
  let sourceHandle: SourceHandle = "out";
  if (edge.type === "loop") sourceHandle = "loop";
  else if (edge.decision === "approve") sourceHandle = "approve";
  else if (edge.decision === "reject") sourceHandle = "reject";
  return { sourceHandle, targetHandle: TARGET_HANDLE };
}

/**
 * The edge fields a new connection from a source handle seeds. A `loop` handle
 * seeds a marked loop edge with placeholder `condition`/`max_iterations` the
 * loop-edge inspector (17.6) fills in; the decision handles seed a decision
 * marker; `out` seeds a plain normal edge. Never sets `from`/`to` — the caller
 * supplies those.
 */
export function edgeSeedForHandle(sourceHandle: SourceHandle | null | undefined): Partial<Edge> {
  switch (sourceHandle) {
    case "loop":
      return { type: "loop", condition: "", max_iterations: 3 };
    case "approve":
      return { decision: "approve" };
    case "reject":
      return { decision: "reject" };
    default:
      return {};
  }
}

/** A React-Flow `Connection`-shaped value (structural; no @xyflow dependency). */
export interface ConnectionLike {
  source?: string | null;
  target?: string | null;
  sourceHandle?: string | null;
  targetHandle?: string | null;
}

/** An existing edge for the duplicate check (structural subset of a canvas edge). */
export interface EdgeLike {
  source: string;
  target: string;
  sourceHandle?: string | null;
}

/**
 * Whether a proposed connection is a legal canvas edge. Enforces only the port
 * rules — a source and target, the target attaches to the `in` handle, no
 * self-connection, and no exact duplicate `(source, sourceHandle, target)`.
 * Graph-level rules (cycles except marked loop edges, dangling endpoints,
 * decision-vs-`on_reject` consistency) are the client validator's job (17.5);
 * this only guards what the canvas can express.
 */
export function isValidConnection(conn: ConnectionLike, edges: readonly EdgeLike[]): boolean {
  const { source, target } = conn;
  if (!source || !target) return false;
  if (source === target) return false;
  // Target must attach to the single `in` handle (null tolerated — RF omits the
  // handle id when a node has exactly one target handle).
  if (conn.targetHandle != null && conn.targetHandle !== TARGET_HANDLE) return false;
  const sh = conn.sourceHandle ?? "out";
  for (const e of edges) {
    if (e.source === source && e.target === target && (e.sourceHandle ?? "out") === sh) {
      return false;
    }
  }
  return true;
}
