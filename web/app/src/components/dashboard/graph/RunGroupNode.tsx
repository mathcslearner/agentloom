"use client";

// RunGroupNode (ticket 18.2) — the container behind a loop's iteration or a
// map's instances. Expanded, it is a labelled boundary box (rendered behind the
// member step nodes). Collapsed, it is a compact pill summarising the group
// with a rolled-up status. The collapse toggle re-projects the graph.

import { type NodeProps } from "@xyflow/react";
import { Badge } from "@/components/ui/badge";
import { stepStatusVariant } from "@/lib/status";
import { cn } from "@/lib/utils";
import type { DisplayStepStatus } from "@/lib/pure/dashboard/run-state";
import type { RunGroupData } from "@/lib/pure/dashboard/projection";

type RunGroupCarried = RunGroupData & { onToggle?: (id: string) => void };

export function RunGroupNode({ id, data: raw, width, height }: NodeProps) {
  const { group, collapsed, memberCount, status, onToggle } = raw as unknown as RunGroupCarried;
  const kindLabel = group.kind === "loop" ? "loop" : "map";

  if (collapsed) {
    return (
      <button
        type="button"
        onClick={() => onToggle?.(id)}
        data-testid="run-group"
        data-group-id={id}
        data-collapsed="true"
        style={{ width, height }}
        className="flex flex-col items-start justify-center gap-1 rounded-lg border-2 border-dashed border-amber-500/60 bg-amber-500/5 px-3 text-left hover:bg-amber-500/10"
      >
        <span className="flex items-center gap-1.5 text-xs font-medium">
          <span className="text-amber-600 dark:text-amber-400">⟳ {kindLabel}</span>
          <Badge variant={stepStatusVariant(status as DisplayStepStatus)} className="text-[9px]">
            {status}
          </Badge>
        </span>
        <span className="text-[10px] text-muted-foreground">
          {group.label} · {memberCount} step{memberCount === 1 ? "" : "s"} · click to expand
        </span>
      </button>
    );
  }

  return (
    <div
      data-testid="run-group"
      data-group-id={id}
      data-collapsed="false"
      style={{ width, height }}
      className={cn("rounded-lg border border-dashed border-amber-500/40 bg-amber-500/[0.03]")}
    >
      <button
        type="button"
        onClick={() => onToggle?.(id)}
        className="flex w-full items-center gap-1.5 border-b border-dashed border-amber-500/30 px-2 py-1 text-left text-[10px] font-medium text-amber-700 hover:bg-amber-500/10 dark:text-amber-300"
      >
        <span>⟳ {kindLabel}</span>
        <span className="text-muted-foreground">{group.label}</span>
        <span className="ml-auto text-muted-foreground">collapse</span>
      </button>
    </div>
  );
}
