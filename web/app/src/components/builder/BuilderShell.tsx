"use client";

// BuilderShell (ticket 17.3, extended 17.5) — the builder layout: a toolbar with
// undo/redo, a live problem count, and a (17.6) submit affordance blocked by
// errors; the palette on the left, the canvas in the centre, and the inspector
// on the right. Everything is wrapped in a ReactFlowProvider (canvas coordinate
// helpers) and a ProblemsProvider (one whole-graph validation shared by the
// canvas marks, the edge highlights, and the Problems panel's click-to-focus).

import { useEffect } from "react";
import { ReactFlowProvider } from "@xyflow/react";
import "@xyflow/react/dist/style.css";
import { redo, undo, useBuilderStore } from "@/lib/builder/store";
import { useBuilderKeyboardShortcuts } from "@/lib/builder/useKeyboardShortcuts";
import { useCatalogStore } from "@/lib/builder/catalog-store";
import { ProblemsProvider, useProblemsCtx } from "@/lib/builder/problems-context";
import { Button } from "@/components/ui/button";
import { Canvas } from "./Canvas";
import { Palette } from "./Palette";
import { Inspector } from "./Inspector";

function Toolbar() {
  const count = useBuilderStore((s) => s.nodes.length);
  const { problems, focusIssue } = useProblemsCtx();
  const errorCount = problems.errors.length;
  const warningCount = problems.warnings.length;
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

      <div className="ml-auto flex items-center gap-3">
        {errorCount > 0 || warningCount > 0 ? (
          <button
            type="button"
            data-testid="problem-count"
            onClick={() => {
              const first = problems.errors[0] ?? problems.warnings[0];
              if (first) focusIssue(first);
            }}
            className="text-xs font-medium"
            title="Jump to the first problem"
          >
            {errorCount > 0 ? (
              <span className="text-destructive">
                {errorCount} {errorCount === 1 ? "error" : "errors"}
              </span>
            ) : null}
            {errorCount > 0 && warningCount > 0 ? <span className="text-muted-foreground"> · </span> : null}
            {warningCount > 0 ? (
              <span className="text-amber-600 dark:text-amber-500">
                {warningCount} {warningCount === 1 ? "warning" : "warnings"}
              </span>
            ) : null}
          </button>
        ) : (
          <span className="text-xs font-medium text-emerald-600 dark:text-emerald-500" data-testid="problem-count-ok">
            No problems
          </span>
        )}
        <span className="text-xs text-muted-foreground" data-testid="node-count">
          {count} {count === 1 ? "step" : "steps"}
        </span>
        <Button
          size="sm"
          disabled={errorCount > 0}
          aria-label="Submit run"
          data-testid="submit-run"
          title={errorCount > 0 ? `${errorCount} ${errorCount === 1 ? "error" : "errors"} block submit` : "Submit a run (17.6)"}
        >
          Submit
        </Button>
      </div>
    </div>
  );
}

export function BuilderShell() {
  useBuilderKeyboardShortcuts();
  const loadCatalog = useCatalogStore((s) => s.load);
  useEffect(() => {
    void loadCatalog();
  }, [loadCatalog]);
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
        </div>
      </ProblemsProvider>
    </ReactFlowProvider>
  );
}
