"use client";

// Live-DAG edges (ticket 18.2). Two renderers registered as `runStep` and
// `runLoop`. Stroke reflects the edge's live resolution — a fired edge is solid
// foreground, a skipped edge muted/dashed, an unresolved edge faint — and an
// active hand-off (source just completed, target running/ready) animates. Loop
// edges are amber and dashed. The `when`/`decision` guard is labelled.

import { BaseEdge, EdgeLabelRenderer, getSmoothStepPath, getBezierPath, type EdgeProps } from "@xyflow/react";
import type { RunEdgeData } from "@/lib/pure/dashboard/projection";

const FIRED = "var(--color-foreground, #111)";
const MUTED = "var(--color-muted-foreground, #888)";
const LOOP = "var(--color-amber-500, #f59e0b)";

function strokeFor(data: RunEdgeData | undefined, loop: boolean): { stroke: string; opacity: number; dash?: string } {
  const res = data?.visual.resolution ?? "unresolved";
  if (loop) return { stroke: LOOP, opacity: res === "skipped" ? 0.4 : 1, dash: "6 4" };
  if (res === "fired") return { stroke: FIRED, opacity: 0.85 };
  if (res === "skipped") return { stroke: MUTED, opacity: 0.5, dash: "4 4" };
  return { stroke: MUTED, opacity: 0.5 };
}

function labelOf(data: RunEdgeData | undefined): string {
  const e = data?.edge;
  if (!e) return "";
  if (e.decision) return e.decision;
  if (e.when) return "when";
  return "";
}

function EdgeBody({
  id,
  data,
  path,
  labelX,
  labelY,
  markerEnd,
  loop,
}: {
  id: string;
  data: RunEdgeData | undefined;
  path: string;
  labelX: number;
  labelY: number;
  markerEnd?: string;
  loop: boolean;
}) {
  const s = strokeFor(data, loop);
  const active = data?.visual.active ?? false;
  const label = labelOf(data);
  // An active hand-off marches: give it a dash if it hasn't one already.
  const dash = active ? (s.dash ?? "6 6") : s.dash;
  return (
    <>
      <BaseEdge
        id={id}
        path={path}
        markerEnd={markerEnd}
        style={{ stroke: s.stroke, strokeOpacity: s.opacity, strokeWidth: active ? 2.5 : 1.5, strokeDasharray: dash }}
        className={active ? "animate-[dag-dash_0.6s_linear_infinite]" : undefined}
      />
      <EdgeLabelRenderer>
        <span
          data-testid="run-edge"
          data-edge-id={id}
          data-resolution={data?.visual.resolution ?? "unresolved"}
          data-active={active ? "true" : "false"}
          className="hidden"
        />
      </EdgeLabelRenderer>
      {label ? (
        <EdgeLabelRenderer>
          <div
            style={{ transform: `translate(-50%, -50%) translate(${labelX}px, ${labelY}px)` }}
            className="pointer-events-none absolute rounded bg-card px-1.5 py-0.5 text-[10px] font-medium text-muted-foreground shadow-sm"
          >
            {label}
          </div>
        </EdgeLabelRenderer>
      ) : null}
    </>
  );
}

export function RunStepEdge(p: EdgeProps) {
  const data = p.data as unknown as RunEdgeData | undefined;
  const [path, labelX, labelY] = getSmoothStepPath({
    sourceX: p.sourceX,
    sourceY: p.sourceY,
    targetX: p.targetX,
    targetY: p.targetY,
    sourcePosition: p.sourcePosition,
    targetPosition: p.targetPosition,
  });
  return <EdgeBody id={p.id} data={data} path={path} labelX={labelX} labelY={labelY} markerEnd={p.markerEnd} loop={false} />;
}

export function RunLoopEdge(p: EdgeProps) {
  const data = p.data as unknown as RunEdgeData | undefined;
  const [path, labelX, labelY] = getBezierPath({
    sourceX: p.sourceX,
    sourceY: p.sourceY,
    targetX: p.targetX,
    targetY: p.targetY,
    sourcePosition: p.sourcePosition,
    targetPosition: p.targetPosition,
  });
  return <EdgeBody id={p.id} data={data} path={path} labelX={labelX} labelY={labelY} markerEnd={p.markerEnd} loop={true} />;
}
