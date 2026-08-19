"use client";

// RunStepNode (ticket 18.2) — the React Flow wrapper that paints a run step on
// the live DAG. It reuses the builder's status-neutral StepNodeView and layers
// a run-status skin (tone tint + status badge + run-oriented summary + a
// provenance footer for injected nodes) over it. Non-connectable — the
// dashboard shows a graph, it does not author one.

// React Flow v12 constrains node `data` to `Record<string, unknown>`, so — as
// the 17.3 canvas does — the typed RunStepData rides inside `data` and is read
// back with a cast (the projection is the single typed producer).
import { Handle, Position, type NodeProps } from "@xyflow/react";
import type { Step } from "@agentloom/graphdef";
import { StepNodeView, type NodeSkin } from "@/components/builder/StepNodeView";
import { Badge } from "@/components/ui/badge";
import { stepStatusVariant } from "@/lib/status";
import { cn } from "@/lib/utils";
import type { RunStepData } from "@/lib/pure/dashboard/projection";
import type { SkinTone } from "@/lib/pure/dashboard/skins";
import { sourceHandlesFor, TARGET_HANDLE, type SourceHandle } from "@/lib/pure/builder/ports";

const RIGHT_HANDLES: SourceHandle[] = ["out", "approve", "reject"];

// Tone → shell classes. Kept light so the neutral node reads through; the badge
// carries the strong status color.
const TONE_SHELL: Record<SkinTone, string> = {
  neutral: "",
  ready: "border-sky-400/60",
  active: "border-blue-500 ring-1 ring-blue-500/40 shadow-blue-500/10",
  success: "border-emerald-500/60",
  danger: "border-red-500/70 ring-1 ring-red-500/30",
  warn: "border-amber-500/70",
  attention: "border-violet-500/70 ring-1 ring-violet-500/30",
  muted: "opacity-70",
};

export function RunStepNode({ id, data: raw, selected }: NodeProps) {
  const { stepType, skin, origin, state, entered, pendingApprovalId, onDecide } =
    raw as unknown as RunStepData & {
      onDecide?: (approvalId: string, stepId: string) => void;
    };
  const step = { id, type: stepType, config: {} } as unknown as Step;
  const sources = sourceHandlesFor(stepType as Step["type"]);
  const right = RIGHT_HANDLES.filter((h) => sources.includes(h));

  // A gate awaiting a decision (18.5): the "waiting on you" affordance links
  // into the decision dialog. Its click must not bubble to node selection.
  const decideAffordance =
    pendingApprovalId && onDecide ? (
      <button
        type="button"
        data-testid="node-decide"
        className="rounded bg-violet-600 px-1.5 py-0.5 text-[10px] font-medium text-white hover:bg-violet-500"
        onClick={(e) => {
          e.stopPropagation();
          onDecide(pendingApprovalId, id);
        }}
      >
        Decide →
      </button>
    ) : null;

  const provenance =
    origin.kind !== "definition" ? (
      <span
        className="text-[10px] text-muted-foreground"
        title={origin.step ? `injected by ${origin.step}` : undefined}
        data-testid="node-provenance"
      >
        ⑂ {origin.kind}
        {origin.step ? ` · ${origin.step}` : ""}
      </span>
    ) : null;

  const nodeSkin: NodeSkin = {
    className: cn(TONE_SHELL[skin.tone], skin.pulse && "animate-[dag-pulse_1.6s_ease-in-out_infinite]"),
    badge: (
      <Badge variant={stepStatusVariant(state?.status ?? "pending")} className="text-[9px]">
        {skin.badge}
      </Badge>
    ),
    summary: (
      <span className="flex items-center gap-1.5">
        {skin.summary}
        {skin.marker ? <span className="text-[10px] text-amber-600 dark:text-amber-400">{skin.marker}</span> : null}
      </span>
    ),
    footer:
      decideAffordance || provenance ? (
        <span className="flex items-center justify-between gap-1.5">
          {provenance ?? <span />}
          {decideAffordance}
        </span>
      ) : undefined,
  };

  return (
    <div
      className={cn("relative", entered && "animate-[dag-enter_400ms_ease-out]")}
      data-testid="run-node"
      data-step-id={id}
      data-step-status={state?.status ?? "pending"}
      data-attempt={state?.attempt ?? 0}
      data-reclaims={state?.reclaims ?? 0}
      data-origin-kind={origin.kind}
      data-entered={entered ? "true" : undefined}
    >
      <Handle
        type="target"
        position={Position.Left}
        id={TARGET_HANDLE}
        isConnectable={false}
        className="!h-2.5 !w-2.5 !border-2 !border-background !bg-muted-foreground"
      />
      <StepNodeView step={step} selected={selected} skin={nodeSkin} />
      {right.map((h, i) => {
        const top = ((i + 1) / (right.length + 1)) * 100;
        return (
          <Handle
            key={h}
            type="source"
            position={Position.Right}
            id={h}
            isConnectable={false}
            style={{ top: `${top}%` }}
            className="!h-2.5 !w-2.5 !border-2 !border-background !bg-primary"
          />
        );
      })}
      {sources.includes("loop") ? (
        <Handle
          type="source"
          position={Position.Bottom}
          id="loop"
          isConnectable={false}
          className="!h-2.5 !w-2.5 !border-2 !border-background !bg-amber-500"
        />
      ) : null}
    </div>
  );
}
