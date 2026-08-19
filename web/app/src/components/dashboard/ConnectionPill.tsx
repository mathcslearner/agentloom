"use client";

import { cn } from "@/lib/utils";

/**
 * A small live-connection indicator (ticket 18.1). Green = live, amber =
 * connecting/reconnecting, grey = idle/closed. The reconnect count and last seq
 * are surfaced (with data-testids) so the e2e can assert a mid-run reconnect
 * resumed the feed.
 */
export function ConnectionPill({
  state,
  reconnects,
  lastSeq,
}: {
  state: string;
  reconnects?: number;
  lastSeq?: number;
}) {
  const live = state === "live";
  const connecting = state === "connecting" || state === "backfilling" || state === "reconnecting";
  const color = live ? "bg-green-500" : connecting ? "bg-amber-500" : "bg-zinc-400";
  const label = live ? "live" : connecting ? "connecting…" : state;
  return (
    <span
      className="inline-flex items-center gap-1.5 text-xs text-muted-foreground"
      data-testid="connection-pill"
      data-connection={state}
    >
      <span className={cn("inline-block h-2 w-2 rounded-full", color, connecting && "animate-pulse")} />
      <span data-testid="connection-label">{label}</span>
      {typeof reconnects === "number" && reconnects > 0 ? (
        <span data-testid="reconnect-count" className="text-amber-600 dark:text-amber-400">
          · {reconnects} reconnect{reconnects === 1 ? "" : "s"}
        </span>
      ) : null}
      {typeof lastSeq === "number" ? (
        <span data-testid="last-seq" className="tabular-nums text-muted-foreground/70">
          · seq {lastSeq}
        </span>
      ) : null}
    </span>
  );
}
