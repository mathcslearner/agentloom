"use client";

// BuilderShell (ticket 17.3) — the builder layout: a toolbar with undo/redo and
// a live node count, the palette on the left, the React Flow canvas in the
// centre, and the inspector on the right. Wraps everything in a
// ReactFlowProvider so the canvas' coordinate helpers work, and installs the
// keyboard shortcuts (undo/redo/delete). The canvas seeds itself from the
// store's empty starting document; import (17.6) calls store.load.

import { ReactFlowProvider } from "@xyflow/react";
import "@xyflow/react/dist/style.css";
import { redo, undo, useBuilderStore } from "@/lib/builder/store";
import { useBuilderKeyboardShortcuts } from "@/lib/builder/useKeyboardShortcuts";
import { Button } from "@/components/ui/button";
import { Canvas } from "./Canvas";
import { Palette } from "./Palette";
import { Inspector } from "./Inspector";

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
      <span className="ml-auto text-xs text-muted-foreground" data-testid="node-count">
        {count} {count === 1 ? "step" : "steps"}
      </span>
    </div>
  );
}

export function BuilderShell() {
  useBuilderKeyboardShortcuts();
  return (
    <ReactFlowProvider>
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
    </ReactFlowProvider>
  );
}
