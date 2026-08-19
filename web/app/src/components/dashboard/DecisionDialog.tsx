"use client";

/**
 * The human-approval decision dialog (ticket 18.5). Renders the gate's proposed
 * action, an optional schema-validated JSON editor (when the gate permits an
 * edit), and approve/reject controls with a comment. It surfaces what a reject
 * will do (fail vs route) from the gate config + graph edges, validates an
 * edited payload client-side before submit (the server 422 is authoritative and
 * its issues are shown too), and recovers gracefully from a 409 when another
 * session decided the approval first — flipping to a read-only "already decided"
 * state (DoD-2).
 */
import { useEffect, useMemo, useState } from "react";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { Button } from "@/components/ui/button";
import { Textarea } from "@/components/ui/textarea";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Checkbox } from "@/components/ui/checkbox";
import { JsonViewer } from "@/components/dashboard/inspector/JsonViewer";
import { useApprovalActions } from "@/lib/dashboard/useApprovalActions";
import { refetchApproval, type DecisionOutcome } from "@/lib/dashboard/streams";
import { validateEditedPayload, type EditIssue } from "@/lib/pure/dashboard/edit-validate";
import { rejectPlan, type RejectPlan } from "@/lib/pure/dashboard/decision-outcome";
import { decidable, parkExpired, type ApprovalRecord } from "@/lib/pure/dashboard/approvals";
import { decisionOutcome } from "@/lib/pure/dashboard/decision-outcome";
import type { GraphTopology } from "@/lib/pure/dashboard/graph-topology";
import type { ApprovalView } from "@agentloom/api-client";

export interface DecisionDialogProps {
  approval: ApprovalRecord;
  /** The gate's materialized config (for the reject plan), when available. */
  stepConfig?: unknown;
  /** The live topology (for reject-route targets), when available. */
  topology?: GraphTopology;
  open: boolean;
  onOpenChange: (open: boolean) => void;
  /** Called after a decision commits (or a 409 reveals the current record), so
   * the caller can reconcile its state. */
  onDecided?: (outcome: DecisionOutcome, current?: ApprovalView) => void;
}

