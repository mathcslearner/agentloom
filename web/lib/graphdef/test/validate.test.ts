// Unit tables for the full validator (17.5) over hand-built definitions,
// covering the graph-semantic and templating rules the corpus exercises only
// thinly, plus the client-only advisory warnings and the `related` highlight
// paths the Problems panel uses.
import { describe, expect, it } from "vitest";
import { validateDefinition, isValidDefinition, type Issue } from "../src/index.js";

function def(over: Record<string, unknown>): Record<string, unknown> {
  return { schema_version: 1, name: "t", steps: [], edges: [], ...over };
}
const noop = (id: string) => ({ id, type: "noop" });
function codes(issues: Issue[]): string[] {
  return issues.map((i) => `${i.code}@${i.path}`);
}

describe("graph-semantic validation", () => {
  it("accepts a marked loop edge; rejects the same cycle unmarked", () => {
    const steps = [noop("a"), noop("b"), noop("c")];
    const normalEdges = [
      { from: "a", to: "b" },
      { from: "b", to: "c" },
    ];
    const loopOK = def({ steps, edges: [...normalEdges, { from: "c", to: "b", type: "loop", condition: "output.again == true", max_iterations: 3 }] });
    expect(isValidDefinition(loopOK)).toBe(true);

    const unmarked = def({ steps, edges: [...normalEdges, { from: "c", to: "b" }] });
    const issues = validateDefinition(unmarked);
    const cycle = issues.find((i) => i.code === "cycle_detected");
    expect(cycle).toBeDefined();
    expect(cycle!.path).toBe("edges[2]");
    // The `related` highlight names every node on the cycle plus the edge.
    expect(cycle!.related).toContain("edges[2]");
    expect(cycle!.related).toEqual(expect.arrayContaining(["steps[1]", "steps[2]"]));
  });

  it("rejects a loop edge whose target is not an ancestor", () => {
    const steps = [noop("a"), noop("b"), noop("c")];
    const d = def({
      steps,
      edges: [
        { from: "a", to: "b" },
        { from: "b", to: "c" },
        { from: "a", to: "c", type: "loop", condition: "output.x == true", max_iterations: 2 },
      ],
    });
    expect(codes(validateDefinition(d))).toContain("loop_edge_not_ancestor@edges[2]");
  });

  it("reports a dangling edge endpoint and suppresses graph-semantic checks", () => {
    const d = def({ steps: [noop("a")], edges: [{ from: "a", to: "ghost" }] });
    const c = codes(validateDefinition(d));
    expect(c).toContain("unknown_edge_endpoint@edges[0].to");
  });

  it("warns on an isolated step (a warning, not an error)", () => {
    const d = def({ steps: [noop("a"), noop("b"), noop("c")], edges: [{ from: "a", to: "b" }] });
    const issues = validateDefinition(d);
    const iso = issues.find((i) => i.code === "isolated_step");
    expect(iso?.severity).toBe("warning");
    expect(isValidDefinition(d)).toBe(true); // a warning does not block
  });

  it("rejects a definition with no entry step", () => {
    const d = def({
      steps: [noop("a"), noop("b")],
      edges: [
        { from: "a", to: "b" },
        { from: "b", to: "a" },
      ],
    });
    expect(codes(validateDefinition(d))).toContain("no_entry_step@");
  });
});

describe("templating ref lint", () => {
  const llm = (id: string, prompt: string) => ({ id, type: "llm", config: { model: "mock/x", prompt } });

  it("reports unknown / self / not-upstream / undeclared-param refs at the config path", () => {
    const d = def({
      steps: [
        noop("src"),
        llm("mid", "${{ steps.ghost.output }} ${{ steps.mid.output }} ${{ steps.sink.output }} ${{ run.params.absent }}"),
        noop("sink"),
      ],
      edges: [
        { from: "src", to: "mid" },
        { from: "mid", to: "sink" },
      ],
    });
    const c = codes(validateDefinition(d));
    expect(c).toContain("template_ref_unknown_step@steps[1].config.prompt");
    expect(c).toContain("template_ref_not_upstream@steps[1].config.prompt"); // self + downstream both
    expect(c).toContain("template_ref_unknown_param@steps[1].config.prompt");
  });

  it("accepts an upstream ref and a declared param", () => {
    const d = def({
      params: { topic: { type: "string" } },
      steps: [llm("a", "start"), llm("b", "${{ steps.a.output.text }} on ${{ run.params.topic }}")],
      edges: [{ from: "a", to: "b" }],
    });
    expect(isValidDefinition(d)).toBe(true);
  });

  it("flags item/item_index outside a map body", () => {
    const d = def({ steps: [llm("a", "${{ item }}")], edges: [] });
    expect(codes(validateDefinition(d))).toContain("template_ref_invalid@steps[0].config.prompt");
  });
});

describe("advisory budget warnings (client-only, never block)", () => {
  it("warns when a run budget is set but no step is cost-bearing", () => {
    const d = def({ budget_usd: 1, steps: [noop("a")], edges: [] });
    const issues = validateDefinition(d);
    const adv = issues.find((i) => i.code === "advisory_budget");
    expect(adv?.severity).toBe("warning");
    expect(isValidDefinition(d)).toBe(true);
  });

  it("warns when a step cap is at/above the run budget", () => {
    const d = def({
      budget_usd: 1,
      steps: [{ id: "a", type: "llm", config: { model: "mock/x", prompt: "hi" }, budget: { max_usd: 2 } }],
      edges: [],
    });
    const adv = validateDefinition(d).filter((i) => i.code === "advisory_budget");
    expect(adv.some((i) => i.path === "steps[0].budget.max_usd")).toBe(true);
  });

  it("does not warn when the budget can bind", () => {
    const d = def({
      budget_usd: 5,
      steps: [{ id: "a", type: "llm", config: { model: "mock/x", prompt: "hi" }, budget: { max_usd: 1 } }],
      edges: [],
    });
    expect(validateDefinition(d).filter((i) => i.code === "advisory_budget")).toEqual([]);
  });
});
