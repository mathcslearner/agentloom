// Autocomplete refs (ticket 17.4, DoD-3): upstream-only suggestions. The
// upstream predicate must be exactly the backend's ancestry over normal edges.
import { describe, expect, it } from "vitest";
import {
  activeExpression,
  outputPaths,
  paramNames,
  suggestionsFor,
  upstreamStepIds,
  type RefEdge,
  type RefNode,
} from "@/lib/pure/builder/refs";

// A branching graph: a → b, a → c, b → d, c → e, and a loop edge e → b.
//        a
//       / \
//      b   c
//      |   |
//      d   e --(loop)--> b
const nodes: RefNode[] = [
  { id: "a", type: "llm" },
  { id: "b", type: "llm" },
  { id: "c", type: "retrieve" },
  { id: "d", type: "tool" },
  { id: "e", type: "llm" },
];
const edges: RefEdge[] = [
  { from: "a", to: "b", loop: false },
  { from: "a", to: "c", loop: false },
  { from: "b", to: "d", loop: false },
  { from: "c", to: "e", loop: false },
  { from: "e", to: "b", loop: true }, // loop edge — confers no ancestry
];

describe("upstreamStepIds", () => {
  it("returns exactly the normal-edge ancestors", () => {
    expect(upstreamStepIds(nodes, edges, "d")).toEqual(["a", "b"]);
    expect(upstreamStepIds(nodes, edges, "e")).toEqual(["a", "c"]);
    expect(upstreamStepIds(nodes, edges, "b")).toEqual(["a"]); // loop e→b excluded
    expect(upstreamStepIds(nodes, edges, "a")).toEqual([]);
  });

  it("never includes the step itself", () => {
    for (const n of nodes) {
      expect(upstreamStepIds(nodes, edges, n.id)).not.toContain(n.id);
    }
  });
});

describe("suggestionsFor", () => {
  it("offers only upstream steps after `steps.`", () => {
    const s = suggestionsFor({ fragment: " steps.", nodes, edges, doc: {}, currentStepId: "d" });
    expect(s.map((x) => x.label).sort()).toEqual(["a", "b"]);
    expect(s.every((x) => x.value.startsWith("steps.") && x.value.endsWith(".output"))).toBe(true);
  });

  it("offers a step's output paths after `steps.<id>.output.`", () => {
    const s = suggestionsFor({ fragment: "steps.a.output.", nodes, edges, doc: {}, currentStepId: "d" });
    expect(s.map((x) => x.label)).toEqual(outputPaths("llm"));
  });

  it("offers declared run params after `run.params.`", () => {
    const doc = { params: { topic: { type: "string" }, count: { type: "number" } } };
    const s = suggestionsFor({ fragment: "run.params.", nodes, edges, doc, currentStepId: "d" });
    expect(s.map((x) => x.label).sort()).toEqual(["count", "topic"]);
    expect(paramNames(doc)).toEqual(["count", "topic"]);
  });

  it("filters upstream steps by the typed prefix", () => {
    const s = suggestionsFor({ fragment: "steps.b", nodes, edges, doc: {}, currentStepId: "d" });
    expect(s.map((x) => x.label)).toEqual(["b"]);
  });
});

describe("activeExpression", () => {
  it("finds the fragment inside an open ${{ }}", () => {
    const text = "hello ${{ steps.a";
    expect(activeExpression(text, text.length)?.fragment).toBe(" steps.a");
  });
  it("returns null past a closed expression", () => {
    const text = "${{ steps.a.output }} tail";
    expect(activeExpression(text, text.length)).toBeNull();
  });
});
