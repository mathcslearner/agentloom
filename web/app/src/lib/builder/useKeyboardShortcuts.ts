"use client";

// Keyboard shortcuts for the builder (ticket 17.3 — keyboard a11y basics):
// undo (Mod+Z), redo (Mod+Shift+Z / Mod+Y), and delete the selection
// (Delete / Backspace). Bound on the window; ignored while focus is in a text
// field so typing a config value never deletes a node or triggers undo.

import { useEffect } from "react";
import { redo, undo, useBuilderStore } from "./store";

function isTextEntry(el: EventTarget | null): boolean {
  if (!(el instanceof HTMLElement)) return false;
  const tag = el.tagName;
  return tag === "INPUT" || tag === "TEXTAREA" || tag === "SELECT" || el.isContentEditable;
}

export function useBuilderKeyboardShortcuts(): void {
  const deleteSelected = useBuilderStore((s) => s.deleteSelected);

  useEffect(() => {
    function onKeyDown(e: KeyboardEvent) {
      if (isTextEntry(e.target)) return;
      const mod = e.metaKey || e.ctrlKey;
      const key = e.key.toLowerCase();

      if (mod && key === "z") {
        e.preventDefault();
        if (e.shiftKey) redo();
        else undo();
        return;
      }
      if (mod && key === "y") {
        e.preventDefault();
        redo();
        return;
      }
      if (key === "delete" || key === "backspace") {
        e.preventDefault();
        deleteSelected();
      }
    }

    window.addEventListener("keydown", onKeyDown);
    return () => window.removeEventListener("keydown", onKeyDown);
  }, [deleteSelected]);
}
