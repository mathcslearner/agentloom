"use client";

import { useMemo, useState } from "react";
import Link from "next/link";
import { useRunController } from "@/lib/dashboard/useRunController";
import { useRunCost } from "@/lib/dashboard/useStepInspector";
import { runStatusVariant } from "@/lib/status";
import { Badge } from "@/components/ui/badge";
import { ConnectionPill } from "@/components/dashboard/ConnectionPill";
import { CostMeter } from "@/components/dashboard/CostMeter";
import { BudgetBanners } from "@/components/dashboard/BudgetBanners";
import { RaiseBudgetDialog } from "@/components/dashboard/RaiseBudgetDialog";
import { ApprovalBanner } from "@/components/dashboard/ApprovalBanner";
import { RunControls } from "@/components/dashboard/RunControls";
import { DecisionDialog } from "@/components/dashboard/DecisionDialog";
import { usePermissions } from "@/lib/permissions";
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
  const perms = usePermissions();
  const [selected, setSelected] = useState<string | undefined>(undefined);
  const [budgetOpen, setBudgetOpen] = useState(false);
  const [decideId, setDecideId] = useState<string | undefined>(undefined);

  const run = s.run?.run;
  const steps = s.run?.steps;
  const approvals = s.run?.approvals;
  const topology = s.topology ?? emptyTopology();
  const selectedStep = selected && steps ? steps.get(selected) : undefined;

  const openDecide = (approvalId: string, stepId: string) => {
    setSelected(stepId);
    setDecideId(approvalId);
  };
  const decideRecord = decideId ? approvals?.get(decideId) : undefined;
  const decideStepConfig = decideRecord && steps ? steps.get(decideRecord.step_id)?.view?.config : undefined;

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
        {run?.park_reason && run.park_reason !== "budget_exceeded" ? (
          <span className="text-xs text-amber-600 dark:text-amber-400">parked: {run.park_reason}</span>
        ) : null}
        {run ? <CostMeter run={run} /> : null}
        {run &&
        (run.cost.budget_nano_usd !== undefined || run.status === "parked") &&
        perms.controlState("raiseBudget") !== "hidden" ? (
          <button
            type="button"
            onClick={() => setBudgetOpen(true)}
            disabled={perms.controlState("raiseBudget") === "disabled"}
            data-testid="raise-budget-open"
            className="text-xs text-muted-foreground underline-offset-2 hover:text-foreground hover:underline disabled:opacity-50"
          >
            Raise budget
          </button>
        ) : null}
        <div className="ml-auto flex items-center gap-3">
          {run ? <RunControls run={run} controller={controller} /> : null}
          <ConnectionPill state={s.connection} reconnects={s.reconnects} lastSeq={s.lastSeq} />
        </div>
      </header>

      <BudgetBanners events={s.events} run={run} onRaise={() => setBudgetOpen(true)} />
      <ApprovalBanner
        approvals={approvals}
        onDecide={openDecide}
        canDecide={perms.controlState("decide") !== "hidden"}
      />

      {run ? (
        <RaiseBudgetDialog
          runId={runId}
          run={run}
          controller={controller}
          open={budgetOpen}
          onOpenChange={setBudgetOpen}
        />
      ) : null}

      {decideRecord ? (
        <DecisionDialog
          approval={decideRecord}
          stepConfig={decideStepConfig}
          topology={topology}
          open={decideId !== undefined}
          onOpenChange={(o) => {
            if (!o) setDecideId(undefined);
          }}
          onDecided={() => void controller.refreshViews()}
        />
      ) : null}

      {s.error ? (
        <p className="px-6 py-2 text-sm text-red-600 dark:text-red-400">{s.error}</p>
      ) : null}

      {/* Body: live DAG canvas + inspector */}
      <div className="flex min-h-0 flex-1">
        <div className="min-h-0 flex-1 border-r">
          {steps && steps.size > 0 ? (
            <RunGraph
              topology={topology}
              steps={steps}
              selected={selected}
              onSelect={setSelected}
              approvals={approvals}
              onDecide={openDecide}
            />
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
            approvals={approvals}
            topology={topology}
            onDecide={openDecide}
          />
        </div>
      </div>

      {/* Timeline strip */}
      <Timeline events={s.events} onSelectStep={setSelected} />
    </div>
  );
}
