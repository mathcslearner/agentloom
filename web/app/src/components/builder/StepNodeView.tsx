// StepNodeView — the presentational body of a step node (ticket 17.3). Pure and
// status-neutral: it renders the step's id, type label, and a one-line config
// summary, with a `selected` ring and an optional `skin` slot (an accent
// class + a status chip) that the live dashboard (M18.2) supplies to paint
// per-step run status. No React Flow import — this is the shared visual the
// builder and the dashboard both render, so it stays free of canvas coupling.

import type { Step } from "@agentloom/graphdef";
import { cn } from "@/lib/utils";
import { stepMeta } from "@/lib/pure/builder/catalog";

/** A run-status skin the dashboard (M18) layers over the neutral node. */
export interface NodeSkin {
  /** Extra class on the node shell (e.g. a status-tinted border). */
  className?: string;
  /** A short status chip rendered in the header (e.g. "running"). */
  badge?: React.ReactNode;
}

export interface StepNodeViewProps {
  step: Step;
  selected?: boolean;
  skin?: NodeSkin;
  /** Count of config-validation errors on the step (17.4); marks the node. */
  problemCount?: number;
}

export function StepNodeView({ step, selected, skin, problemCount = 0 }: StepNodeViewProps) {
  const meta = stepMeta(step.type);
  const invalid = problemCount > 0;
  return (
    <div
      data-testid="step-node"
      data-step-type={step.type}
      data-selected={selected ? "true" : "false"}
      data-invalid={invalid ? "true" : "false"}
      className={cn(
        "w-52 rounded-md border bg-card text-card-foreground shadow-sm transition-colors",
        selected ? "border-primary ring-2 ring-ring" : "border-border",
        invalid ? "border-destructive ring-1 ring-destructive" : "",
        skin?.className,
      )}
    >
      <div className="flex items-center justify-between gap-2 border-b px-3 py-1.5">
        <span className="truncate text-sm font-medium" title={step.id}>
          {step.id}
        </span>
        <div className="flex shrink-0 items-center gap-1.5">
          {invalid ? (
            <span
              data-testid="node-problem-badge"
              title={`${problemCount} config ${problemCount === 1 ? "error" : "errors"}`}
              className="rounded bg-destructive/15 px-1.5 py-0.5 text-[10px] font-semibold text-destructive"
            >
              {problemCount}
            </span>
          ) : null}
          {skin?.badge}
          <span className="rounded bg-muted px-1.5 py-0.5 text-[10px] font-medium uppercase tracking-wide text-muted-foreground">
            {meta.label}
          </span>
        </div>
      </div>
      <div className="truncate px-3 py-2 text-xs text-muted-foreground" title={meta.summary(step)}>
        {meta.summary(step)}
      </div>
    </div>
  );
}
