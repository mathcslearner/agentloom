"use client";

// Edge components (ticket 17.3). Two renderers registered as `step` and `loop`
// (see adapter.edgeRenderType): a normal edge drawn as a smooth-step path, and a
// loop back-edge drawn dashed and amber. Each shows a small label when the edge
// carries a routing marker — a normal edge's `when` guard or `decision`
// (approve/reject), a loop edge's `condition` — so the routing is legible on the
// canvas. Editing those fields is the loop-edge inspector / config panels (17.6).

import { BaseEdge, EdgeLabelRenderer, getSmoothStepPath, type EdgeProps } from "@xyflow/react";
import type { FlowEdgeData } from "@agentloom/graphdef";
import type { CanvasEdge } from "@/lib/builder/adapter";

function edgeOf(data: unknown): FlowEdgeData["edge"] | undefined {
  return (data as unknown as FlowEdgeData | undefined)?.edge;
}

function EdgeLabel({ label, x, y }: { label: string; x: number; y: number }) {
  return (
    <EdgeLabelRenderer>
      <div
        style={{ transform: `translate(-50%, -50%) translate(${x}px, ${y}px)` }}
        className="pointer-events-none absolute rounded bg-card px-1.5 py-0.5 text-[10px] font-medium text-muted-foreground shadow-sm"
      >
        {label}
      </div>
    </EdgeLabelRenderer>
  );
}

export function StepEdge({ id, sourceX, sourceY, targetX, targetY, sourcePosition, targetPosition, markerEnd, data }: EdgeProps<CanvasEdge>) {
  const [path, labelX, labelY] = getSmoothStepPath({ sourceX, sourceY, targetX, targetY, sourcePosition, targetPosition });
  const edge = edgeOf(data);
  const label = edge?.decision ? edge.decision : edge?.when ? "when" : "";
  return (
    <>
      <BaseEdge id={id} path={path} markerEnd={markerEnd} />
      {label ? <EdgeLabel label={label} x={labelX} y={labelY} /> : null}
    </>
  );
}

export function LoopEdge({ id, sourceX, sourceY, targetX, targetY, sourcePosition, targetPosition, markerEnd }: EdgeProps<CanvasEdge>) {
  const [path, labelX, labelY] = getSmoothStepPath({ sourceX, sourceY, targetX, targetY, sourcePosition, targetPosition });
  return (
    <>
      <BaseEdge id={id} path={path} markerEnd={markerEnd} style={{ strokeDasharray: "6 4", stroke: "var(--color-amber-500, #f59e0b)" }} />
      <EdgeLabel label="loop" x={labelX} y={labelY} />
    </>
  );
}
