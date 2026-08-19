import { describe, expect, it } from "vitest";
import type { RunGraphResponse, RunResponse } from "@agentloom/api-client";
import {
  applyGraphEvent,
  edgeKey,
  emptyTopology,
  mergeTopology,
  topologyFromGraph,
  topologyFromSnapshot,
} from "@/lib/pure/dashboard/graph-topology";
import { makeEnv, makeRun, makeStep } from "./helpers";

function snapshot(): RunResponse {
  return {
    run: makeRun({ event_seq: 3 }),
    steps: [makeStep({ id: "a", type: "planner" }), makeStep({ id: "b", type: "join" })],
    edges: [{ from: "a", to: "b", type: "normal", resolution: "unresolved" }],
  };
}

function graph(): RunGraphResponse {
  return {
    run_id: "run-1",
    graph_version: 2,
    steps_total: 3,
    event_seq: 9,
    nodes: [
      { id: "a", type: "planner", status: "succeeded", depth: 0, graph_version: 1, origin: { kind: "definition" }, added_at: "", position: { x: 10, y: 20 } },
      { id: "b", type: "join", status: "succeeded", depth: 0, graph_version: 1, origin: { kind: "definition" }, added_at: "" },
      { id: "w#1", type: "llm", status: "succeeded", depth: 1, graph_version: 2, origin: { kind: "planner", step: "a" }, added_at: "" },
    ],
    edges: [
      { from: "a", to: "b", type: "normal", resolution: "fired", graph_version: 1, origin: { kind: "definition" } },
      { from: "a", to: "w#1", type: "normal", resolution: "fired", graph_version: 2, origin: { kind: "planner", step: "a" } },
      { from: "gate", to: "x", type: "normal", decision: "reject", resolution: "unresolved", graph_version: 1, origin: { kind: "definition" } },
    ],
    expansions: [],
  };
}

describe("topologyFromSnapshot", () => {
  it("seeds authored nodes + edges with the run's as-of cursor", () => {
    const t = topologyFromSnapshot(snapshot());
    expect(t.nodes.size).toBe(2);
    expect(t.nodes.get("a")?.origin.kind).toBe("definition");
    expect(t.edges.get(edgeKey("a", "b", "normal"))?.resolution).toBe("unresolved");
    expect(t.asOf).toBe(3);
  });
});

describe("topologyFromGraph", () => {
  it("carries provenance, positions, and the decision marker", () => {
    const t = topologyFromGraph(graph());
    expect(t.nodes.get("w#1")?.origin).toEqual({ kind: "planner", step: "a" });
    expect(t.nodes.get("a")?.position).toEqual({ x: 10, y: 20 });
    expect(t.edges.get(edgeKey("gate", "x", "normal"))?.decision).toBe("reject");
    expect(t.graphVersion).toBe(2);
    expect(t.asOf).toBe(9);
  });
});

describe("mergeTopology", () => {
  it("is order-independent: snapshot-before-graph == graph-before-snapshot", () => {
    const snap = topologyFromSnapshot(snapshot());
    const gr = topologyFromGraph(graph());
    const a = mergeTopology(snap, gr);
    const b = mergeTopology(gr, snap);
    expect([...a.nodes.keys()].sort()).toEqual([...b.nodes.keys()].sort());
    expect([...a.edges.keys()].sort()).toEqual([...b.edges.keys()].sort());
    // richer values win either way: provenance + position survive.
    expect(a.nodes.get("a")?.position).toEqual({ x: 10, y: 20 });
    expect(b.nodes.get("a")?.position).toEqual({ x: 10, y: 20 });
    expect(a.nodes.get("w#1")?.origin.kind).toBe("planner");
    expect(b.nodes.get("w#1")?.origin.kind).toBe("planner");
  });

  it("unions the injected node the graph read carries into the snapshot", () => {
    const t = mergeTopology(topologyFromSnapshot(snapshot()), topologyFromGraph(graph()));
    expect(t.nodes.has("w#1")).toBe(true);
    expect(t.graphVersion).toBe(2);
  });
});

describe("applyGraphEvent", () => {
  const base = topologyFromSnapshot(snapshot());

  it("adds delta steps/edges with provenance and is idempotent by seq", () => {
    const env = makeEnv("graph_expanded", 5, {
      origin_step: "a",
      origin_kind: "planner",
      from_version: 1,
      to_version: 2,
      depth: 1,
      delta: {
        schema_version: 1,
        steps: [{ id: "w#1", type: "llm", config: {} }],
        edges: [
          { from: "a", to: "w#1" },
          { from: "w#1", to: "b" },
        ],
      },
      readied: ["w#1"],
      widened: ["b"],
    });
    const t = applyGraphEvent(base, env);
    expect(t.nodes.get("w#1")?.origin).toEqual({ kind: "planner", step: "a" });
    expect(t.nodes.get("w#1")?.addedSeq).toBe(5);
    expect(t.edges.has(edgeKey("w#1", "b", "normal"))).toBe(true);
    expect(t.graphVersion).toBe(2);
    // Re-applying the same event (a reconnect suffix) is a no-op by identity.
    expect(applyGraphEvent(t, env)).toBe(t);
  });

  it("ignores an event at or below the as-of cursor", () => {
    const seeded = { ...base, asOf: 5 };
    const env = makeEnv("graph_expanded", 5, {
      origin_step: "a",
      origin_kind: "planner",
      from_version: 1,
      to_version: 2,
      depth: 1,
      delta: { schema_version: 1, steps: [{ id: "late", type: "llm", config: {} }], edges: [] },
    });
    expect(applyGraphEvent(seeded, env)).toBe(seeded);
  });

  it("ignores non-graph events", () => {
    expect(applyGraphEvent(base, makeEnv("step_succeeded", 4, {}, "a"))).toBe(base);
  });

  it("emptyTopology is a valid neutral element", () => {
    expect(mergeTopology(emptyTopology(), base).nodes.size).toBe(base.nodes.size);
  });
});
