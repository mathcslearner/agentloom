"use client";

import { useState } from "react";
import Link from "next/link";
import { useRunController } from "@/lib/dashboard/useRunController";
import { runStatusVariant } from "@/lib/status";
import { Badge } from "@/components/ui/badge";
import { ConnectionPill } from "@/components/dashboard/ConnectionPill";
import { StepsPane } from "@/components/dashboard/StepsPane";
import { InspectorPane } from "@/components/dashboard/InspectorPane";
import { Timeline } from "@/components/dashboard/Timeline";

/**
 * The live run-detail view (ticket 18.1): a header (status, counters, cost,
 * connection), a graph/steps pane, an inspector pane, and an event timeline
 * strip. 18.2 replaces the steps pane with the React Flow canvas + status
 * skins; 18.3 fills out the inspector. The step map + selection they need are
 * already wired here.
 */
export function RunDetail({ runId }: { runId: string }) {
  const s = useRunController(runId);
  const [selected, setSelected] = useState<string | undefined>(undefined);

  const run = s.run?.run;
  const steps = s.run?.steps;
  const selectedStep = selected && steps ? steps.get(selected) : undefined;

  return (
    <div className="flex min-h-0 flex-1 flex-col" data-testid="run-detail" data-run-status={run?.status ?? ""}>
      {/* Header */}
      <header className="flex flex-wrap items-center gap-x-4 gap-y-2 border-b px-6 py-3">
        <Link href="/runs" className="text-sm text-muted-foreground hover:text-foreground">
          ← Runs
        </Link>
        <span className="font-mono text-sm" data-testid="run-id">
          {runId}
        </span>
        {run ? (
          <Badge variant={runStatusVariant(run.status)} data-testid="run-status">
            {run.status}
          </Badge>
        ) : (
          <span className="text-sm text-muted-foreground">loading…</span>
        )}
        {run ? (
          <span className="text-sm text-muted-foreground tabular-nums" data-testid="run-counters">
            {run.steps_succeeded}/{run.steps_total} ok
            {run.steps_failed > 0 ? ` · ${run.steps_failed} failed` : ""}
            {run.steps_skipped > 0 ? ` · ${run.steps_skipped} skipped` : ""}
          </span>
        ) : null}
        {run?.park_reason ? (
          <span className="text-xs text-amber-600 dark:text-amber-400">parked: {run.park_reason}</span>
        ) : null}
        {run && run.cost.spent_nano_usd > 0 ? (
          <span className="text-sm text-muted-foreground tabular-nums">
            ${(run.cost.spent_nano_usd / 1e9).toFixed(6).replace(/0+$/, "").replace(/\.$/, "")}
          </span>
        ) : null}
        <div className="ml-auto">
          <ConnectionPill state={s.connection} reconnects={s.reconnects} lastSeq={s.lastSeq} />
        </div>
      </header>

      {s.error ? (
        <p className="px-6 py-2 text-sm text-red-600 dark:text-red-400">{s.error}</p>
      ) : null}

      {/* Body: graph/steps pane + inspector */}
      <div className="flex min-h-0 flex-1">
        <div className="min-h-0 flex-1 overflow-auto border-r p-4">
          <StepsPane steps={steps} selected={selected} onSelect={setSelected} />
        </div>
        <div className="min-h-0 w-96 shrink-0 overflow-auto p-4">
          <InspectorPane step={selectedStep} />
        </div>
      </div>

      {/* Timeline strip */}
      <Timeline events={s.events} onSelectStep={setSelected} />
    </div>
  );
}
