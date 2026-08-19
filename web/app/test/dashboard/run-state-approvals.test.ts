import { describe, expect, it } from "vitest";
import { applyEvent, fromSnapshot, mergeRunResponse } from "@/lib/pure/dashboard/run-state";
import { makeRun, makeStep, makeEnv } from "./helpers";
import { approvalListFixture } from "./inspector-fixtures";

describe("RunState approvals", () => {
  it("seeds approvals from a snapshot body", () => {
    const st = fromSnapshot({
      run: makeRun({ id: "33333333-3333-3333-3333-333333333333", event_seq: 5 }),
      steps: [makeStep({ id: "approve_publish", type: "human_approval", status: "awaiting_human" })],
      approvals: approvalListFixture.approvals,
    });
    expect(st.approvals.size).toBe(approvalListFixture.approvals.length);
    expect(st.approvals.get(approvalListFixture.approvals[0]!.id)!.partial).toBe(false);
  });

  it("folds an approval_requested event into the map and sets step awaiting_human", () => {
    let st = fromSnapshot({ run: makeRun({ event_seq: 0 }), steps: [makeStep({ id: "gate", type: "human_approval" })] });
    st = applyEvent(
      st,
      makeEnv("approval_requested", 1, { approval_id: "ap1", step_id: "gate", attempt: 1, title: "Publish?", allowed_decisions: ["approve"], allow_edit: true }, "gate"),
    );
    expect(st.steps.get("gate")!.status).toBe("awaiting_human");
    expect(st.approvals.get("ap1")!.status).toBe("pending");
  });

  it("a decision event marks the approval decided without regressing", () => {
    let st = fromSnapshot({ run: makeRun({ event_seq: 0 }), steps: [makeStep({ id: "gate", type: "human_approval" })] });
    st = applyEvent(st, makeEnv("approval_requested", 1, { approval_id: "ap1", step_id: "gate", attempt: 1, title: "t", allowed_decisions: ["approve"] }, "gate"));
    st = applyEvent(st, makeEnv("approval_decided", 2, { approval_id: "ap1", step_id: "gate", attempt: 1, decision: "approve", decided_by: "u", source: "human" }, "gate"));
    expect(st.approvals.get("ap1")!.status).toBe("approved");
  });

  it("mergeRunResponse completes a partial record from the body", () => {
    let st = fromSnapshot({ run: makeRun({ id: "33333333-3333-3333-3333-333333333333", event_seq: 0 }), steps: [makeStep({ id: "approve_publish", type: "human_approval" })] });
    st = applyEvent(st, makeEnv("approval_requested", 1, { approval_id: approvalListFixture.approvals[0]!.id, step_id: "approve_publish", attempt: 1, title: "t", allowed_decisions: ["approve", "reject"] }, "approve_publish"));
    expect(st.approvals.get(approvalListFixture.approvals[0]!.id)!.partial).toBe(true);
    st = mergeRunResponse(st, {
      run: makeRun({ id: "33333333-3333-3333-3333-333333333333", event_seq: 5 }),
      steps: [],
      approvals: approvalListFixture.approvals,
    });
    const rec = st.approvals.get(approvalListFixture.approvals[0]!.id)!;
    expect(rec.partial).toBe(false);
    expect(rec.payload).toBeDefined();
  });
});
