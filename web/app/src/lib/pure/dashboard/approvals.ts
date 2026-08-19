/**
 * Pure approval-record reducer (ticket 18.5).
 *
 * A `human_approval` gate parks its step without a lease (ADR-017); the operator
 * decides it through `POST /v1/approvals/{id}:decide`. The dashboard tracks the
 * pending/decided state of each approval two ways, folded here:
 *
 *   - live from the `approval_*` event feed (the low-latency signal), and
 *   - from a `GET /v1/runs/{id}` body's `approvals[]` (the authoritative record).
 *
 * An `approval_requested` event carries only id/step/title/allowed_decisions/
 * allow_edit/timeout_at — NOT the payload, description, or edit_schema — so a
 * record folded from an event alone is `partial: true` and must be completed by
 * a REST fetch before the decision dialog can render the payload/edit form.
 *
 * Every fold is guarded by a per-record `lastSeq`: an event with seq ≤ the
 * record's cursor is a no-op, so replaying a backfill or a reconnected suffix is
 * idempotent, and a status is always set absolutely (never incremented).
 *
 * No React/UI imports — under the app's `pure/` eslint boundary.
 */
import type { EventEnvelope, EventType } from "@agentloom/engine-client";
import type { ApprovalView } from "@agentloom/api-client";

/** The approval events the inbox + run detail fold. */
export const APPROVAL_EVENT_TYPES: readonly EventType[] = [
  "approval_requested",
  "approval_decided",
  "approval_expired",
  "approval_cancelled",
] as const;

export type ApprovalStatus = ApprovalView["status"];

/**
 * A tracked approval: the REST view plus two dashboard-local fields.
 *   - `lastSeq`: the highest event seq folded into this record (the resume
 *     cursor + idempotence key).
 *   - `partial`: true when the record was seeded from an `approval_requested`
 *     event and has not yet been completed by a REST body (no payload/edit
 *     schema yet).
 */
export interface ApprovalRecord extends ApprovalView {
  lastSeq: number;
  partial: boolean;
}

export type ApprovalMap = Map<string, ApprovalRecord>;

/** True when a record can still be decided by a human. A `park`-timeout row
 * (on_timeout: park) stays `pending` with `expired_at` stamped — still
 * decidable — which is why the check is on status, not on expiry. */
export function decidable(rec: Pick<ApprovalRecord, "status">): boolean {
  return rec.status === "pending";
}

/** True when a timeout escalation stamped `expired_at` but left the row pending
 * (on_timeout: park) — the inbox renders this distinctly from a plain expired. */
export function parkExpired(rec: Pick<ApprovalRecord, "status" | "expired_at">): boolean {
  return rec.status === "pending" && rec.expired_at !== undefined;
}

/** The pending approval for a step, if any (a step has at most one open gate). */
export function pendingForStep(map: ApprovalMap, stepId: string): ApprovalRecord | undefined {
  for (const rec of map.values()) {
    if (rec.step_id === stepId && decidable(rec)) return rec;
  }
  return undefined;
}

/** All records for a step (any status), newest-decided last. */
export function recordsForStep(map: ApprovalMap, stepId: string): ApprovalRecord[] {
  const out: ApprovalRecord[] = [];
  for (const rec of map.values()) if (rec.step_id === stepId) out.push(rec);
  return out;
}

/**
 * Fold one approval event into the map (returns the same map identity when the
 * event was already reflected). `runId` stamps `run_id` on a record seeded from
 * an event (the run-scoped feed knows its run; the event payload does not carry
 * it, but the run-detail caller does).
 */
