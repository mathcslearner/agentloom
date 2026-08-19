import { describe, expect, it } from "vitest";
import {
  columnLayouter,
  computeLayout,
  emptyLayout,
  NODE_H,
  NODE_W,
  planPlacement,
  resolveCollisions,
  type Layouter,
  type Point,
} from "@/lib/pure/dashboard/layout";
import { applyGraphEvent, topologyFromGraph, type GraphTopology } from "@/lib/pure/dashboard/graph-topology";
import type { RunGraphResponse } from "@agentloom/api-client";
import { makeEnv } from "./helpers";

// A deterministic fake layouter: a left→right row (grid step ~ node width), so
// the tests assert stable placement without depending on elkjs internals.
const rowLayouter: Layouter = async (nodes) => {
  const out = new Map<string, Point>();
  nodes.forEach((n, i) => out.set(n.id, { x: i * (NODE_W + 40), y: 0 }));
  return out;
};

function graphWithHints(): RunGraphResponse {
  return {
    run_id: "r",
    graph_version: 1,
    steps_total: 2,
    event_seq: 1,
    nodes: [
      { id: "a", type: "planner", status: "running", depth: 0, graph_version: 1, origin: { kind: "definition" }, added_at: "", position: { x: 100, y: 200 } },
      { id: "b", type: "join", status: "pending", depth: 0, graph_version: 1, origin: { kind: "definition" }, added_at: "", position: { x: 400, y: 200 } },
    ],
    edges: [{ from: "a", to: "b", type: "normal", resolution: "unresolved", graph_version: 1, origin: { kind: "definition" } }],
    expansions: [],
  };
}

describe("planPlacement", () => {
  it("honors ui hints verbatim (they become fixed, no block)", () => {
    const topo = topologyFromGraph(graphWithHints());
    const { fixed, blocks } = planPlacement(topo, emptyLayout());
    expect(fixed.get("a")).toEqual({ x: 100, y: 200 });
    expect(fixed.get("b")).toEqual({ x: 400, y: 200 });
    expect(blocks).toHaveLength(0);
  });

  it("groups unhinted nodes into a base block and expansions into their own blocks", () => {
    const bare: RunGraphResponse = {
      ...graphWithHints(),
      nodes: graphWithHints().nodes.map((n) => ({ ...n, position: undefined })),
    };
    let topo = topologyFromGraph(bare);
    topo = applyGraphEvent(topo, expandEnv(5));
    const { blocks } = planPlacement(topo, emptyLayout());
    const keys = blocks.map((b) => b.key).sort();
    expect(keys).toEqual(["5", "base"]);
    expect(blocks.find((b) => b.key === "5")?.origin).toBe("a");
  });
});

function expandEnv(seq: number) {
  return makeEnv("graph_expanded", seq, {
    origin_step: "a",
    origin_kind: "planner",
    from_version: 1,
    to_version: 2,
    depth: 1,
    delta: { schema_version: 1, steps: [{ id: "w#1", type: "llm", config: {} }], edges: [{ from: "a", to: "w#1" }] },
    readied: ["w#1"],
  });
}

describe("computeLayout — sticky incremental", () => {
  it("keeps already-placed nodes fixed across an expansion (no reshuffle)", async () => {
    const bare: RunGraphResponse = {
      ...graphWithHints(),
      nodes: graphWithHints().nodes.map((n) => ({ ...n, position: undefined })),
    };
    let topo: GraphTopology = topologyFromGraph(bare);
    const first = await computeLayout(topo, emptyLayout(), rowLayouter);
    const posA = first.positions.get("a")!;
    const posB = first.positions.get("b")!;
    expect(posA).toBeDefined();

    // An expansion injects w#1; re-layout must not move a or b.
    topo = applyGraphEvent(topo, expandEnv(5));
    const second = await computeLayout(topo, first, rowLayouter);
    expect(second.positions.get("a")).toEqual(posA);
    expect(second.positions.get("b")).toEqual(posB);
    // The new node is placed to the right of its origin (a).
    const w = second.positions.get("w#1")!;
    expect(w.x).toBeGreaterThan(posA.x);
  });

  it("anchors an injected block to the right of its origin", async () => {
    // Only `a` is present (hinted at (100,200)) so nothing collides — the block
    // lands exactly at the anchor. (Collision push is covered separately.)
    const g = graphWithHints();
    const single: RunGraphResponse = { ...g, nodes: [g.nodes[0]!], edges: [] };
    const topo1 = applyGraphEvent(topologyFromGraph(single), expandEnv(5));
    const laid = await computeLayout(topo1, emptyLayout(), rowLayouter);
    const w = laid.positions.get("w#1")!;
    expect(w.x).toBe(100 + NODE_W + 80); // origin.x + node width + gap
    expect(w.y).toBe(200);
  });
});

describe("resolveCollisions", () => {
  it("pushes a colliding block straight down until clear", () => {
    const placed = new Map<string, Point>([["p", { x: 0, y: 0 }]]);
    const block = new Map<string, Point>([["q", { x: 10, y: 0 }]]); // overlaps p
    const out = resolveCollisions(placed, block);
    expect(out.get("q")!.y).toBeGreaterThan(0);
    expect(out.get("q")!.x).toBe(10); // x unchanged (horizontal flow intact)
  });

  it("leaves a non-colliding block untouched (same reference)", () => {
    const placed = new Map<string, Point>([["p", { x: 0, y: 0 }]]);
    const block = new Map<string, Point>([["q", { x: 1000, y: 0 }]]);
    expect(resolveCollisions(placed, block)).toBe(block);
  });
});

describe("columnLayouter", () => {
  it("stacks nodes in a column", async () => {
    const out = await columnLayouter([{ id: "a", width: NODE_W, height: NODE_H }, { id: "b", width: NODE_W, height: NODE_H }], []);
    expect(out.get("a")!.y).toBe(0);
    expect(out.get("b")!.y).toBeGreaterThan(0);
  });
});
