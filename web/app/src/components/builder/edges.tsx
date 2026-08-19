"use client";

// Edge components (ticket 17.3, extended 17.5). Two renderers registered as
// `step` and `loop`: a normal edge as a smooth-step path, a loop back-edge
// dashed and amber. Each shows a routing label (a normal edge's `when`/`decision`,
// a loop edge's `condition`) and now reflects validation state: an edge with
// error-severity problems (a bad CEL predicate, a loop field error) strokes
// destructive, and a focused/hovered problem highlights the edge (DoD-2's path
// highlight). Editing the fields is the EdgePanel / config panels.

import { BaseEdge, EdgeLabelRenderer, getSmoothStepPath, type EdgeProps } from "@xyflow/react";
import type { FlowEdgeData } from "@agentloom/graphdef";
import type { CanvasEdge } from "@/lib/builder/adapter";
import { useEdgeHighlighted, useEdgeProblemCount } from "@/lib/builder/problems-context";

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

const DESTRUCTIVE = "var(--color-destructive, #ef4444)";
const HIGHLIGHT = "var(--color-amber-500, #f59e0b)";

// A tiny transparent overlay carrying the edge's validation state as data-*
// attributes, so tests and the DOM can read invalid/highlight without a
// separate SVG query. React Flow renders the BaseEdge <path> itself.
function EdgeState({ id, invalid, highlighted, kind }: { id: string; invalid: boolean; highlighted: boolean; kind: "step" | "loop" }) {
  return (
    <EdgeLabelRenderer>
      <span
        data-testid="step-edge"
        data-edge-id={id}
        data-edge-kind={kind}
        data-invalid={invalid ? "true" : "false"}
        data-highlight={highlighted ? "true" : undefined}
        className="hidden"
      />
    </EdgeLabelRenderer>
  );
}

export function StepEdge({ id, sourceX, sourceY, targetX, targetY, sourcePosition, targetPosition, markerEnd, data, selected }: EdgeProps<CanvasEdge>) {
  const [path, labelX, labelY] = getSmoothStepPath({ sourceX, sourceY, targetX, targetY, sourcePosition, targetPosition });
  const edge = edgeOf(data);
  const label = edge?.decision ? edge.decision : edge?.when ? "when" : "";
  const invalid = useEdgeProblemCount(id) > 0;
  const highlighted = useEdgeHighlighted(id);
  const stroke = highlighted ? HIGHLIGHT : invalid ? DESTRUCTIVE : undefined;
  return (
    <>
      <BaseEdge id={id} path={path} markerEnd={markerEnd} style={stroke ? { stroke, strokeWidth: selected || highlighted ? 2.5 : 2 } : undefined} />
      <EdgeState id={id} invalid={invalid} highlighted={highlighted} kind="step" />
      {label ? <EdgeLabel label={label} x={labelX} y={labelY} /> : null}
    </>
  );
}

export function LoopEdge({ id, sourceX, sourceY, targetX, targetY, sourcePosition, targetPosition, markerEnd, selected }: EdgeProps<CanvasEdge>) {
  const [path, labelX, labelY] = getSmoothStepPath({ sourceX, sourceY, targetX, targetY, sourcePosition, targetPosition });
  const invalid = useEdgeProblemCount(id) > 0;
  const highlighted = useEdgeHighlighted(id);
  const stroke = highlighted ? HIGHLIGHT : invalid ? DESTRUCTIVE : "var(--color-amber-500, #f59e0b)";
  return (
    <>
      <BaseEdge id={id} path={path} markerEnd={markerEnd} style={{ strokeDasharray: "6 4", stroke, strokeWidth: selected || highlighted ? 2.5 : 1.5 }} />
      <EdgeState id={id} invalid={invalid} highlighted={highlighted} kind="loop" />
      <EdgeLabel label="loop" x={labelX} y={labelY} />
    </>
  );
}