export function applyApprovalEvent(
  map: ApprovalMap,
  env: EventEnvelope,
  runId?: string,
): ApprovalMap {
  const id = approvalIdOf(env);
  if (!id) return map;
  const prev = map.get(id);
  if (prev && env.seq <= prev.lastSeq) return map;

  const next = new Map(map);
  const base: ApprovalRecord = prev ?? {
    id,
    run_id: runId,
    step_id: env.step_id ?? "",
    attempt: 0,
    status: "pending",
    title: "",
    allowed_decisions: [],
    created_at: env.ts,
    lastSeq: 0,
    partial: true,
  };

  switch (env.type) {
    case "approval_requested": {
      const p = env.payload;
      next.set(id, {
        ...base,
        run_id: base.run_id ?? runId,
        step_id: p.step_id,
        attempt: p.attempt,
        status: "pending",
        title: p.title,
        allowed_decisions: p.allowed_decisions as ApprovalView["allowed_decisions"],
        allow_edit: p.allow_edit,
        timeout_at: p.timeout_at,
        lastSeq: env.seq,
        // Keep any richer fields a prior REST fold supplied; still partial if
        // we have not seen a body (no payload).
        partial: base.payload === undefined,
      });
      break;
    }
    case "approval_decided": {
      const p = env.payload;
      next.set(id, {
        ...base,
        status: p.decision === "approve" ? "approved" : "rejected",
        decision: p.decision as ApprovalView["decision"],
        comment: p.comment,
        decided_by: p.decided_by,
        decision_source: p.source as ApprovalView["decision_source"],
        lastSeq: env.seq,
      });
      break;
    }
    case "approval_expired": {
      const p = env.payload;
      // action "run_parked" ⇒ on_timeout: park — the row stays pending and
      // decidable; only stamp expired_at. Otherwise the timeout policy settled
      // the approval (reject/approve): status expired.
      const parked = p.action === "run_parked";
      next.set(id, {
        ...base,
        status: parked ? "pending" : "expired",
        decision: p.decision as ApprovalView["decision"],
        decision_source: "timeout",
        timeout_at: p.timeout_at ?? base.timeout_at,
        expired_at: p.timeout_at ?? base.expired_at ?? env.ts,
        lastSeq: env.seq,
      });
      break;
    }
    case "approval_cancelled": {
      next.set(id, { ...base, status: "cancelled", lastSeq: env.seq });
      break;
    }
    default:
      return map;
  }
  return next;
}

/**
 * Fold a run body's authoritative `approvals[]` into the map. A REST record
 * completes a partial one (payload/edit_schema) and never regresses a record
 * whose live cursor is already past the body's seq — the event feed is
 * authoritative for status, the body for the full shape. Stamps `lastSeq` to
 * `bodySeq` only when it advances the record's cursor.
 */
export function mergeApprovalViews(
  map: ApprovalMap,
  views: ApprovalView[] | undefined,
  runId: string,
  bodySeq: number,
): ApprovalMap {
  if (!views || views.length === 0) return map;
  const next = new Map(map);
  for (const v of views) {
    const prev = next.get(v.id);
    if (prev && prev.lastSeq > bodySeq) {
      // The live feed is fresher; keep its status but backfill the shape fields
      // a partial live record lacks (payload/description/edit_schema/attempt).
      if (prev.partial) {
        next.set(v.id, {
          ...prev,
          run_id: prev.run_id ?? v.run_id ?? runId,
          attempt: v.attempt,
          title: v.title || prev.title,
          description: v.description ?? prev.description,
          payload: v.payload,
          edit_schema: v.edit_schema,
          allow_edit: v.allow_edit ?? prev.allow_edit,
          allowed_decisions:
            v.allowed_decisions.length > 0 ? v.allowed_decisions : prev.allowed_decisions,
          partial: false,
        });
      }
      continue;
    }
    next.set(v.id, {
      ...v,
      run_id: v.run_id ?? runId,
      lastSeq: Math.max(prev?.lastSeq ?? 0, bodySeq),
      partial: false,
    });
  }
  return next;
}

/** Seed a map from a run body's approvals (the snapshot path). */
export function approvalsFromViews(
  views: ApprovalView[] | undefined,
  runId: string,
  bodySeq: number,
): ApprovalMap {
  return mergeApprovalViews(new Map(), views, runId, bodySeq);
}

/** The approval id an event addresses, if it is an approval event. */
function approvalIdOf(env: EventEnvelope): string | undefined {
  const p = env.payload as { approval_id?: unknown };
  return typeof p.approval_id === "string" ? p.approval_id : undefined;
}
