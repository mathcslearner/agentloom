"use client";

// Inspector (ticket 17.3) — a placeholder right pane. The schema-driven config
// panel replaces the "selected step" section in 17.4; the live definition
// preview stays useful for debugging and lets tests assert the serialized
// semantics (e.g. that an edge dragged from the reject port carries
// decision: "reject"). Read-only in 17.3 — no config editing yet.

import type { StepType } from "@agentloom/graphdef";
import { useBuilderStore } from "@/lib/builder/store";
import { stepMeta } from "@/lib/pure/builder/catalog";

export function Inspector({ className }: { className?: string }) {
  // Subscribes to nodes + edges, so the preview recomputes whenever the graph
  // changes (toDefinitionValue reads the current state on each render).
  const nodes = useBuilderStore((s) => s.nodes);
  const edges = useBuilderStore((s) => s.edges);
  const toDefinitionValue = useBuilderStore((s) => s.toDefinitionValue);

  const selected = nodes.filter((n) => n.selected);
  const preview = JSON.stringify(toDefinitionValue(), null, 2);

  return (
    <div className={className} aria-label="Inspector">
      <div className="border-b p-3">
        <h2 className="text-xs font-semibold uppercase tracking-wide text-muted-foreground">Selection</h2>
        {selected.length === 0 ? (
          <p className="mt-1 text-sm text-muted-foreground">No step selected.</p>
        ) : selected.length === 1 ? (
          <div className="mt-1 text-sm">
            <div className="font-medium" data-testid="inspector-step-id">
              {selected[0]!.id}
            </div>
            <div className="text-muted-foreground">{stepMeta((selected[0]!.type ?? "noop") as StepType).label}</div>
          </div>
        ) : (
          <p className="mt-1 text-sm text-muted-foreground">{selected.length} steps selected.</p>
        )}
      </div>
      <div className="flex min-h-0 flex-1 flex-col p-3">
        <h2 className="text-xs font-semibold uppercase tracking-wide text-muted-foreground">
          Definition{" "}
          <span className="font-normal normal-case tracking-normal">
            ({nodes.length} steps, {edges.length} edges)
          </span>
        </h2>
        <pre
          data-testid="definition-preview"
          className="mt-1 flex-1 overflow-auto rounded bg-muted p-2 text-[11px] leading-snug text-muted-foreground"
        >
          {preview}
        </pre>
      </div>
    </div>
  );
}
