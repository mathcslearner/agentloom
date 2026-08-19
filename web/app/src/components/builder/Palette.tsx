"use client";

// Palette (ticket 17.3) — the node menu. Every catalog step type is an item in
// two groups: the nine authoring types and a collapsible "Test steps" group.
// Each item supports two add paths: drag-to-create (HTML5 DnD; the Canvas reads
// the type off the dataTransfer and drops it at the cursor) and click / Enter
// (adds at the default cascade position — the reliable keyboard + e2e path).

import { useState } from "react";
import type { StepType } from "@agentloom/graphdef";
import { CORE_TYPES, TEST_TYPES, stepMeta } from "@/lib/pure/builder/catalog";
import { useBuilderStore } from "@/lib/builder/store";
import { cn } from "@/lib/utils";

/** The dataTransfer MIME the Canvas reads a dropped step type from. */
export const STEP_DND_MIME = "application/agentloom-step-type";

function PaletteItem({ type }: { type: StepType }) {
  const addStep = useBuilderStore((s) => s.addStep);
  const meta = stepMeta(type);
  return (
    <button
      type="button"
      draggable
      aria-label={`Add ${meta.label} step`}
      data-step-type={type}
      onDragStart={(e) => {
        e.dataTransfer.setData(STEP_DND_MIME, type);
        e.dataTransfer.effectAllowed = "copy";
      }}
      onClick={() => addStep(type)}
      className="flex w-full items-center justify-between rounded-md border border-border bg-card px-2.5 py-1.5 text-left text-sm hover:border-primary hover:bg-muted focus:outline-none focus-visible:ring-2 focus-visible:ring-ring"
    >
      <span className="font-medium">{meta.label}</span>
      <span className="text-[10px] uppercase tracking-wide text-muted-foreground">{type}</span>
    </button>
  );
}

export function Palette({ className }: { className?: string }) {
  const [showTest, setShowTest] = useState(false);
  return (
    <div className={cn("flex flex-col gap-3 overflow-y-auto p-3", className)} aria-label="Node palette">
      <div className="space-y-1.5">
        <h2 className="px-0.5 text-xs font-semibold uppercase tracking-wide text-muted-foreground">Steps</h2>
        {CORE_TYPES.map((t) => (
          <PaletteItem key={t} type={t} />
        ))}
      </div>
      <div className="space-y-1.5">
        <button
          type="button"
          aria-expanded={showTest}
          onClick={() => setShowTest((v) => !v)}
          className="flex w-full items-center gap-1 px-0.5 text-xs font-semibold uppercase tracking-wide text-muted-foreground hover:text-foreground"
        >
          <span>{showTest ? "▾" : "▸"}</span>
          <span>Test steps</span>
        </button>
        {showTest ? TEST_TYPES.map((t) => <PaletteItem key={t} type={t} />) : null}
      </div>
    </div>
  );
}
