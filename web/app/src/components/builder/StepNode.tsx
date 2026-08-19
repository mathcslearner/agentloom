"use client";

// StepNode — the React Flow wrapper around StepNodeView (ticket 17.3). It draws
// the presentational body and the typed connection handles: one `in` target on
// the left, and source handles on the right (`out`, plus `approve`/`reject` on a
// human_approval step) with a `loop` source on the bottom. The handle ids are
// the port contract in src/lib/pure/builder/ports.ts; the same one component is
// registered for every step type (nodeTypes maps each StepType → StepNode).

import { Handle, Position, type NodeProps } from "@xyflow/react";
import type { Step } from "@agentloom/graphdef";
import type { CanvasNode } from "@/lib/builder/adapter";
import { sourceHandlesFor, TARGET_HANDLE, type SourceHandle } from "@/lib/pure/builder/ports";
import { useNodeHighlighted, useNodeProblemCount } from "@/lib/builder/problems-context";
import { StepNodeView } from "./StepNodeView";

const RIGHT_HANDLES: SourceHandle[] = ["out", "approve", "reject"];

const HANDLE_LABEL: Record<SourceHandle, string> = {
  out: "next",
  approve: "on approve",
  reject: "on reject",
  loop: "loop back",
};

export function StepNode({ id, data, selected }: NodeProps<CanvasNode>) {
  const step = (data as unknown as { step: Step }).step;
  const sources = sourceHandlesFor(step.type);
  const right = RIGHT_HANDLES.filter((h) => sources.includes(h));
  const problemCount = useNodeProblemCount(id);
  const highlighted = useNodeHighlighted(id);

  return (
    <div className="relative">
      <Handle
        type="target"
        position={Position.Left}
        id={TARGET_HANDLE}
        aria-label={`${step.id} input`}
        className="!h-3 !w-3 !border-2 !border-background !bg-muted-foreground"
      />
      <StepNodeView step={step} selected={selected} problemCount={problemCount} highlighted={highlighted} />
      {right.map((h, i) => {
        const top = ((i + 1) / (right.length + 1)) * 100;
        return (
          <Handle
            key={h}
            type="source"
            position={Position.Right}
            id={h}
            style={{ top: `${top}%` }}
            aria-label={`${step.id} ${HANDLE_LABEL[h]}`}
            data-handle-id={h}
            className="!h-3 !w-3 !border-2 !border-background !bg-primary"
          />
        );
      })}
      {sources.includes("loop") ? (
        <Handle
          type="source"
          position={Position.Bottom}
          id="loop"
          aria-label={`${step.id} ${HANDLE_LABEL.loop}`}
          data-handle-id="loop"
          className="!h-3 !w-3 !border-2 !border-background !bg-amber-500"
        />
      ) : null}
    </div>
  );
}
