import { describe, expect, it } from "vitest";
import { projectGraph } from "@/lib/pure/dashboard/projection";
import { topologyFromGraph } from "@/lib/pure/dashboard/graph-topology";
import type { LayoutState } from "@/lib/pure/dashboard/layout";
import type { StepState } from "@/lib/pure/dashboard/run-state";
import type { GraphNodeView, RunGraphResponse } from "@agentloom/api-client";

function step(id: string, status: StepState["status"], type = "llm"): StepState {
  return { id, type, status, attempt: 1, reclaims: 0 };
}

function graph(nodes: GraphNodeView[], edges: RunGraphResponse["edges"]): RunGraphResponse {
  return { run_id: "r", graph_version: 2, steps_total: nodes.length, event_seq: 1, nodes, edges, expansions: [] };
}

function layout(entries: Array<[string, number, number]>): LayoutState {
  return { positions: new Map(entries.map(([id, x, y]) => [id, { x, y }])) };
}

describe("projectGraph", () => {
  it("projects step nodes with skins, positions, and provenance", () => {
    const topo = topologyFromGraph(
      graph(
        [
          { id: "a", type: "planner", status: "succeeded", depth: 0, graph_version: 1, origin: { kind: "definition" }, added_at: "" },
          { id: "w#1", type: "llm", status: "running", depth: 1, graph_version: 2, origin: { kind: "planner", step: "a" }, added_at: "" },
        ],
        [{ from: "a", to: "w#1", type: "normal", resolution: "fired", graph_version: 2, origin: { kind: "planner", step: "a" } }],
      ),
    );
    const steps = new Map([["a", step("a", "succeeded", "planner")], ["w#1", step("w#1", "running")]]);
    const proj = projectGraph({ topology: topo, steps, layout: layout([["a", 0, 0], ["w#1", 300, 0]]), collapsed: new Set() });
    const stepNodes = proj.nodes.filter((n) => n.kind === "runStep");
    expect(stepNodes).toHaveLength(2);
    const w = stepNodes.find((n) => n.id === "w#1")!;
    expect(w.position).toEqual({ x: 300, y: 0 });
    // one active edge (a succeeded → w#1 running)
    expect(proj.edges).toHaveLength(1);
    expect(proj.edges[0]!.data.visual.active).toBe(true);
    expect(proj.edges[0]!.sourceHandle).toBe("out");
  });

  it("renders a decision edge from the reject port", () => {
    const topo = topologyFromGraph(
      graph(
        [
          { id: "gate", type: "human_approval", status: "awaiting_human", depth: 0, graph_version: 1, origin: { kind: "definition" }, added_at: "" },
          { id: "x", type: "echo", status: "pending", depth: 0, graph_version: 1, origin: { kind: "definition" }, added_at: "" },
        ],
        [{ from: "gate", to: "x", type: "normal", decision: "reject", resolution: "unresolved", graph_version: 1, origin: { kind: "definition" } }],
      ),
    );
    const steps = new Map([["gate", step("gate", "awaiting_human", "human_approval")], ["x", step("x", "pending", "echo")]]);
    const proj = projectGraph({ topology: topo, steps, layout: layout([["gate", 0, 0], ["x", 300, 0]]), collapsed: new Set() });
    expect(proj.edges[0]!.sourceHandle).toBe("reject");
  });

  it("collapsing a loop group hides members + internal edges and keeps a container", () => {
    const topo = topologyFromGraph(
      graph(
        [
          { id: "src", type: "llm", status: "succeeded", depth: 0, graph_version: 1, origin: { kind: "definition" }, added_at: "" },
          { id: "draft#1", type: "llm", status: "succeeded", depth: 1, graph_version: 2, origin: { kind: "loop", step: "src" }, added_at: "" },
          { id: "crit#1", type: "llm", status: "succeeded", depth: 1, graph_version: 2, origin: { kind: "loop", step: "src" }, added_at: "" },
          { id: "after", type: "echo", status: "pending", depth: 0, graph_version: 1, origin: { kind: "definition" }, added_at: "" },
        ],
        [
          { from: "draft#1", to: "crit#1", type: "normal", resolution: "fired", graph_version: 2, origin: { kind: "loop", step: "src" } },
          { from: "crit#1", to: "after", type: "normal", resolution: "unresolved", graph_version: 2, origin: { kind: "loop", step: "src" } },
        ],
      ),
    );
    const steps = new Map([
      ["src", step("src", "succeeded")],
      ["draft#1", step("draft#1", "succeeded")],
      ["crit#1", step("crit#1", "succeeded")],
      ["after", step("after", "pending", "echo")],
    ]);
    const lay = layout([["src", 0, 0], ["draft#1", 300, 0], ["crit#1", 560, 0], ["after", 900, 0]]);
    const groupId = "loop:src:1";

    // Expanded: all members visible + a container.
    const expanded = projectGraph({ topology: topo, steps, layout: lay, collapsed: new Set() });
    expect(expanded.nodes.some((n) => n.kind === "runGroup" && n.id === groupId)).toBe(true);
    expect(expanded.nodes.filter((n) => n.kind === "runStep" && n.hidden)).toHaveLength(0);

    // Collapsed: members hidden, internal edge dropped, boundary edge re-targeted.
    const collapsed = projectGraph({ topology: topo, steps, layout: lay, collapsed: new Set([groupId]) });
    const hidden = collapsed.nodes.filter((n) => n.kind === "runStep" && (n.id === "draft#1" || n.id === "crit#1"));
    expect(hidden.every((n) => n.hidden)).toBe(true);
    // The internal draft#1 → crit#1 edge is gone.
    expect(collapsed.edges.some((e) => e.source === "draft#1" && e.target === "crit#1")).toBe(false);
    // The crit#1 → after boundary edge is re-targeted onto the group node.
    expect(collapsed.edges.some((e) => e.source === groupId && e.target === "after")).toBe(true);
  });

  it("marks a recently-injected node as entered", () => {
    const topo = topologyFromGraph(
      graph([{ id: "w#1", type: "llm", status: "ready", depth: 1, graph_version: 2, origin: { kind: "planner", step: "a" }, added_at: "" }], []),
    );
    // topologyFromGraph does not set addedSeq; simulate a live-added node.
    topo.nodes.get("w#1")!.addedSeq = 7;
    const steps = new Map([["w#1", step("w#1", "ready")]]);
    const proj = projectGraph({ topology: topo, steps, layout: layout([["w#1", 0, 0]]), collapsed: new Set(), recent: new Set([7]) });
    const node = proj.nodes.find((n) => n.id === "w#1")!;
    expect((node.data as { entered: boolean }).entered).toBe(true);
  });
});
