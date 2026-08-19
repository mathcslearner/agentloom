"use client";

/**
 * The inspector's Approval tab (ticket 18.5), shown for a `human_approval` gate.
 * It renders the gate's current record — pending (with a Decide affordance),
 * decided (the outcome), or expired — plus the proposed payload and aging.
 */
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { JsonViewer } from "./JsonViewer";
import type { ApprovalRecord } from "@/lib/pure/dashboard/approvals";
import { decidable, parkExpired } from "@/lib/pure/dashboard/approvals";
import { decisionOutcome } from "@/lib/pure/dashboard/decision-outcome";
import { deriveAging, deriveDeadline } from "@/lib/pure/dashboard/approval-aging";
import type { GraphTopology } from "@/lib/pure/dashboard/graph-topology";
import type { StepState } from "@/lib/pure/dashboard/run-state";

export function ApprovalTab({
  record,
  topology,
  step,
  now,
  onDecide,
}: {
  record: ApprovalRecord;
  topology?: GraphTopology;
  step?: StepState;
  now: number;
  onDecide?: (approvalId: string, stepId: string) => void;
}) {
  const aging = deriveAging(record, now);
  const deadline = deriveDeadline(record, now);
  const pending = decidable(record);

  return (
    <div className="space-y-3 text-sm" data-testid="approval-tab" data-approval-status={record.status}>
      <div className="flex items-center gap-2">
        <Badge variant={pending ? "parked" : "muted"}>{record.status}</Badge>
        <span className="text-xs text-muted-foreground">
          {aging.label} old
          {parkExpired(record) ? " · parked at timeout" : ""}
        </span>
      </div>

      <p className="text-sm text-muted-foreground" data-testid="approval-outcome">
        {decisionOutcome(record, topology, step)}
      </p>

      {pending && deadline ? (
        <p
          className={deadline.overdue ? "text-xs text-red-600 dark:text-red-400" : "text-xs text-muted-foreground"}
          data-testid="approval-deadline"
        >
          {deadline.label}
        </p>
      ) : null}

      {record.comment ? (
        <p className="text-xs">
          <span className="text-muted-foreground">comment: </span>
          {record.comment}
        </p>
      ) : null}

      <div className="space-y-1">
        <p className="text-xs font-medium text-muted-foreground">Proposed action</p>
        <div className="rounded-md border bg-muted/30 p-2">
          <JsonViewer value={record.edited_payload ?? record.payload ?? null} />
        </div>
      </div>

      {pending && onDecide ? (
        <Button data-testid="approval-tab-decide" onClick={() => onDecide(record.id, record.step_id)}>
          Decide
        </Button>
      ) : null}
    </div>
  );
}
