import { describe, expect, it } from "vitest";
import { approveTargets, decisionOutcome, rejectPlan } from "@/lib/pure/dashboard/decision-outcome";
import { emptyTopology, type GraphTopology, type TopoEdge } from "@/lib/pure/dashboard/graph-topology";
import type { ApprovalRecord } from "@/lib/pure/dashboard/approvals";

function topo(edges: Partial<TopoEdge>[]): GraphTopology {
  const t = emptyTopology();
  edges.forEach((e, i) => {
    const edge: TopoEdge = { from: "gate", to: "x", type: "normal", resolution: "unresolved", graphVersion: 1, origin: { kind: "definition" }, ...e };
    t.edges.set(`e${i}`, edge);
  });
  return t;
}

const rec = (over: Partial<ApprovalRecord>): ApprovalRecord => ({
  id: "a", run_id: "r", step_id: "gate", attempt: 1, status: "pending",
  title: "t", allowed_decisions: ["approve", "reject"], created_at: "2026-08-18T09:00:00Z", lastSeq: 1, partial: false, ...over,
});

describe("rejectPlan", () => {
  it("fail when on_reject is fail (default)", () => {
    const plan = rejectPlan({ on_reject: "fail" }, topo([{ to: "publish" }]), "gate");
    expect(plan).toEqual({ policy: "fail", targets: [] });
  });

  it("route names the reject-marked targets", () => {
    const t = topo([{ to: "publish" }, { to: "notify", decision: "reject" }]);
    const plan = rejectPlan({ on_reject: "route" }, t, "gate");
    expect(plan.policy).toBe("route");
    expect(plan.targets).toEqual(["notify"]);
  });
});

describe("approveTargets", () => {
  it("returns unmarked + approve-marked edges", () => {
    const t = topo([{ to: "publish" }, { to: "notify", decision: "reject" }, { to: "audit", decision: "approve" }]);
    expect(approveTargets(t, "gate").sort()).toEqual(["audit", "publish"]);
  });
});

describe("decisionOutcome", () => {
  it("approved with edit names the routed target", () => {
    const t = topo([{ to: "publish" }]);
    const out = decisionOutcome(rec({ status: "approved", decided_by: "u", edited_payload: { text: "x" }, decision: "approve" }), t);
    expect(out).toContain("approved (edited) by u");
    expect(out).toContain("publish");
  });

  it("rejected with a reject edge says routed", () => {
    const t = topo([{ to: "notify", decision: "reject" }]);
    expect(decisionOutcome(rec({ status: "rejected", decided_by: "u" }), t)).toContain("routed to notify");
  });

  it("rejected under fail says run failed when the step dead-lettered", () => {
    const t = topo([{ to: "publish" }]);
    const out = decisionOutcome(rec({ status: "rejected" }), t, { id: "gate", type: "human_approval", status: "dead_lettered", attempt: 1, reclaims: 0 });
    expect(out).toContain("run failed");
  });

  it("expired reads as timed-out auto-rejected", () => {
    expect(decisionOutcome(rec({ status: "expired", decision: "reject" }))).toContain("auto-rejected");
  });

  it("park-expired pending is still awaiting", () => {
    expect(decisionOutcome(rec({ expired_at: "2026-08-18T09:00:00Z" }))).toContain("still awaiting");
  });
});
