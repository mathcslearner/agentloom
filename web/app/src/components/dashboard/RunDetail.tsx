"use client";

import { useMemo, useState } from "react";
import Link from "next/link";
import { useRunController } from "@/lib/dashboard/useRunController";
import { useRunCost } from "@/lib/dashboard/useStepInspector";
import { runStatusVariant } from "@/lib/status";
import { Badge } from "@/components/ui/badge";
import { ConnectionPill } from "@/components/dashboard/ConnectionPill";
import { RunGraph } from "@/components/dashboard/graph/RunGraph";
import { InspectorPane } from "@/components/dashboard/InspectorPane";
import { Timeline } from "@/components/dashboard/Timeline";
import { emptyTopology } from "@/lib/pure/dashboard/graph-topology";

/**
 * The live run-detail view: a header (status, counters, cost, connection), the
 * live DAG canvas (18.2), an inspector pane, and an event timeline strip. 18.3
 * fills out the inspector; the step map + selection are wired here.
 */
export function RunDetail({ runId }: { runId: string }) {
  const { state: s, controller } = useRunController(runId);
  const [selected, setSelected] = useState<string | undefined>(undefined);

  const run = s.run?.run;
  const steps = s.run?.steps;
  const topology = s.topology ?? emptyTopology();
  const selectedStep = selected && steps ? steps.get(selected) : undefined;

  // The cost breakdown feeds the inspector's Cost tab; refetch when a
  // cost_updated event lands (the count of them is the live cost cursor).
  const costSeq = useMemo(() => s.events.filter((e) => e.type === "cost_updated").length, [s.events]);
  const cost = useRunCost(runId, costSeq);

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

      {/* Body: live DAG canvas + inspector */}
      <div className="flex min-h-0 flex-1">
        <div className="min-h-0 flex-1 border-r">
          {steps && steps.size > 0 ? (
            <RunGraph topology={topology} steps={steps} selected={selected} onSelect={setSelected} />
          ) : (
            <p className="p-4 text-sm text-muted-foreground">Waiting for steps…</p>
          )}
        </div>
        <div className="flex min-h-0 w-[26rem] shrink-0 flex-col overflow-hidden p-4">
          <InspectorPane
            runId={runId}
            step={selectedStep}
            events={s.events}
            cost={cost}
            controller={controller}
          />
        </div>
      </div>

      {/* Timeline strip */}
      <Timeline events={s.events} onSelectStep={setSelected} />
    </div>
  );
}
