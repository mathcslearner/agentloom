"use client";

import { stepStatusVariant } from "@/lib/status";
import { Badge } from "@/components/ui/badge";
import type { StepState } from "@/lib/pure/dashboard/run-state";

/**
 * The inspector pane placeholder (ticket 18.1): the selected step's basic facts
 * and its raw projection. 18.3 replaces this with the tabbed inspector
 * (overview / output / logs / validation / cost).
 */
export function InspectorPane({ step }: { step?: StepState }) {
  if (!step) {
    return <p className="text-sm text-muted-foreground">Select a step to inspect it.</p>;
  }
  return (
    <div className="space-y-3" data-testid="inspector-pane" data-step-id={step.id}>
      <div className="flex items-center gap-2">
        <span className="font-mono text-sm">{step.id}</span>
        <Badge variant={stepStatusVariant(step.status)}>{step.status}</Badge>
      </div>
      <dl className="grid grid-cols-2 gap-x-3 gap-y-1 text-xs">
        <dt className="text-muted-foreground">Type</dt>
        <dd>{step.type || "—"}</dd>
        <dt className="text-muted-foreground">Attempt</dt>
        <dd className="tabular-nums">{step.attempt}</dd>
        {step.origin ? (
          <>
            <dt className="text-muted-foreground">Origin</dt>
            <dd>
              {step.origin.step} ({step.origin.kind})
            </dd>
          </>
        ) : null}
      </dl>
      {step.view ? (
        <details>
          <summary className="cursor-pointer text-xs text-muted-foreground">Raw step projection</summary>
          <pre className="mt-2 max-h-96 overflow-auto rounded-md bg-muted p-2 text-[11px]">
            {JSON.stringify(step.view, null, 2)}
          </pre>
        </details>
      ) : (
        <p className="text-xs italic text-muted-foreground">
          Injected mid-run; full detail loads on the next snapshot.
        </p>
      )}
    </div>
  );
}
