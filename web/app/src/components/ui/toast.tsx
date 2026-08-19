"use client";

// A minimal toast surface (ticket 17.6): save/submit confirmations and errors.
// A tiny zustand store holds the queue; `<Toaster/>` renders it fixed at the
// bottom-right; `toast(...)` (imported anywhere) enqueues one. Deliberately
// dependency-light — no new lib beyond zustand, which the builder already uses.

import { useEffect } from "react";
import { create } from "zustand";
import { cn } from "@/lib/utils";

export type ToastVariant = "default" | "success" | "error";

export interface Toast {
  id: number;
  title: string;
  description?: string;
  variant: ToastVariant;
}

interface ToastState {
  toasts: Toast[];
  push: (t: Omit<Toast, "id">) => number;
  dismiss: (id: number) => void;
}

let nextId = 1;

const useToastStore = create<ToastState>((set) => ({
  toasts: [],
  push: (t) => {
    const id = nextId++;
    set((s) => ({ toasts: [...s.toasts, { ...t, id }] }));
    return id;
  },
  dismiss: (id) => set((s) => ({ toasts: s.toasts.filter((t) => t.id !== id) })),
}));

/** Enqueue a toast from anywhere (event handlers, async flows). */
export function toast(t: { title: string; description?: string; variant?: ToastVariant }): number {
  return useToastStore.getState().push({ title: t.title, description: t.description, variant: t.variant ?? "default" });
}

const VARIANT_CLASS: Record<ToastVariant, string> = {
  default: "border-border",
  success: "border-emerald-500/50",
  error: "border-destructive/60",
};

function ToastCard({ t }: { t: Toast }) {
  const dismiss = useToastStore((s) => s.dismiss);
  useEffect(() => {
    const timer = setTimeout(() => dismiss(t.id), t.variant === "error" ? 8000 : 4000);
    return () => clearTimeout(timer);
  }, [t.id, t.variant, dismiss]);
  return (
    <button
      type="button"
      data-testid="toast"
      data-variant={t.variant}
      onClick={() => dismiss(t.id)}
      className={cn(
        "pointer-events-auto w-80 rounded-md border bg-background px-3 py-2 text-left shadow-md",
        VARIANT_CLASS[t.variant],
      )}
    >
      <p className="text-sm font-medium">{t.title}</p>
      {t.description ? <p className="mt-0.5 break-words text-xs text-muted-foreground">{t.description}</p> : null}
    </button>
  );
}

/** Fixed bottom-right toast stack. Mount once (in the root layout). */
export function Toaster() {
  const toasts = useToastStore((s) => s.toasts);
  return (
    <div className="pointer-events-none fixed bottom-4 right-4 z-[60] flex flex-col gap-2" aria-live="polite" aria-atomic="false">
      {toasts.map((t) => (
        <ToastCard key={t.id} t={t} />
      ))}
    </div>
  );
}