export function DecisionDialog({
  approval,
  stepConfig,
  topology,
  open,
  onOpenChange,
  onDecided,
}: DecisionDialogProps) {
  const { pending, error, issues, decide, reset } = useApprovalActions();
  const [editing, setEditing] = useState(false);
  const [editText, setEditText] = useState("");
  const [comment, setComment] = useState("");
  const [conflict, setConflict] = useState<ApprovalRecord | undefined>();

  const allowedApprove = approval.allowed_decisions.includes("approve");
  const allowedReject = approval.allowed_decisions.includes("reject");
  const canEdit = approval.allow_edit === true && allowedApprove;
  const plan: RejectPlan | undefined = topology ? rejectPlan(stepConfig, topology, approval.step_id) : undefined;

  // Reset on open.
  useEffect(() => {
    if (open) {
      setEditing(false);
      setEditText(JSON.stringify(approval.payload ?? {}, null, 2));
      setComment("");
      setConflict(undefined);
      reset();
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [open, approval.id]);

  const clientIssues: EditIssue[] = useMemo(() => {
    if (!editing) return [];
    const res = validateEditedPayload(editText, approval.edit_schema);
    return res.ok ? [] : res.issues;
  }, [editing, editText, approval.edit_schema]);

  const decided = !decidable(approval) || conflict !== undefined;
  const shownRecord = conflict ?? approval;

  async function submit(decision: "approve" | "reject") {
    let editedPayload: unknown | undefined;
    if (decision === "approve" && editing) {
      const res = validateEditedPayload(editText, approval.edit_schema);
      if (!res.ok) return; // client issues are shown; block until valid
      editedPayload = res.value;
    }
    const outcome = await decide(approval.id, {
      decision,
      ...(editedPayload !== undefined ? { edited_payload: editedPayload } : {}),
      ...(comment.trim() ? { comment: comment.trim() } : {}),
    });
    if (outcome.kind === "ok") {
      onDecided?.(outcome);
      onOpenChange(false);
      return;
    }
    if (outcome.kind === "not_pending") {
      // Another session decided first — reveal the current record read-only.
      const current = await refetchApproval(approval.id, approval.run_id);
      if (current) setConflict({ ...current, lastSeq: approval.lastSeq, partial: false });
      onDecided?.(outcome, current);
    }
    // invalid/forbidden/error surface via `error`/`issues` from the hook.
  }

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent data-testid="decision-dialog" data-approval-id={approval.id}>
        <DialogHeader>
          <DialogTitle>{approval.title || "Approval"}</DialogTitle>
          {approval.description ? <DialogDescription>{approval.description}</DialogDescription> : null}
        </DialogHeader>

        {decided ? (
          <div className="space-y-2" data-testid="decision-conflict">
            <p className="text-sm text-amber-600 dark:text-amber-400">
              {conflict
                ? "This approval was decided in another session."
                : "This approval is no longer pending."}
            </p>
            <p className="text-sm text-muted-foreground">
              {decisionOutcome(shownRecord as ApprovalRecord, topology)}
            </p>
            {shownRecord.run_id ? (
              <a
                href={`/runs/${shownRecord.run_id}`}
                className="text-sm text-primary underline"
                data-testid="decision-run-link"
              >
                Open run
              </a>
            ) : null}
          </div>
        ) : (
          <div className="space-y-3">
            {parkExpired(approval) ? (
              <p className="text-xs text-amber-600 dark:text-amber-400" data-testid="decision-park-note">
                This gate timed out and parked the run; it can still be decided.
              </p>
            ) : null}

            <div className="space-y-1">
              <Label>Proposed action</Label>
              {editing ? (
                <div className="space-y-1">
                  <Textarea
                    data-testid="decision-editor"
                    rows={8}
                    className="font-mono text-[12px]"
                    value={editText}
                    onChange={(e) => setEditText(e.target.value)}
                  />
                  {clientIssues.length > 0 ? (
                    <ul className="space-y-0.5 text-xs text-destructive" data-testid="decision-client-issues">
                      {clientIssues.map((i, n) => (
                        <li key={n}>
                          {i.path ? `${i.path}: ` : ""}
                          {i.message}
                        </li>
                      ))}
                    </ul>
                  ) : null}
                </div>
              ) : (
                <div className="rounded-md border bg-muted/30 p-2">
                  <JsonViewer value={approval.payload ?? null} />
                </div>
              )}
              {canEdit ? (
                <label className="flex items-center gap-2 text-xs text-muted-foreground">
                  <Checkbox
                    checked={editing}
                    data-testid="decision-edit-toggle"
                    onChange={(e) => setEditing(e.target.checked)}
                  />
                  Edit the payload before approving
                </label>
              ) : null}
            </div>

            <div className="space-y-1">
              <Label htmlFor="decision-comment">Comment (optional)</Label>
              <Input
                id="decision-comment"
                data-testid="decision-comment"
                value={comment}
                onChange={(e) => setComment(e.target.value)}
                placeholder="Why you approve or reject…"
              />
            </div>

            {allowedReject && plan ? (
              <p className="text-xs text-muted-foreground" data-testid="decision-reject-plan">
                {plan.policy === "route" && plan.targets.length > 0
                  ? `Reject routes to: ${plan.targets.join(", ")}.`
                  : "Reject fails the run."}
              </p>
            ) : null}

            {issues && issues.length > 0 ? (
              <ul className="space-y-0.5 text-xs text-destructive" data-testid="decision-issues">
                {issues.map((i, n) => (
                  <li key={n}>
                    {i.path ? `${i.path}: ` : ""}
                    {i.msg}
                  </li>
                ))}
              </ul>
            ) : null}
            {error ? (
              <p className="text-sm text-destructive" data-testid="decision-error">
                {error}
              </p>
            ) : null}
          </div>
        )}

        <DialogFooter>
          {decided ? (
            <Button variant="outline" onClick={() => onOpenChange(false)}>
              Close
            </Button>
          ) : (
            <>
              <Button variant="outline" onClick={() => onOpenChange(false)} disabled={pending}>
                Cancel
              </Button>
              {allowedReject ? (
                <Button
                  variant="destructive"
                  onClick={() => submit("reject")}
                  disabled={pending}
                  data-testid="decision-reject"
                >
                  {pending ? "…" : "Reject"}
                </Button>
              ) : null}
              {allowedApprove ? (
                <Button
                  onClick={() => submit("approve")}
                  disabled={pending || (editing && clientIssues.length > 0)}
                  data-testid="decision-approve"
                >
                  {pending ? "…" : editing ? "Approve with edit" : "Approve"}
                </Button>
              ) : null}
            </>
          )}
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
