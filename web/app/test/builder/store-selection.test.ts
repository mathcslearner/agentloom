import { beforeEach, describe, expect, it } from "vitest";
import { toFlow } from "@agentloom/graphdef";
import { emptyDefinition, useBuilderStore } from "@/lib/builder/store";
import { edgeData } from "@/lib/builder/adapter";

const store = useBuilderStore;
const past = () => store.temporal.getState().pastStates.length;

beforeEach(() => {
  store.getState().load(toFlow(emptyDefinition()));
});

describe("selectOnly", () => {
  it("selects exactly one node and clears others, without a history entry", () => {
    store.getState().addStep("llm");
    store.getState().addStep("tool");
    const [a, b] = store.getState().nodes;
    const before = past();

    store.getState().selectOnly("node", b!.id);
    const nodes = store.getState().nodes;
    expect(nodes.find((n) => n.id === b!.id)!.selected).toBe(true);
    expect(nodes.find((n) => n.id === a!.id)!.selected).toBe(false);
    // Selection is not tracked (partialize excludes it) → no undo entry.
    expect(past()).toBe(before);
  });

  it("selects an edge and clears node selection", () => {
    store.getState().addStep("llm");
    store.getState().addStep("tool");
    const [a, b] = store.getState().nodes;
    store.getState().connect({ source: a!.id, target: b!.id, sourceHandle: "out", targetHandle: "in" });
    const edgeId = store.getState().edges[0]!.id;

    store.getState().selectOnly("edge", edgeId);
    expect(store.getState().edges[0]!.selected).toBe(true);
    expect(store.getState().nodes.every((n) => !n.selected)).toBe(true);
  });
});

describe("patchEdge", () => {
  it("marks a normal edge as a loop and re-derives the render type + handle", () => {
    store.getState().addStep("llm");
    store.getState().addStep("tool");
    const [a, b] = store.getState().nodes;
    store.getState().connect({ source: a!.id, target: b!.id, sourceHandle: "out", targetHandle: "in" });
    const id = store.getState().edges[0]!.id;
    expect(store.getState().edges[0]!.type).toBe("step");

    store.getState().patchEdge(id, { type: "loop", condition: "output.again == true", max_iterations: 2 });
    const edge = store.getState().edges[0]!;
    expect(edge.type).toBe("loop");
    expect(edge.sourceHandle).toBe("loop");
    const ed = edgeData(edge).edge as unknown as Record<string, unknown>;
    expect(ed["type"]).toBe("loop");
    expect(ed["condition"]).toBe("output.again == true");
    expect(ed["max_iterations"]).toBe(2);
  });

  it("removes keys patched to undefined (loop → normal)", () => {
    store.getState().addStep("llm");
    store.getState().addStep("tool");
    const [a, b] = store.getState().nodes;
    store.getState().connect({ source: a!.id, target: b!.id, sourceHandle: "out", targetHandle: "in" });
    const id = store.getState().edges[0]!.id;
    store.getState().patchEdge(id, { type: "loop", condition: "x", max_iterations: 2 });
    store.getState().patchEdge(id, { type: undefined, condition: undefined, max_iterations: undefined });
    const ed = edgeData(store.getState().edges[0]!).edge as unknown as Record<string, unknown>;
    expect("type" in ed).toBe(false);
    expect("condition" in ed).toBe(false);
    expect(store.getState().edges[0]!.type).toBe("step");
  });
});
