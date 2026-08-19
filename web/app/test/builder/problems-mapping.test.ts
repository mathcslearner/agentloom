import { describe, expect, it } from "vitest";
import type { Issue } from "@agentloom/graphdef";
import {
  edgeIndexOfPath,
  issueTargets,
  mapIssuesToEdges,
  mapIssuesToNodes,
  stepIndexOfPath,
} from "@/lib/builder/problems";

const err = (path: string, related?: string[]): Issue => ({ code: "x", severity: "error", path, msg: "", ...(related ? { related } : {}) });

describe("path index extraction", () => {
  it("reads step and edge indices, null otherwise", () => {
    expect(stepIndexOfPath("steps[3].config.model")).toBe(3);
    expect(stepIndexOfPath("edges[1].to")).toBeNull();
    expect(edgeIndexOfPath("edges[2]")).toBe(2);
    expect(edgeIndexOfPath("steps[0].id")).toBeNull();
    expect(edgeIndexOfPath("budget_usd")).toBeNull();
  });
});

describe("mapIssuesToEdges", () => {
  it("attributes edges[i] issues to the edge id, never a steps[i] issue", () => {
    const issues = [err("edges[0].condition"), err("steps[1].config.model"), err("edges[2]")];
    const byEdge = mapIssuesToEdges(issues, ["e0", "e1", "e2"]);
    expect(byEdge.get("e0")?.length).toBe(1);
    expect(byEdge.has("e1")).toBe(false); // steps[1] is a node's, not edge index 1
    expect(byEdge.get("e2")?.length).toBe(1);
  });
});

describe("mapIssuesToNodes still ignores edge paths", () => {
  it("drops edges[i] paths", () => {
    const byNode = mapIssuesToNodes([err("edges[0].to"), err("steps[0].id")], ["a"]);
    expect(byNode.get("a")?.length).toBe(1);
  });
});

describe("issueTargets", () => {
  const nodes = ["a", "b", "c"];
  const edges = ["e0", "e1"];
  it("resolves the primary path", () => {
    expect(issueTargets(err("steps[1].config.model"), nodes, edges)).toEqual({ nodes: ["b"], edges: [] });
    expect(issueTargets(err("edges[0].condition"), nodes, edges)).toEqual({ nodes: [], edges: ["e0"] });
  });
  it("resolves related paths (a cycle's nodes + edge)", () => {
    const cycle = err("edges[1]", ["steps[1]", "steps[2]", "edges[1]"]);
    const t = issueTargets(cycle, nodes, edges);
    expect(t.nodes.sort()).toEqual(["b", "c"]);
    expect(t.edges).toEqual(["e1"]);
  });
  it("ignores out-of-range indices", () => {
    expect(issueTargets(err("steps[9].id"), nodes, edges)).toEqual({ nodes: [], edges: [] });
    expect(issueTargets(err("budget_usd"), nodes, edges)).toEqual({ nodes: [], edges: [] });
  });
});
