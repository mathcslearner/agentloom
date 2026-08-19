"use client";

// RunGraph (ticket 18.2) — the live DAG canvas that replaces 18.1's steps pane.
// It reuses the builder's node visuals with run-status skins, lays the graph out
// incrementally (sticky positions honoring `ui` hints), animates planner / map /
// loop injections in, and groups loop iterations / map instances into
// collapsible containers. Read-only: nodes are not connectable or authored here.

import { useCallback, useEffect, useMemo, useRef } from "react";
import {
  Background,
  Controls,
  MiniMap,
  ReactFlow,
  ReactFlowProvider,
  useReactFlow,
  type Edge as RFEdge,
  type EdgeTypes,
  type Node as RFNode,
  type NodeChange,
  type NodePositionChange,
  type NodeTypes,
} from "@xyflow/react";
import type { GraphTopology } from "@/lib/pure/dashboard/graph-topology";
import type { StepState } from "@/lib/pure/dashboard/run-state";
import type { ApprovalMap } from "@/lib/pure/dashboard/approvals";
import type { Layouter } from "@/lib/pure/dashboard/layout";
import type { DagEdge, DagNode } from "@/lib/pure/dashboard/projection";
import { useRunGraph } from "@/lib/dashboard/useRunGraph";
import { RunStepNode } from "./RunStepNode";
import { RunGroupNode } from "./RunGroupNode";
import { RunLoopEdge, RunStepEdge } from "./RunEdge";
import "@xyflow/react/dist/style.css";

const nodeTypes: NodeTypes = { runStep: RunStepNode, runGroup: RunGroupNode };
const edgeTypes: EdgeTypes = { runStep: RunStepEdge, runLoop: RunLoopEdge };

export interface RunGraphProps {
  topology: GraphTopology;
  steps: Map<string, StepState>;
  selected?: string;
  onSelect: (id: string) => void;
  /** Pending/decided approvals (18.5) — drives the node "Decide" affordance. */
  approvals?: ApprovalMap;
  /** Open the decision dialog for a gate's pending approval. */
  onDecide?: (approvalId: string, stepId: string) => void;
  /** Overridable for tests (defaults to the elkjs layouter). */
  layouter?: Layouter;
}

export function RunGraph(props: RunGraphProps) {
  return (
    <ReactFlowProvider>
      <RunGraphInner {...props} />
    </ReactFlowProvider>
  );
}

function RunGraphInner({ topology, steps, selected, onSelect, approvals, onDecide, layouter }: RunGraphProps) {
  const view = useRunGraph(topology, steps, layouter, approvals);
  const { fitView } = useReactFlow();
  const fitted = useRef(false);

  const rfNodes = useMemo<RFNode[]>(
    () => view.nodes.map((n) => toRFNode(n, selected, view.toggleGroup, onDecide)),
    [view.nodes, selected, view.toggleGroup, onDecide],
  );
  const rfEdges = useMemo<RFEdge[]>(() => view.edges.map(toRFEdge), [view.edges]);

  // Fit the view once, after the first layout completes.
  useEffect(() => {
    if (!fitted.current && view.layoutReady && rfNodes.length > 0) {
      fitted.current = true;
      // Defer so React Flow has measured the freshly-placed nodes.
      requestAnimationFrame(() => fitView({ padding: 0.2, duration: 300 }));
    }
  }, [view.layoutReady, rfNodes.length, fitView]);

  const onNodesChange = useCallback(
    (changes: NodeChange[]) => {
      const moves = changes.filter((c): c is NodePositionChange => c.type === "position");
      if (moves.length) view.onNodesMoved(moves);
    },
    [view],
  );

  const onNodeClick = useCallback(
    (_: unknown, node: RFNode) => {
      if (node.type === "runStep") onSelect(node.id);
    },
    [onSelect],
  );

  return (
    <div className="relative h-full w-full" data-testid="run-graph" data-graph-version={topology.graphVersion} data-layout-ready={view.layoutReady ? "true" : "false"}>
      <ReactFlow
        nodes={rfNodes}
        edges={rfEdges}
        nodeTypes={nodeTypes}
        edgeTypes={edgeTypes}
        onNodesChange={onNodesChange}
        onNodeClick={onNodeClick}
        nodesConnectable={false}
        nodesDraggable
        elementsSelectable
        deleteKeyCode={null}
        minZoom={0.1}
        proOptions={{ hideAttribution: true }}
      >
        <Background />
        <MiniMap pannable zoomable className="!bg-card" />
        <Controls showInteractive={false} />
      </ReactFlow>
      <div className="pointer-events-none absolute right-2 top-2 flex flex-col items-end gap-1">
        {view.groups.length > 0 ? (
          <div className="pointer-events-auto flex gap-1 rounded-md border bg-card/90 p-0.5 text-[11px] shadow-sm">
            <button className="rounded px-1.5 py-0.5 hover:bg-accent" onClick={view.collapseAll} data-testid="collapse-groups">
              collapse loops
            </button>
            <button className="rounded px-1.5 py-0.5 hover:bg-accent" onClick={view.expandAll} data-testid="expand-groups">
              expand
            </button>
          </div>
        ) : null}
      </div>
    </div>
  );
}

function toRFNode(
  n: DagNode,
  selected: string | undefined,
  onToggle: (id: string) => void,
  onDecide?: (approvalId: string, stepId: string) => void,
): RFNode {
  const base = {
    id: n.id,
    type: n.kind,
    position: n.position,
    hidden: n.hidden,
    zIndex: n.z,
    selected: n.kind === "runStep" && n.id === selected,
    draggable: n.kind === "runStep",
    selectable: n.kind === "runStep",
    ...(n.width != null ? { width: n.width } : {}),
    ...(n.height != null ? { height: n.height } : {}),
    // React Flow constrains data to Record<string, unknown>; the typed shapes
    // ride inside and are read back with a cast in the node components. Group
    // nodes carry `onToggle`; a gate step carries `onDecide` (18.5).
    data: (n.kind === "runGroup"
      ? { ...n.data, onToggle }
      : { ...n.data, onDecide }) as unknown as Record<string, unknown>,
  };
  return base as unknown as RFNode;
}

function toRFEdge(e: DagEdge): RFEdge {
  return {
    id: e.id,
    source: e.source,
    target: e.target,
    sourceHandle: e.sourceHandle,
    targetHandle: e.targetHandle,
    type: e.kind,
    hidden: e.hidden,
    data: e.data as unknown as Record<string, unknown>,
  } as unknown as RFEdge;
}
