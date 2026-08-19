"use client";

import { useMemo, useState } from "react";
import type { EventEnvelope } from "@agentloom/engine-client";
import {
  eventCategory,
  eventStepId,
  EVENT_CATEGORIES,
  summarizeEvent,
  type EventCategory,
} from "@/lib/pure/dashboard/events";
import { cn } from "@/lib/utils";
import { Button } from "@/components/ui/button";

/**
 * The event timeline strip (ticket 18.1): the normalized event feed, newest
 * last, with category filter chips (type filtering) and click-to-select-step.
 * Collapsible; follows the latest event when expanded.
 */
export function Timeline({
  events,
  onSelectStep,
}: {
  events: EventEnvelope[];
  onSelectStep?: (stepId: string) => void;
}) {
  const [open, setOpen] = useState(true);
  const [active, setActive] = useState<Set<EventCategory>>(new Set());

  const visible = useMemo(() => {
    if (active.size === 0) return events;
    return events.filter((e) => active.has(eventCategory(e.type)));
  }, [events, active]);

  const toggle = (c: EventCategory) => {
    setActive((prev) => {
      const next = new Set(prev);
      if (next.has(c)) next.delete(c);
      else next.add(c);
      return next;
    });
  };

  return (
    <div className="shrink-0 border-t" data-testid="timeline">
      <div className="flex flex-wrap items-center gap-2 px-4 py-2">
        <button
          className="text-xs font-medium text-muted-foreground hover:text-foreground"
          onClick={() => setOpen((o) => !o)}
          data-testid="timeline-toggle"
        >
          {open ? "▾" : "▸"} Timeline ({events.length})
        </button>
        <div className="flex flex-wrap gap-1">
          {EVENT_CATEGORIES.map((c) => (
            <Button
              key={c}
              size="sm"
              variant={active.has(c) ? "default" : "outline"}
              onClick={() => toggle(c)}
              data-testid={`timeline-filter-${c}`}
            >
              {c}
            </Button>
          ))}
          {active.size > 0 ? (
            <Button size="sm" variant="ghost" onClick={() => setActive(new Set())}>
              clear
            </Button>
          ) : null}
        </div>
      </div>
      {open ? (
        <ol className="max-h-56 overflow-auto px-4 pb-3 text-xs" data-testid="timeline-list">
          {visible.map((e) => {
            const stepId = eventStepId(e);
            return (
              <li
                key={`${e.run_id}:${e.seq}`}
                data-testid="timeline-row"
                data-seq={e.seq}
                data-type={e.type}
                data-category={eventCategory(e.type)}
                className={cn(
                  "flex items-center gap-2 border-b border-border/40 py-1",
                  stepId && "cursor-pointer hover:bg-accent",
                )}
                onClick={() => stepId && onSelectStep?.(stepId)}
              >
                <span className="w-10 shrink-0 tabular-nums text-muted-foreground">{e.seq}</span>
                <span className="w-40 shrink-0 truncate font-mono text-[10px] text-muted-foreground">
                  {e.type}
                </span>
                <span className="truncate">{summarizeEvent(e)}</span>
                <span className="ml-auto shrink-0 text-[10px] text-muted-foreground">
                  {new Date(e.ts).toLocaleTimeString()}
                </span>
              </li>
            );
          })}
          {visible.length === 0 ? (
            <li className="py-2 text-muted-foreground">No events match this filter.</li>
          ) : null}
        </ol>
      ) : null}
    </div>
  );
}
