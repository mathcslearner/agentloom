/**
 * Reject-plan + decision-outcome derivations for the decision dialog and the
 * Approval inspector tab (ticket 18.5). Pure; no network, no clock.
 *
 * The dialog shows the operator what a reject will do *before* they click it —
 * `on_reject: fail` fails the run; `on_reject: route` sends the run down the
 * reject-marked edge(s). Both facts are already on the client: `on_reject` from
 * the gate's materialized `StepView.config` (18.3), the reject targets from the
 * topology's per-edge `decision` markers (18.2 / graph endpoint). No backend
 * change.
 *
 * No React/UI imports — under the app's `pure/` eslint boundary.
 */
import type { GraphTopology } from "./graph-topology";
import type { ApprovalRecord } from "./approvals";
import type { StepState } from "./run-state";

export type RejectPolicy = "fail" | "route";

export interface RejectPlan {
  policy: RejectPolicy;
  /** The step ids a reject routes to (empty for `fail`). */
  targets: string[];
}

/** What a reject on this gate will do, from the gate's config + reject edges. */
export function rejectPlan(
  stepConfig: unknown,
  topology: GraphTopology,
  stepId: string,
): RejectPlan {
  const policy = onRejectOf(stepConfig);
  if (policy !== "route") return { policy: "fail", targets: [] };
  const targets: string[] = [];
  for (const e of topology.edges.values()) {
    if (e.from === stepId && e.type === "normal" && e.decision === "reject") targets.push(e.to);
  }
  return { policy: "route", targets };
}

/** The approve-path targets (unmarked + approve-marked normal out-edges). */
export function approveTargets(topology: GraphTopology, stepId: string): string[] {
  const targets: string[] = [];
  for (const e of topology.edges.values()) {
    if (e.from === stepId && e.type === "normal" && e.decision !== "reject") targets.push(e.to);
  }
  return targets;
}

/**
 * A rendered one-line outcome for a decided/expired approval — used in the
 * inbox row and the Approval tab. Uses the topology (when available) to name
 * the routed/readied targets.
 */
export function decisionOutcome(
  rec: ApprovalRecord,
  topology?: GraphTopology,
  stepState?: StepState,
): string {
  switch (rec.status) {
    case "pending":
      return rec.expired_at ? "parked at timeout — still awaiting a decision" : "awaiting a decision";
    case "approved": {
      const who = rec.decided_by ? ` by ${rec.decided_by}` : "";
      const edited = rec.decision && rec.edited_payload ? " (edited)" : "";
      const targets = topology ? approveTargets(topology, rec.step_id) : [];
      const routed = targets.length > 0 ? ` → ${targets.join(", ")}` : "";
      return `approved${edited}${who}${routed}`;
    }
    case "rejected": {
      const who = rec.decided_by ? ` by ${rec.decided_by}` : "";
      const targets = topology ? rejectRouteTargets(topology, rec.step_id) : [];
      if (targets.length > 0) return `rejected${who} → routed to ${targets.join(", ")}`;
      // No reject edge ⇒ on_reject: fail; the gate dead-letters and the run
      // fails (or the failed step is visible in the graph).
      const failed = stepState?.status === "dead_lettered" || stepState?.status === "failed";
      return failed ? `rejected${who} → run failed` : `rejected${who}`;
    }
    case "expired": {
      const action =
        rec.decision === "approve"
          ? "auto-approved"
          : rec.decision === "reject"
            ? "auto-rejected"
            : "expired";
      return `timed out — ${action}`;
    }
    case "cancelled":
      return "cancelled";
    default:
      return rec.status;
  }
}

function rejectRouteTargets(topology: GraphTopology, stepId: string): string[] {
  const out: string[] = [];
  for (const e of topology.edges.values()) {
    if (e.from === stepId && e.type === "normal" && e.decision === "reject") out.push(e.to);
  }
  return out;
}

/** Read `on_reject` off a gate's materialized config (`fail` default). */
function onRejectOf(config: unknown): RejectPolicy {
  if (config && typeof config === "object") {
    const v = (config as Record<string, unknown>).on_reject;
    if (v === "route") return "route";
  }
  return "fail";
}
