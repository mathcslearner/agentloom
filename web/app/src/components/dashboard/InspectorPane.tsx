"use client";

import { useEffect, useState } from "react";
import type { RunCostResponse } from "@agentloom/api-client";
import type { EventEnvelope } from "@agentloom/engine-client";
import { stepStatusVariant } from "@/lib/status";
import { cn } from "@/lib/utils";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { toast } from "@/components/ui/toast";
import { usePermissions } from "@/lib/permissions";
import { useRunControls } from "@/lib/dashboard/useRunControls";
import type { RunController } from "@/lib/dashboard/run-controller";
import { useStepDetailRefresh } from "@/lib/dashboard/useStepInspector";
import type { StepState } from "@/lib/pure/dashboard/run-state";
import { OverviewTab } from "./inspector/OverviewTab";
import { OutputTab } from "./inspector/OutputTab";
import { LogsTab } from "./inspector/LogsTab";
import { ValidationTab } from "./inspector/ValidationTab";
import { CostTab } from "./inspector/CostTab";
import { ApprovalTab } from "./inspector/ApprovalTab";
import { recordsForStep, type ApprovalMap } from "@/lib/pure/dashboard/approvals";
import type { GraphTopology } from "@/lib/pure/dashboard/graph-topology";

type TabKey = "overview" | "output" | "logs" | "validation" | "cost" | "approval";
const BASE_TABS: { key: TabKey; label: string }[] = [
  { key: "overview", label: "Overview" },
  { key: "output", label: "Output" },
  { key: "logs", label: "Logs" },
  { key: "validation", label: "Validation" },
  { key: "cost", label: "Cost" },
];

/**
 * The tabbed step inspector (ticket 18.3). Overview / Output / Logs /
 * Validation / Cost tabs render the selected step's durable projection plus the
 * live event feed. The step's `view` is refreshed from the run-detail body as
 * the step advances (`useStepDetailRefresh`), so new attempts/verdicts appear
 * without a page reload.
 */
export function InspectorPane({
  runId,
  step,
  events,
  cost,
  controller,
  approvals,
  topology,
  onDecide,
}: {
  runId: string;
  step?: StepState;
  events: EventEnvelope[];
  cost?: RunCostResponse;
  controller: RunController;
  approvals?: ApprovalMap;
  topology?: GraphTopology;
  onDecide?: (approvalId: string, stepId: string) => void;
}) {
  const [tab, setTab] = useState<TabKey>("overview");
  const perms = usePermissions();
  const controls = useRunControls(runId, controller);
  useStepDetailRefresh(controller, step);

  // Reset to Overview when the selection changes.
  useEffect(() => {
    setTab("overview");
  }, [step?.id]);

  if (!step) {
    return <p className="text-sm text-muted-foreground">Select a step to inspect it.</p>;
  }
  const view = step.view;

  // The gate's latest approval record (if this is a human_approval step) —
  // adds an Approval tab (18.5).
  const stepRecords = approvals ? recordsForStep(approvals, step.id) : [];
  const record = stepRecords.length > 0 ? stepRecords[stepRecords.length - 1] : undefined;
  const TABS = record ? [...BASE_TABS, { key: "approval" as TabKey, label: "Approval" }] : BASE_TABS;

  return (
    <div className="flex min-h-0 flex-col" data-testid="inspector-pane" data-step-id={step.id}>
      <div className="mb-2 flex items-center gap-2">
        <span className="font-mono text-sm">{step.id}</span>
        <Badge variant={stepStatusVariant(step.status)}>{step.status}</Badge>
        <span className="text-xs text-muted-foreground">{step.type || "—"}</span>
        {step.status === "dead_lettered" && perms.controlState("requeue") !== "hidden" ? (
          <Button
            size="sm"
            className="ml-auto"
            disabled={perms.controlState("requeue") === "disabled" || controls.pending}
            data-testid="inspector-requeue"
            onClick={() => {
              void controls.requeue(step.id).then((ok) => {
                if (ok) toast({ title: `Requeued ${step.id}`, variant: "success" });
              });
            }}
          >
            Requeue
          </Button>
        ) : null}
      </div>

      {view ? (
        <>
          <nav className="mb-3 flex gap-1 border-b" role="tablist">
            {TABS.map((t) => (
              <button
                key={t.key}
                type="button"
                role="tab"
                aria-selected={tab === t.key}
                data-testid={`inspector-tab-${t.key}`}
                onClick={() => setTab(t.key)}
                className={cn(
                  "-mb-px border-b-2 px-2 py-1 text-xs",
                  tab === t.key
                    ? "border-foreground font-medium text-foreground"
                    : "border-transparent text-muted-foreground hover:text-foreground",
                )}
              >
                {t.label}
              </button>
            ))}
          </nav>
          <div className="min-h-0 flex-1 overflow-auto">
            {tab === "overview" ? <OverviewTab step={view} events={events} /> : null}
            {tab === "output" ? <OutputTab step={view} /> : null}
            {tab === "logs" ? <LogsTab runId={runId} step={view} /> : null}
            {tab === "validation" ? <ValidationTab step={view} /> : null}
            {tab === "cost" ? <CostTab step={view} cost={cost} /> : null}
            {tab === "approval" && record ? (
              <ApprovalTab
                record={record}
                topology={topology}
                step={step}
                now={Date.now()}
                onDecide={onDecide}
              />
            ) : null}
          </div>
        </>
      ) : (
        <p className="text-xs italic text-muted-foreground">
          Injected mid-run; full detail loads on the next refresh.
        </p>
      )}
    </div>
  );
}
