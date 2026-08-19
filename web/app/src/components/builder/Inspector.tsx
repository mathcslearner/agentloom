"use client";

// Inspector (ticket 17.4, extended 17.5) — the right pane. It shows the
// schema-driven config panel for a selected step, the minimal edge inspector
// for a selected edge, or a selection summary; below it the whole-document
// Problems panel (click-to-focus) and the live definition preview.

import { useBuilderStore } from "@/lib/builder/store";
import { ConfigPanel } from "./config/ConfigPanel";
import { EdgePanel } from "./EdgePanel";
import { ProblemsPanel } from "./ProblemsPanel";

export function Inspector({ className }: { className?: string }) {
  const nodes = useBuilderStore((s) => s.nodes);
  const edges = useBuilderStore((s) => s.edges);
  const toDefinitionValue = useBuilderStore((s) => s.toDefinitionValue);

  const selectedNodes = nodes.filter((n) => n.selected);
  const selectedEdges = edges.filter((e) => e.selected);
  const preview = JSON.stringify(toDefinitionValue(), null, 2);

  return (
    <div className={className} aria-label="Inspector">
      {selectedNodes.length === 1 ? (
        <ConfigPanel selected={selectedNodes[0]!} />
      ) : selectedNodes.length === 0 && selectedEdges.length === 1 ? (
        <EdgePanel selected={selectedEdges[0]!} />
      ) : (
        <div className="border-b p-3">
          <h2 className="text-xs font-semibold uppercase tracking-wide text-muted-foreground">Selection</h2>
          <p className="mt-1 text-sm text-muted-foreground">
            {selectedNodes.length === 0 && selectedEdges.length === 0
              ? "No step selected."
              : `${selectedNodes.length} steps, ${selectedEdges.length} edges selected.`}
          </p>
        </div>
      )}

      <ProblemsPanel className="border-t" />

      <div className="flex min-h-0 shrink-0 flex-col border-t p-3" style={{ maxHeight: "35%" }}>
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
