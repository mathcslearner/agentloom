import { describe, expect, it } from "vitest";
import { toDefinition, toFlow } from "@agentloom/graphdef";
import { canvasToFlow, edgeRenderType, seedEdges, seedNodes } from "@/lib/builder/adapter";

// A definition exercising a plain edge, a decision edge, and a loop edge — the
// three handle/render cases seedEdges must derive.
const DEF = {
  schema_version: 1,
  name: "adapter_fixture",
  steps: [
    { id: "gate", type: "human_approval", config: { title: "ok?" } },
    { id: "work", type: "llm", config: { model: "mock/sim-1", prompt: "go" } },
    { id: "publish", type: "echo", config: {} },
  ],
  edges: [
    { from: "gate", to: "work", decision: "approve" },
    { from: "gate", to: "publish", decision: "reject" },
    { from: "work", to: "gate", type: "loop", condition: "x", max_iterations: 2 },
  ],
  ui: { nodes: { work: { position: { x: 100, y: 50 } } } },
};

describe("seed*", () => {
  it("derives handles and render type per edge", () => {
    const flow = toFlow(DEF);
    const edges = seedEdges(flow);
    expect(edges.map((e) => e.sourceHandle)).toEqual(["approve", "reject", "loop"]);
    expect(edges.every((e) => e.targetHandle === "in")).toBe(true);
    expect(edges.map((e) => e.type)).toEqual(["step", "step", "loop"]);
  });

  it("edgeRenderType keys loop vs step", () => {
    expect(edgeRenderType({ from: "a", to: "b", type: "loop", condition: "x", max_iterations: 1 })).toBe("loop");
    expect(edgeRenderType({ from: "a", to: "b" })).toBe("step");
  });
});

describe("canvasToFlow strips runtime fields losslessly", () => {
  it("round-trips through toDefinition at the JSON-value level", () => {
    const flow = toFlow(DEF);
    // Simulate React Flow having added runtime fields to the canvas arrays.
    const nodes = seedNodes(flow).map((n) => ({ ...n, selected: true, dragging: false, measured: { width: 208, height: 60 } }));
    const edges = seedEdges(flow).map((e) => ({ ...e, selected: false }));
    const back = canvasToFlow({ doc: flow.doc, ui: flow.ui, uiPresent: flow.uiPresent, nodes, edges });
    // No runtime fields leak onto the Flow shapes.
    expect(Object.keys(back.nodes[0]!).sort()).toEqual(["data", "id", "position", "type"]);
    // And the definition round-trips value-equal to the input.
    expect(toDefinition(back)).toEqual(DEF);
  });
});
