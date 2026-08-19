import { describe, expect, it } from "vitest";
import {
  applyApprovalEvent,
  approvalsFromViews,
  decidable,
  mergeApprovalViews,
  parkExpired,
  pendingForStep,
  recordsForStep,
  type ApprovalMap,
} from "@/lib/pure/dashboard/approvals";
import { approvalListFixture } from "./inspector-fixtures";
import { makeEnv } from "./helpers";

const RUN = "run-9";

function req(seq: number, id: string, over: Record<string, unknown> = {}) {
  return makeEnv(
    "approval_requested",
    seq,
    { approval_id: id, step_id: "gate", attempt: 1, title: "Publish?", allowed_decisions: ["approve", "reject"], allow_edit: true, ...over },
    "gate",
  );
}

describe("applyApprovalEvent", () => {
  it("seeds a partial pending record from approval_requested", () => {
    const m = applyApprovalEvent(new Map(), req(1, "ap1"), RUN);
    const rec = m.get("ap1")!;
    expect(rec.status).toBe("pending");
    expect(rec.partial).toBe(true);
    expect(rec.title).toBe("Publish?");
    expect(rec.run_id).toBe(RUN);
    expect(decidable(rec)).toBe(true);
  });

  it("is idempotent under seq guard", () => {
    const m = applyApprovalEvent(new Map(), req(5, "ap1"), RUN);
    const same = applyApprovalEvent(m, req(3, "ap1"), RUN);
    expect(same).toBe(m); // same identity, stale event dropped
  });

  it("folds a decision to approved/rejected", () => {
    let m = applyApprovalEvent(new Map(), req(1, "ap1"), RUN);
    m = applyApprovalEvent(m, makeEnv("approval_decided", 2, { approval_id: "ap1", step_id: "gate", attempt: 1, decision: "approve", decided_by: "key_x", source: "human", comment: "ok" }, "gate"), RUN);
    const rec = m.get("ap1")!;
    expect(rec.status).toBe("approved");
    expect(rec.decision).toBe("approve");
    expect(rec.decided_by).toBe("key_x");
    expect(decidable(rec)).toBe(false);
  });

  it("expired: on_timeout reject settles to expired", () => {
    let m = applyApprovalEvent(new Map(), req(1, "ap1"), RUN);
    m = applyApprovalEvent(m, makeEnv("approval_expired", 2, { approval_id: "ap1", step_id: "gate", attempt: 1, policy: "reject", decision: "reject", action: "rejected", timeout_at: "2026-08-18T09:00:00Z" }, "gate"), RUN);
    const rec = m.get("ap1")!;
    expect(rec.status).toBe("expired");
    expect(decidable(rec)).toBe(false);
  });

  it("park-expired: on_timeout park stays pending + decidable", () => {
    let m = applyApprovalEvent(new Map(), req(1, "ap1"), RUN);
    m = applyApprovalEvent(m, makeEnv("approval_expired", 2, { approval_id: "ap1", step_id: "gate", attempt: 1, policy: "park", action: "run_parked", timeout_at: "2026-08-18T09:00:00Z" }, "gate"), RUN);
    const rec = m.get("ap1")!;
    expect(rec.status).toBe("pending");
    expect(rec.expired_at).toBeDefined();
    expect(parkExpired(rec)).toBe(true);
    expect(decidable(rec)).toBe(true);
  });

  it("cancel settles to cancelled", () => {
    let m = applyApprovalEvent(new Map(), req(1, "ap1"), RUN);
    m = applyApprovalEvent(m, makeEnv("approval_cancelled", 2, { approval_id: "ap1", step_id: "gate", reason: "run_cancelled" }, "gate"), RUN);
    expect(m.get("ap1")!.status).toBe("cancelled");
  });
});

describe("mergeApprovalViews", () => {
  it("completes a partial record from a REST body", () => {
    let m: ApprovalMap = applyApprovalEvent(new Map(), req(10, "ap1"), RUN);
    expect(m.get("ap1")!.partial).toBe(true);
    // A stale body (bodySeq < record.lastSeq) backfills shape but keeps status.
    const view = { id: "ap1", run_id: RUN, step_id: "gate", attempt: 1, status: "pending" as const, title: "Publish?", payload: { text: "hi" }, allowed_decisions: ["approve", "reject"] as ("approve" | "reject")[], allow_edit: true, edit_schema: { type: "object" }, created_at: "2026-08-18T09:00:00Z" };
    m = mergeApprovalViews(m, [view], RUN, 5);
    const rec = m.get("ap1")!;
    expect(rec.partial).toBe(false);
    expect(rec.payload).toEqual({ text: "hi" });
    expect(rec.edit_schema).toEqual({ type: "object" });
  });

  it("a fresher body replaces the record and advances lastSeq", () => {
    const view = approvalListFixture.approvals[0]!;
    const m = mergeApprovalViews(new Map(), [view], view.run_id ?? RUN, 100);
    expect(m.get(view.id)!.lastSeq).toBe(100);
    expect(m.get(view.id)!.partial).toBe(false);
  });
});

describe("selectors over the golden", () => {
  it("seeds from views and finds the pending gate for its step", () => {
    const m = approvalsFromViews(approvalListFixture.approvals, "33333333-3333-3333-3333-333333333333", 200);
    // fixture row 1 (approve_publish) is pending.
    const pending = pendingForStep(m, "approve_publish");
    expect(pending?.status).toBe("pending");
    // row 5 (approve_d) is park-expired: pending + expired_at.
    const parked = pendingForStep(m, "approve_d");
    expect(parked && parkExpired(parked)).toBe(true);
    // a decided row is not decidable.
    expect(recordsForStep(m, "approve_a").every((r) => !decidable(r))).toBe(true);
  });
});
