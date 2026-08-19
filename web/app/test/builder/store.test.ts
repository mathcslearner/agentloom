import { beforeEach, describe, expect, it } from "vitest";
import { toFlow } from "@agentloom/graphdef";
import {
  beginInteraction,
  emptyDefinition,
  endInteraction,
  redo,
  undo,
  useBuilderStore,
} from "@/lib/builder/store";

const store = useBuilderStore;
const past = () => store.temporal.getState().pastStates.length;

beforeEach(() => {
  store.getState().load(toFlow(emptyDefinition()));
});

describe("addStep", () => {
  it("adds a node, allocates a typed id, and selects it", () => {
    const id = store.getState().addStep("llm", { x: 0, y: 0 });
    expect(id).toBe("llm_1");
    expect(store.getState().nodes).toHaveLength(1);
    expect(store.getState().nodes[0]!.selected).toBe(true);
  });

  it("allocates unique ids per type", () => {
    store.getState().addStep("llm");
    store.getState().addStep("llm");
    store.getState().addStep("tool");
    expect(store.getState().nodes.map((n) => n.id)).toEqual(["llm_1", "llm_2", "tool_1"]);
  });

  it("is undoable and redoable", () => {
    store.getState().addStep("llm");
    expect(store.getState().nodes).toHaveLength(1);
    undo();
    expect(store.getState().nodes).toHaveLength(0);
    redo();
    expect(store.getState().nodes).toHaveLength(1);
  });
});

describe("connect", () => {
  beforeEach(() => {
    store.getState().addStep("llm");
    store.getState().addStep("echo");
  });

  it("creates an edge honoring the port seed", () => {
    store.getState().connect({ source: "llm_1", target: "echo_1", sourceHandle: "out", targetHandle: "in" });
    const edges = store.getState().edges;
    expect(edges).toHaveLength(1);
    expect(edges[0]!.id).toBe("llm_1->echo_1");
    expect((edges[0]!.data as { edge: unknown }).edge).toMatchObject({ from: "llm_1", to: "echo_1" });
  });

  it("rejects an invalid connection (self-loop)", () => {
    store.getState().connect({ source: "llm_1", target: "llm_1", sourceHandle: "out" });
    expect(store.getState().edges).toHaveLength(0);
  });

  it("gives duplicate edges distinct deterministic ids", () => {
    store.getState().connect({ source: "llm_1", target: "echo_1", sourceHandle: "out" });
    store.getState().connect({ source: "llm_1", target: "echo_1", sourceHandle: "loop" });
    expect(store.getState().edges.map((e) => e.id)).toEqual(["llm_1->echo_1", "llm_1->echo_1#2"]);
  });

  it("is undoable", () => {
    store.getState().connect({ source: "llm_1", target: "echo_1", sourceHandle: "out" });
    expect(store.getState().edges).toHaveLength(1);
    undo();
    expect(store.getState().edges).toHaveLength(0);
  });
});

describe("deleteSelected", () => {
  it("removes selected nodes and their attached edges, and is undoable", () => {
    store.getState().addStep("llm"); // llm_1, selected
    store.getState().addStep("echo"); // echo_1, selected (llm_1 deselected)
    store.getState().connect({ source: "llm_1", target: "echo_1", sourceHandle: "out" });
    // Select llm_1 only.
    store.getState().onNodesChange([
      { type: "select", id: "llm_1", selected: true },
      { type: "select", id: "echo_1", selected: false },
    ]);
    store.getState().deleteSelected();
    expect(store.getState().nodes.map((n) => n.id)).toEqual(["echo_1"]);
    expect(store.getState().edges).toHaveLength(0); // attached edge pruned
    undo();
    expect(store.getState().nodes).toHaveLength(2);
    expect(store.getState().edges).toHaveLength(1);
  });
});

describe("history hygiene", () => {
  it("does not record selection-only changes", () => {
    store.getState().addStep("llm");
    const before = past();
    store.getState().onNodesChange([{ type: "select", id: "llm_1", selected: true }]);
    store.getState().onNodesChange([{ type: "select", id: "llm_1", selected: false }]);
    expect(past()).toBe(before);
  });

  it("coalesces a drag into a single undo entry", () => {
    const id = store.getState().addStep("llm", { x: 0, y: 0 });
    const before = past();
    beginInteraction();
    // Two intermediate position updates during the drag.
    store.getState().onNodesChange([{ type: "position", id, position: { x: 40, y: 40 }, dragging: true }]);
    store.getState().onNodesChange([{ type: "position", id, position: { x: 80, y: 80 }, dragging: true }]);
    endInteraction();
    expect(past()).toBe(before + 1); // exactly one entry for the whole move
    expect(store.getState().nodes[0]!.position).toEqual({ x: 80, y: 80 });
    undo();
    expect(store.getState().nodes[0]!.position).toEqual({ x: 0, y: 0 });
    redo();
    expect(store.getState().nodes[0]!.position).toEqual({ x: 80, y: 80 });
  });
});
