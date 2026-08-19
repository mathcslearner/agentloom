"use client";

import type { RunCostResponse, StepView } from "@agentloom/api-client";
import { Badge } from "@/components/ui/badge";
import { nanoUsd, stepCost } from "@/lib/pure/dashboard/inspector-cost";

export function CostTab({ step, cost }: { step: StepView; cost?: RunCostResponse }) {
  const c = stepCost(cost, step.id);
  if (c.rows.length === 0) {
    return (
      <div className="text-xs" data-testid="inspector-cost">
        <p className="italic text-muted-foreground">No cost recorded for this step.</p>
      </div>
    );
  }
  return (
    <div className="space-y-4 text-xs" data-testid="inspector-cost">
      <dl className="grid grid-cols-2 gap-x-3 gap-y-1">
        <dt className="text-muted-foreground">Spent</dt>
        <dd className="tabular-nums">{nanoUsd(c.spentNanoUsd)}</dd>
        {c.overheadNanoUsd > 0 ? (
          <>
            <dt className="text-muted-foreground">Overhead (judge)</dt>
            <dd className="tabular-nums">{nanoUsd(c.overheadNanoUsd)}</dd>
          </>
        ) : null}
        {c.savedNanoUsd > 0 ? (
          <>
            <dt className="text-muted-foreground">Saved by cache</dt>
            <dd className="tabular-nums text-emerald-700 dark:text-emerald-400">{nanoUsd(c.savedNanoUsd)}</dd>
          </>
        ) : null}
      </dl>
      <table className="w-full text-[11px]" data-testid="cost-rows">
        <thead className="text-muted-foreground">
          <tr>
            <th className="text-left font-normal">#</th>
            <th className="text-left font-normal">entry</th>
            <th className="text-left font-normal">resource</th>
            <th className="text-right font-normal">tok in/out</th>
            <th className="text-right font-normal">cost</th>
          </tr>
        </thead>
        <tbody>
          {c.rows.map((r, i) => (
            <tr key={i} data-entry={r.entry}>
              <td className="tabular-nums">{r.attempt}</td>
              <td>
                {r.entry}
                {r.overhead ? <Badge variant="parked" className="ml-1 text-[9px]">overhead</Badge> : null}
                {r.cacheHit ? <Badge variant="succeeded" className="ml-1 text-[9px]">cache</Badge> : null}
              </td>
              <td className="font-mono">{r.resource}</td>
              <td className="text-right tabular-nums">
                {r.inputTokens ?? "—"}/{r.outputTokens ?? "—"}
              </td>
              <td className="text-right tabular-nums">
                {r.cacheHit ? <span className="text-emerald-700 dark:text-emerald-400">{nanoUsd(r.savedNanoUsd)}</span> : nanoUsd(r.spentNanoUsd)}
              </td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}
