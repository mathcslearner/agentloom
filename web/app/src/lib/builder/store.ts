"use client";

// The builder store (ticket 17.3) — zustand holding the canvas state, wrapped in
// zundo's `temporal` middleware for undo/redo. The tracked slice is the
// serializable canvas (doc/ui/nodes/edges); React-Flow runtime churn (selection,
// measurement, viewport) is deliberately kept out of history via `partialize`
// + a structural `equality`, so a select or a re-measure pushes no undo entry.
// A drag is coalesced into a single undo entry by pausing history for its
// duration and pushing the pre-drag snapshot once on drag stop.

import { create } from "zustand";
import { temporal } from "zundo";
import { applyEdgeChanges, applyNodeChanges } from "@xyflow/react";
import type { EdgeChange, NodeChange } from "@xyflow/react";
import type { Edge, Flow, Position, StepType } from "@agentloom/graphdef";
import { edgeId, toDefinition } from "@agentloom/graphdef";
import { canvasToFlow, edgeRenderType, seedEdges, seedNodes, type CanvasEdge, type CanvasNode } from "./adapter";
import { deriveHandles, edgeSeedForHandle, isValidConnection, TARGET_HANDLE, type ConnectionLike } from "@/lib/pure/builder/ports";
import { allocateStepId, cascadePlacement, newStepForType } from "@/lib/pure/builder/steps";

const HISTORY_LIMIT = 100;

/** An empty starting document (a fresh, unnamed workflow). */
export function emptyDefinition(): Record<string, unknown> {
  return { schema_version: 1, name: "untitled", steps: [], edges: [] };
}

export interface BuilderState {
  doc: Record<string, unknown>;
  ui: Record<string, unknown>;
  uiPresent: boolean;
  nodes: CanvasNode[];
  edges: CanvasEdge[];

  /** Replace the whole canvas from a Flow and reset undo history. */
  load: (flow: Flow) => void;
  onNodesChange: (changes: NodeChange<CanvasNode>[]) => void;
  onEdgesChange: (changes: EdgeChange<CanvasEdge>[]) => void;
  /** Add a step of a type; returns its allocated id. */
  addStep: (type: StepType, position?: Position) => string;
  /** Create an edge from a proposed connection, honoring the port rules. */
  connect: (conn: ConnectionLike) => void;
  /** Delete the currently selected nodes (and their edges) and selected edges. */
  deleteSelected: () => void;

  /** Current canvas as a graphdef Flow (runtime fields stripped). */
  toFlowState: () => Flow;
  /** Current canvas as a definition JSON value. */
  toDefinitionValue: () => unknown;
}

// The tracked (undoable) slice, in a fixed key order so a structural JSON
// comparison is a stable equality test.
interface Tracked {
  doc: Record<string, unknown>;
  ui: Record<string, unknown>;
  uiPresent: boolean;
  nodes: Array<{ id: string; type: string | undefined; position: Position; data: CanvasNode["data"] }>;
  edges: Array<{
    id: string;
    source: string;
    target: string;
    sourceHandle: string | null;
    targetHandle: string | null;
    type: string | undefined;
    data: CanvasEdge["data"];
  }>;
}

function partialize(s: BuilderState): Tracked {
  return {
    doc: s.doc,
    ui: s.ui,
    uiPresent: s.uiPresent,
    nodes: s.nodes.map((n) => ({ id: n.id, type: n.type, position: { x: n.position.x, y: n.position.y }, data: n.data })),
    edges: s.edges.map((e) => ({
      id: e.id,
      source: e.source,
      target: e.target,
      sourceHandle: e.sourceHandle ?? null,
      targetHandle: e.targetHandle ?? null,
      type: e.type,
      data: e.data,
    })),
  };
}

const stable = (t: Tracked): string => JSON.stringify(t);
const tracksEqual = (a: Tracked, b: Tracked): boolean => stable(a) === stable(b);

