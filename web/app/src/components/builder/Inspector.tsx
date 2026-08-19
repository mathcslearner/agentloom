"use client";

// Inspector (ticket 17.4) — the right pane. When exactly one step is selected it
// shows the schema-driven config panel (ConfigPanel); otherwise a selection
// summary. The live definition preview stays below (useful for debugging and
// for tests asserting serialized semantics, e.g. a reject-port decision edge).

import { useBuilderStore } from "@/lib/builder/store";
import { ConfigPanel } from "./config/ConfigPanel";

export function Inspector({ className }: { className?: string }) {
  const nodes = useBuilderStore((s) => s.nodes);
  const edges = useBuilderStore((s) => s.edges);
  const toDefinitionValue = useBuilderStore((s) => s.toDefinitionValue);

  const selected = nodes.filter((n) => n.selected);
  const preview = JSON.stringify(toDefinitionValue(), null, 2);

  return (
    <div className={className} aria-label="Inspector">
      {selected.length === 1 ? (
        <ConfigPanel selected={selected[0]!} />
      ) : (
        <div className="border-b p-3">
          <h2 className="text-xs font-semibold uppercase tracking-wide text-muted-foreground">Selection</h2>
          <p className="mt-1 text-sm text-muted-foreground">
            {selected.length === 0 ? "No step selected." : `${selected.length} steps selected.`}
          </p>
        </div>
      )}
      <div className="flex min-h-0 shrink-0 flex-col border-t p-3" style={{ maxHeight: "40%" }}>
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
