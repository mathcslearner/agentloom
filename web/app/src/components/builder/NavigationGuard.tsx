"use client";

// The unsaved-changes guard (ticket 17.6). While the canvas is dirty it:
//   - warns on a hard navigation / refresh / tab close via `beforeunload`;
//   - intercepts in-app link navigation (the header nav) with a confirm prompt,
//     since Next's client-side routing does not fire `beforeunload`.
// The Import action's discard prompt lives in BuilderActions (a canvas
// replacement, not a navigation). Rendered once inside the builder shell.

import { useCallback, useEffect, useState } from "react";
import { useRouter } from "next/navigation";
import { selectIsDirty, useBuilderStore } from "@/lib/builder/store";
import { ConfirmDialog } from "./dialogs/ConfirmDialog";

export function NavigationGuard() {
  const router = useRouter();
  const [pending, setPending] = useState<string | null>(null);

  useEffect(() => {
    const handler = (e: BeforeUnloadEvent) => {
      if (!selectIsDirty(useBuilderStore.getState())) return;
      e.preventDefault();
      e.returnValue = "";
    };
    window.addEventListener("beforeunload", handler);
    return () => window.removeEventListener("beforeunload", handler);
  }, []);

  const onClick = useCallback((e: MouseEvent) => {
    if (e.defaultPrevented || e.button !== 0 || e.metaKey || e.ctrlKey || e.shiftKey || e.altKey) return;
    const anchor = (e.target as HTMLElement | null)?.closest?.("a");
    if (!anchor) return;
    const href = anchor.getAttribute("href");
    if (!href || !href.startsWith("/") || anchor.target === "_blank") return;
    if (href === window.location.pathname) return;
    if (!selectIsDirty(useBuilderStore.getState())) return;
    e.preventDefault();
    e.stopPropagation();
    setPending(href);
  }, []);

  useEffect(() => {
    document.addEventListener("click", onClick, { capture: true });
    return () => document.removeEventListener("click", onClick, { capture: true });
  }, [onClick]);

  return (
    <ConfirmDialog
      open={pending !== null}
      title="Leave the builder?"
      description="You have unsaved changes. Leaving this page will discard them."
      confirmLabel="Discard & leave"
      cancelLabel="Stay"
      onConfirm={() => {
        const href = pending;
        setPending(null);
        if (href) router.push(href);
      }}
      onCancel={() => setPending(null)}
    />
  );
}