export const useBuilderStore = create<BuilderState>()(
  temporal(
    (set, get) => ({
      doc: emptyDefinition(),
      ui: { nodes: {} },
      uiPresent: false,
      nodes: [],
      edges: [],

      load: (flow) => {
        set({
          doc: flow.doc,
          ui: flow.ui,
          uiPresent: flow.uiPresent,
          nodes: seedNodes(flow),
          edges: seedEdges(flow),
        });
        useBuilderStore.temporal.getState().clear();
      },

      onNodesChange: (changes) => set({ nodes: applyNodeChanges(changes, get().nodes) }),
      onEdgesChange: (changes) => set({ edges: applyEdgeChanges(changes, get().edges) }),

      addStep: (type, position) => {
        const { nodes } = get();
        const id = allocateStepId(
          type,
          nodes.map((n) => n.id),
        );
        const pos = position ?? cascadePlacement({ x: 0, y: 0 }, nodes.length);
        const step = newStepForType(type, id);
        const node: CanvasNode = {
          id,
          type,
          position: pos,
          data: { step, positioned: true } as unknown as Record<string, unknown>,
          selected: true,
        };
        set({
          nodes: [...nodes.map((n) => (n.selected ? { ...n, selected: false } : n)), node],
        });
        return id;
      },

      connect: (conn) => {
        const { nodes, edges } = get();
        if (!isValidConnection(conn, edges)) return;
        const from = conn.source as string;
        const to = conn.target as string;
        const seed = edgeSeedForHandle(conn.sourceHandle as never);
        const edgeObj: Edge = { from, to, ...seed } as Edge;

        // Deterministic, collision-free id: the lowest free duplicate ordinal.
        const taken = new Set(edges.map((e) => e.id));
        let n = 1;
        let id = edgeId(from, to, n);
        while (taken.has(id)) {
          n += 1;
          id = edgeId(from, to, n);
        }

        const handles = deriveHandles(edgeObj);
        const edge: CanvasEdge = {
          id,
          source: from,
          target: to,
          sourceHandle: (conn.sourceHandle as string | null | undefined) ?? handles.sourceHandle,
          targetHandle: TARGET_HANDLE,
          type: edgeRenderType(edgeObj),
          data: { edge: edgeObj } as unknown as Record<string, unknown>,
        };
        set({ edges: [...edges, edge] });
        void nodes;
      },

      deleteSelected: () => {
        const { nodes, edges } = get();
        const doomed = new Set(nodes.filter((n) => n.selected).map((n) => n.id));
        if (doomed.size === 0 && !edges.some((e) => e.selected)) return;
        const keptNodes = nodes.filter((n) => !doomed.has(n.id));
        const keptEdges = edges.filter((e) => !e.selected && !doomed.has(e.source) && !doomed.has(e.target));
        set({ nodes: keptNodes, edges: keptEdges });
      },

      toFlowState: () => {
        const s = get();
        return canvasToFlow({ doc: s.doc, ui: s.ui, uiPresent: s.uiPresent, nodes: s.nodes, edges: s.edges });
      },

      toDefinitionValue: () => toDefinition(get().toFlowState()),
    }),
    {
      limit: HISTORY_LIMIT,
      partialize,
      equality: tracksEqual,
    },
  ),
);

// ── Interaction batching ─────────────────────────────────────────────────────
// A node drag emits a stream of position changes. Pausing history for the drag
// keeps those out of the undo stack; on drag stop we push the pre-drag snapshot
// exactly once, so the whole move is a single undo entry.

let dragSnapshot: Tracked | null = null;

/** Begin a batched interaction (a drag): snapshot state and pause history. */
export function beginInteraction(): void {
  dragSnapshot = partialize(useBuilderStore.getState());
  useBuilderStore.temporal.getState().pause();
}

/** End a batched interaction: resume history and record one entry if changed. */
export function endInteraction(): void {
  const temporal = useBuilderStore.temporal;
  temporal.getState().resume();
  const snap = dragSnapshot;
  dragSnapshot = null;
  if (!snap) return;
  const now = partialize(useBuilderStore.getState());
  if (tracksEqual(snap, now)) return;
  // Push the pre-drag snapshot as the single undo entry for the whole move.
  const pastStates = [...temporal.getState().pastStates, snap].slice(-HISTORY_LIMIT);
  temporal.setState({ pastStates, futureStates: [] });
}

/** Undo the last tracked change. */
export function undo(): void {
  useBuilderStore.temporal.getState().undo();
}

/** Redo the last undone change. */
export function redo(): void {
  useBuilderStore.temporal.getState().redo();
}
