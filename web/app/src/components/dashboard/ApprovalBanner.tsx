"use client";

/**
 * "Waiting on you" run-header banner (ticket 18.5): when a run has one or more
 * pending human-approval gates, a dismissible banner names them and offers a
 * Decide action into the first. Sits beside the 18.4 budget banners.
 */
import { useMemo } from "react";
import type { ApprovalMap } from "@/lib/pure/dashboard/approvals";
import { decidable } from "@/lib/pure/dashboard/approvals";

export function ApprovalBanner({
  approvals,
  onDecide,
  canDecide = true,
}: {
  approvals: ApprovalMap | undefined;
  onDecide: (approvalId: string, stepId: string) => void;
  /** Whether the caller may decide (ticket 18.6). False (missing `approve`
   * scope) shows the banner but hides the Decide button. */
  canDecide?: boolean;
}) {
  const pending = useMemo(() => {
    if (!approvals) return [];
    return [...approvals.values()].filter(decidable);
  }, [approvals]);

  const first = pending[0];
  if (!first) return null;
  const label =
    pending.length === 1
      ? `1 approval waiting on you: ${first.title || first.step_id}`
      : `${pending.length} approvals waiting on you`;

  return (
    <div
      data-testid="approval-banner"
      data-count={pending.length}
      className="flex items-center justify-between gap-3 border-b border-violet-500/40 bg-violet-500/10 px-6 py-2 text-sm"
    >
      <span className="text-violet-800 dark:text-violet-200">{label}</span>
      {canDecide ? (
        <button
          type="button"
          data-testid="approval-banner-decide"
          className="rounded bg-violet-600 px-2 py-0.5 text-xs font-medium text-white hover:bg-violet-500"
          onClick={() => onDecide(first.id, first.step_id)}
        >
          Decide →
        </button>
      ) : null}
    </div>
  );
}
