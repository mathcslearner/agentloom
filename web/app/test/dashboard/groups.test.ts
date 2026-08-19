import { describe, expect, it } from "vitest";
import { computeGroups, groupIdFor, groupStatus, parseInstance } from "@/lib/pure/dashboard/groups";
import { topologyFromGraph } from "@/lib/pure/dashboard/graph-topology";
import type { GraphNodeView, RunGraphResponse } from "@agentloom/api-client";
import type { StepState } from "@/lib/pure/dashboard/run-state";

function node(id: string, kind: GraphNodeView["origin"]["kind"], step?: string, gv = 2): GraphNodeView {
  return { id, type: "llm", status: "pending", depth: kind === "definition" ? 0 : 1, graph_version: gv, origin: { kind, step }, added_at: "" };
}

function topo(nodes: GraphNodeView[]) {
  const g: RunGraphResponse = { run_id: "r", graph_version: 2, steps_total: nodes.length, event_seq: 1, nodes, edges: [], expansions: [] };
  return topologyFromGraph(g);
}

describe("parseInstance", () => {
  it("splits base and suffix segments", () => {
    expect(parseInstance("draft#1")).toEqual({ base: "draft", suffixes: ["1"] });
    expect(parseInstance("draft")).toEqual({ base: "draft", suffixes: [] });
  });
});

describe("groupIdFor", () => {
  it("groups a loop instance by (origin, iteration)", () => {
    expect(groupIdFor({ id: "draft#2", type: "llm", origin: { kind: "loop", step: "loop_src" }, depth: 1, graphVersion: 3 })).toEqual({
      id: "loop:loop_src:2",
      kind: "loop",
      iteration: "2",
    });
  });
  it("groups a map instance by origin", () => {
    expect(groupIdFor({ id: "analyze#3", type: "llm", origin: { kind: "map", step: "m" }, depth: 1, graphVersion: 2 })).toEqual({
      id: "map:m",
      kind: "map",
    });
  });
  it("does not group authored or planner nodes", () => {
    expect(groupIdFor({ id: "a", type: "llm", origin: { kind: "definition" }, depth: 0, graphVersion: 1 })).toBeNull();
    expect(groupIdFor({ id: "w#1", type: "llm", origin: { kind: "planner", step: "p" }, depth: 1, graphVersion: 2 })).toBeNull();
  });
});

describe("computeGroups", () => {
  it("makes one group per loop iteration and one per map", () => {
    const t = topo([
      node("a", "definition"),
      node("draft#1", "loop", "loop_src"),
      node("critique#1", "loop", "loop_src"),
      node("draft#2", "loop", "loop_src"),
      node("item#1", "map", "m"),
      node("item#2", "map", "m"),
    ]);
    const groups = computeGroups(t);
    const ids = groups.map((g) => g.id).sort();
    expect(ids).toEqual(["loop:loop_src:1", "loop:loop_src:2", "map:m"]);
    const iter1 = groups.find((g) => g.id === "loop:loop_src:1")!;
    expect(iter1.members.sort()).toEqual(["critique#1", "draft#1"]);
    expect(iter1.label).toBe("iteration 1");
    const map = groups.find((g) => g.id === "map:m")!;
    expect(map.label).toBe("2 instances");
  });
});

describe("groupStatus", () => {
  it("surfaces the most severe / active member status", () => {
    const steps = new Map<string, StepState>([
      ["x", { id: "x", type: "llm", status: "succeeded", attempt: 1, reclaims: 0 }],
      ["y", { id: "y", type: "llm", status: "running", attempt: 1, reclaims: 0 }],
    ]);
    expect(groupStatus(["x", "y"], steps)).toBe("running");
    steps.set("y", { id: "y", type: "llm", status: "dead_lettered", attempt: 1, reclaims: 0 });
    expect(groupStatus(["x", "y"], steps)).toBe("dead_lettered");
  });
});
