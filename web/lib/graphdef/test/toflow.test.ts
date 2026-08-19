// Mapping specifics: position lifting, default grid, edge ids, dangling edges,
// residual ui, and shape errors with backend-style paths.
import { describe, expect, it } from "vitest";
import { assignEdgeIds, edgeId, GraphdefError, toDefinition, toFlow } from "../src/index.js";

const base = (over: Record<string, unknown> = {}) => ({
  schema_version: 1,
  name: "t",
  steps: [
    { id: "a", type: "noop" },
    { id: "b", type: "noop" },
  ],
  edges: [{ from: "a", to: "b" }],
  ...over,
});

describe("position lifting and synthesis", () => {
  it("lifts a valid ui position and marks the node positioned", () => {
    const flow = toFlow(base({ ui: { nodes: { a: { position: { x: 7, y: 9 } } } } }));
    const a = flow.nodes.find((n) => n.id === "a")!;
    expect(a.data.positioned).toBe(true);
    expect(a.position).toEqual({ x: 7, y: 9 });
    // Entry existed but had only `position` → the lifted rest is an empty object
    // (present, not undefined), which reconstructs the entry losslessly.
    expect(a.data.ui).toEqual({});
  });

  it("synthesizes a deterministic grid position when none is given", () => {
    const flow = toFlow(base());
    expect(flow.nodes[0]!.data.positioned).toBe(false);
    expect(flow.nodes[1]!.data.positioned).toBe(false);
    // Two runs agree.
    expect(toFlow(base()).nodes[1]!.position).toEqual(flow.nodes[1]!.position);
    // A synthesized position never round-trips into the document.
    const back = toDefinition(flow) as unknown as Record<string, unknown>;
    expect(back["ui"]).toBeUndefined();
  });

  it("honors a custom defaultPosition", () => {
    const flow = toFlow(base(), { defaultPosition: (_s, i) => ({ x: i * 1000, y: 0 }) });
    expect(flow.nodes[1]!.position).toEqual({ x: 1000, y: 0 });
  });

  it("keeps an entry with no valid position as data.ui, position synthesized", () => {
    const flow = toFlow(base({ ui: { nodes: { a: { note: "hi" } } } }));
    const a = flow.nodes.find((n) => n.id === "a")!;
    expect(a.data.positioned).toBe(false);
    expect(a.data.ui).toEqual({ note: "hi" });
    expect(toDefinition(flow)).toEqual(base({ ui: { nodes: { a: { note: "hi" } } } }));
  });
});

describe("residual ui and orphans", () => {
  it("preserves orphan ui.nodes entries and non-node ui keys", () => {
    const def = base({
      ui: { nodes: { a: { position: { x: 1, y: 2 } }, ghost: { position: { x: 9, y: 9 } } }, zoom: 0.5 },
    });
    const flow = toFlow(def);
    expect(flow.ui["zoom"]).toBe(0.5);
    expect((flow.ui["nodes"] as Record<string, unknown>)["ghost"]).toEqual({ position: { x: 9, y: 9 } });
    expect(toDefinition(flow)).toEqual(def);
  });

  it("leaves a non-object ui.nodes untouched (no lifting)", () => {
    const def = base({ ui: { nodes: "not-a-map", zoom: 1 } });
    const flow = toFlow(def);
    expect(flow.nodes.every((n) => !n.data.positioned)).toBe(true);
    expect(toDefinition(flow)).toEqual(def);
  });

  it("preserves a deliberately-empty ui block", () => {
    const def = base({ ui: {} });
    expect(toDefinition(toFlow(def))).toEqual(def);
  });
});

describe("edge identity", () => {
  it("assigns stable ids, disambiguating duplicate pairs", () => {
    expect(edgeId("a", "b", 1)).toBe("a->b");
    expect(edgeId("a", "b", 2)).toBe("a->b#2");
    expect(assignEdgeIds([{ from: "a", to: "b" }, { from: "a", to: "b" }, { from: "a", to: "c" }])).toEqual([
      "a->b",
      "a->b#2",
      "a->c",
    ]);
  });

  it("carries the full edge object and preserves a dangling endpoint", () => {
    const def = base({ edges: [{ from: "a", to: "ghost", when: "true" }] });
    const flow = toFlow(def);
    expect(flow.edges[0]!.data.edge).toEqual({ from: "a", to: "ghost", when: "true" });
    expect(toDefinition(flow)).toEqual(def); // dangling edge survives; validation is 17.5
  });
});

describe("shape errors carry a path", () => {
  const cases: [string, unknown, string, string][] = [
    ["not an object", 42, "not_object", ""],
    ["steps not an array", { steps: {}, edges: [] }, "steps_not_array", "steps"],
    ["edges not an array", { steps: [], edges: {} }, "edges_not_array", "edges"],
    ["step not object", { steps: [1], edges: [] }, "step_invalid", "steps[0]"],
    ["step id not string", { steps: [{ id: 1, type: "noop" }], edges: [] }, "step_invalid", "steps[0].id"],
    ["step type not string", { steps: [{ id: "a" }], edges: [] }, "step_invalid", "steps[0].type"],
    [
      "duplicate id",
      { steps: [{ id: "a", type: "noop" }, { id: "a", type: "noop" }], edges: [] },
      "duplicate_step_id",
      "steps[1].id",
    ],
    ["ui not object", { steps: [], edges: [], ui: [] }, "ui_not_object", "ui"],
    ["edge from missing", { steps: [], edges: [{ to: "b" }] }, "edge_invalid", "edges[0].from"],
  ];
  for (const [label, input, code, path] of cases) {
    it(label, () => {
      try {
        toFlow(input);
        expect.unreachable("should throw");
      } catch (err) {
        expect(err).toBeInstanceOf(GraphdefError);
        expect((err as GraphdefError).code).toBe(code);
        expect((err as GraphdefError).path).toBe(path);
      }
    });
  }
});
