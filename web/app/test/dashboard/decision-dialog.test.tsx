import { describe, expect, it, vi, beforeEach } from "vitest";
import { render, screen, fireEvent, waitFor } from "@testing-library/react";
import { DecisionDialog } from "@/components/dashboard/DecisionDialog";
import { emptyTopology, type TopoEdge } from "@/lib/pure/dashboard/graph-topology";
import type { ApprovalRecord } from "@/lib/pure/dashboard/approvals";

const decideApproval = vi.fn();
const refetchApproval = vi.fn();
vi.mock("@/lib/dashboard/streams", () => ({
  decideApproval: (...a: unknown[]) => decideApproval(...a),
  refetchApproval: (...a: unknown[]) => refetchApproval(...a),
}));

const gate: ApprovalRecord = {
  id: "ap1", run_id: "r1", step_id: "gate", attempt: 1, status: "pending",
  title: "Publish?", description: "review it",
  payload: { text: "hello" },
  allowed_decisions: ["approve", "reject"],
  allow_edit: true,
  edit_schema: { type: "object", properties: { text: { type: "string" } }, required: ["text"] },
  created_at: "2026-08-18T09:00:00Z", lastSeq: 1, partial: false,
};

function topo(edges: Partial<TopoEdge>[]) {
  const t = emptyTopology();
  edges.forEach((e, i) => t.edges.set(`e${i}`, { from: "gate", to: "x", type: "normal", resolution: "unresolved", graphVersion: 1, origin: { kind: "definition" }, ...e }));
  return t;
}

beforeEach(() => {
  decideApproval.mockReset();
  refetchApproval.mockReset();
  decideApproval.mockResolvedValue({ kind: "ok", response: { approval: { ...gate, status: "approved" }, run: {}, readied_steps: [] } });
});

describe("DecisionDialog", () => {
  it("approves and closes", async () => {
    const onOpenChange = vi.fn();
    render(<DecisionDialog approval={gate} open onOpenChange={onOpenChange} />);
    fireEvent.click(screen.getByTestId("decision-approve"));
    await waitFor(() => expect(decideApproval).toHaveBeenCalledWith("ap1", { decision: "approve" }));
    await waitFor(() => expect(onOpenChange).toHaveBeenCalledWith(false));
  });

  it("blocks approve-with-edit until the payload is valid, then sends edited_payload", async () => {
    render(<DecisionDialog approval={gate} open onOpenChange={vi.fn()} />);
    fireEvent.click(screen.getByTestId("decision-edit-toggle"));
    // Make it invalid (missing required text).
    fireEvent.change(screen.getByTestId("decision-editor"), { target: { value: "{}" } });
    expect(screen.getByTestId("decision-client-issues")).toBeInTheDocument();
    // Approve is disabled while client issues exist.
    expect((screen.getByTestId("decision-approve") as HTMLButtonElement).disabled).toBe(true);
    // Fix it.
    fireEvent.change(screen.getByTestId("decision-editor"), { target: { value: '{"text":"edited"}' } });
    fireEvent.click(screen.getByTestId("decision-approve"));
    await waitFor(() =>
      expect(decideApproval).toHaveBeenCalledWith("ap1", { decision: "approve", edited_payload: { text: "edited" } }),
    );
  });

  it("shows the reject plan from the topology", () => {
    render(
      <DecisionDialog approval={gate} topology={topo([{ to: "notify", decision: "reject" }])} stepConfig={{ on_reject: "route" }} open onOpenChange={vi.fn()} />,
    );
    expect(screen.getByTestId("decision-reject-plan").textContent).toContain("notify");
  });

  it("surfaces a 422's issues", async () => {
    decideApproval.mockResolvedValue({ kind: "invalid", message: "bad", issues: [{ code: "", severity: "error", path: "/text", msg: "too long" }] });
    render(<DecisionDialog approval={gate} open onOpenChange={vi.fn()} />);
    fireEvent.click(screen.getByTestId("decision-approve"));
    await waitFor(() => expect(screen.getByTestId("decision-issues").textContent).toContain("too long"));
  });

  it("recovers from a 409 by showing the decided-elsewhere state (DoD-2)", async () => {
    decideApproval.mockResolvedValue({ kind: "not_pending", message: "not pending" });
    refetchApproval.mockResolvedValue({ ...gate, status: "approved", decided_by: "other" });
    render(<DecisionDialog approval={gate} open onOpenChange={vi.fn()} />);
    fireEvent.click(screen.getByTestId("decision-approve"));
    await waitFor(() => expect(screen.getByTestId("decision-conflict")).toBeInTheDocument());
    expect(screen.getByTestId("decision-conflict").textContent).toContain("another session");
  });
});
