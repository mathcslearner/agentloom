"use client";

/**
 * Decision action for the approval dialog (ticket 18.5). Drives
 * `POST /v1/approvals/{id}:decide`, tracking pending state and the last
 * outcome. Like the budget hook it holds no optimistic state — the decision is
 * reflected live through the `approval_decided` event and the returned response
 * — but it surfaces the two approval-specific 4xx the dialog must recover from:
 * a 422 (`invalid`, carrying edit-schema `issues`) and a 409 (`not_pending`, a
 * concurrent decision from another session — the DoD-2 case). The component
 * decides how to render each; this hook only classifies and returns.
 */
import { useCallback, useState } from "react";
import type { Issue } from "@agentloom/api-client";
import { decideApproval, type DecisionOutcome } from "@/lib/dashboard/streams";

export interface DecideRequest {
  decision: "approve" | "reject";
  edited_payload?: unknown;
  comment?: string;
}

export interface ApprovalActionsState {
  pending: boolean;
  error?: string;
  /** Edit-schema violations from a 422; cleared on the next attempt. */
  issues?: Issue[];
  decide: (approvalID: string, req: DecideRequest) => Promise<DecisionOutcome>;
  reset: () => void;
}

export function useApprovalActions(): ApprovalActionsState {
  const [pending, setPending] = useState(false);
  const [error, setError] = useState<string>();
  const [issues, setIssues] = useState<Issue[]>();

  const decide = useCallback(
    async (approvalID: string, req: DecideRequest): Promise<DecisionOutcome> => {
      setPending(true);
      setError(undefined);
      setIssues(undefined);
      try {
        const outcome = await decideApproval(approvalID, req);
        if (outcome.kind === "invalid") {
          setError(outcome.message);
          setIssues(outcome.issues);
        } else if (outcome.kind !== "ok") {
          setError(outcome.message);
        }
        return outcome;
      } finally {
        setPending(false);
      }
    },
    [],
  );

  const reset = useCallback(() => {
    setError(undefined);
    setIssues(undefined);
  }, []);

  return { pending, error, issues, decide, reset };
}
