"use client";

// BuilderShell (ticket 17.3, extended 17.5) — the builder layout: a toolbar with
// undo/redo, a live problem count, and a (17.6) submit affordance blocked by
// errors; the palette on the left, the canvas in the centre, and the inspector
// on the right. Everything is wrapped in a ReactFlowProvider (canvas coordinate
// helpers) and a ProblemsProvider (one whole-graph validation shared by the
// canvas marks, the edge highlights, and the Problems panel's click-to-focus).

import { useEffect, useRef } from "react";
import { ReactFlowProvider } from "@xyflow/react";
import { toFlow } from "@agentloom/graphdef";
import "@xyflow/react/dist/style.css";
import { redo, undo, useBuilderStore } from "@/lib/builder/store";
import { fetchDefinition } from "@/lib/builder/persistence";
import { toast } from "@/components/ui/toast";
import { useBuilderKeyboardShortcuts } from "@/lib/builder/useKeyboardShortcuts";
import { useCatalogStore } from "@/lib/builder/catalog-store";
import { ProblemsProvider } from "@/lib/builder/problems-context";
import { Button } from "@/components/ui/button";
import { Canvas } from "./Canvas";
import { Palette } from "./Palette";
import { Inspector } from "./Inspector";
import { BuilderActions } from "./BuilderActions";
import { NavigationGuard } from "./NavigationGuard";

function Toolbar() {
  const count = useBuilderStore((s) => s.nodes.length);
  return (
    <div className="flex items-center gap-2 border-b px-3 py-2">
      <span className="text-sm font-semibold tracking-tight">Builder</span>
      <div className="ml-2 flex items-center gap-1.5">
        <Button variant="outline" size="sm" onClick={() => undo()} aria-label="Undo">
          Undo
        </Button>
        <Button variant="outline" size="sm" onClick={() => redo()} aria-label="Redo">
          Redo
        </Button>
      </div>
      <span className="ml-3 text-xs text-muted-foreground" data-testid="node-count">
        {count} {count === 1 ? "step" : "steps"}
      </span>

      <div className="ml-auto">
        <BuilderActions />
      </div>
    </div>
  );
}

export function BuilderShell({ openDefinitionId }: { openDefinitionId?: string }) {
  useBuilderKeyboardShortcuts();
  const loadCatalog = useCatalogStore((s) => s.load);
  const load = useBuilderStore((s) => s.load);
  const openedRef = useRef<string | null>(null);
  useEffect(() => {
    void loadCatalog();
  }, [loadCatalog]);

  // "Open in builder": load a stored definition once per id (client-side, so the
  // spec fetch rides the same-origin proxy).
  useEffect(() => {
    if (!openDefinitionId || openedRef.current === openDefinitionId) return;
    openedRef.current = openDefinitionId;
    void (async () => {
      const res = await fetchDefinition(openDefinitionId);
      if (res.kind !== "ok") {
        toast({ title: "Could not open definition", description: res.message, variant: "error" });
        return;
      }
      try {
        load(toFlow(res.spec), { id: res.id, name: res.name, version: res.version });
        toast({ title: `Opened ${res.name} v${res.version}`, variant: "success" });
      } catch (e) {
        toast({ title: "Could not open definition", description: (e as Error).message, variant: "error" });
      }
    })();
  }, [openDefinitionId, load]);

  return (
    <ReactFlowProvider>
      <ProblemsProvider>
        <div className="flex h-full min-h-0 flex-col">
          <Toolbar />
          <div className="flex min-h-0 flex-1">
            <Palette className="w-52 shrink-0 border-r" />
            <div className="min-w-0 flex-1">
              <Canvas />
            </div>
            <Inspector className="flex w-80 shrink-0 flex-col border-l" />
          </div>
          <NavigationGuard />
        </div>
      </ProblemsProvider>
    </ReactFlowProvider>
  );
}
