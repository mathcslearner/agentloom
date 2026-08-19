"use client";

import { stepStatusVariant } from "@/lib/status";
import { Badge } from "@/components/ui/badge";
import { cn } from "@/lib/utils";
import type { StepState } from "@/lib/pure/dashboard/run-state";

/**
 * The graph/steps pane placeholder (ticket 18.1). Renders the run's live step
 * map as status-badged tiles; 18.2 replaces this with the React Flow canvas
 * (reusing the builder node components + run-status skins). Selection is wired
 * through so the inspector and timeline already work.
 */
export function StepsPane({
  steps,
  selected,
  onSelect,
}: {
  steps?: Map<string, StepState>;
  selected?: string;
  onSelect: (id: string) => void;
}) {
  if (!steps || steps.size === 0) {
    return <p className="text-sm text-muted-foreground">Waiting for steps…</p>;
  }
  const list = [...steps.values()];
  return (
    <div className="flex flex-wrap gap-2" data-testid="steps-pane">
      {list.map((s) => (
        <button
          key={s.id}
          onClick={() => onSelect(s.id)}
          data-testid="step-tile"
          data-step-id={s.id}
          data-step-status={s.status}
          className={cn(
            "flex flex-col items-start gap-1 rounded-md border px-3 py-2 text-left transition-colors hover:bg-accent",
            selected === s.id && "ring-2 ring-primary",
          )}
        >
          <span className="font-mono text-xs">{s.id}</span>
          <span className="text-[10px] text-muted-foreground">{s.type || "—"}</span>
          <Badge variant={stepStatusVariant(s.status)}>{s.status}</Badge>
          {s.origin ? (
            <span className="text-[10px] text-muted-foreground" title={`injected by ${s.origin.step}`}>
              ⑂ {s.origin.kind}
            </span>
          ) : null}
        </button>
      ))}
    </div>
  );
}
