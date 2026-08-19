import { describe, expect, it } from "vitest";
import { RunController, type GraphFetcher, type RunStreamHandlersLike } from "@/lib/dashboard/run-controller";
import type { RunGraphResponse } from "@agentloom/api-client";
import { makeEnv, makeRun, makeStep } from "./helpers";

/** A fake stream the test drives directly through the controller's handlers. */
class FakeStream {
  started = false;
  closed = false;
  constructor(private readonly h: RunStreamHandlersLike) {}
  start() {
    this.started = true;
    return this;
  }
  close() {
    this.closed = true;
  }
  // Test drivers:
  connecting() {
    this.h.onState?.("connecting", "closed");
  }
  snapshot(runView = makeRun({ event_seq: 3, status: "running" })) {
    this.h.onState?.("backfilling", "connecting");
    this.h.onSnapshot?.({
      run: runView,
      steps: [makeStep({ id: "a", status: "running", attempt_count: 1 }), makeStep({ id: "b" })],
      edges: [],
    });
  }
  event(...envs: ReturnType<typeof makeEnv>[]) {
    for (const e of envs) this.h.onEvent?.(e);
  }
  caughtUp(seq: number) {
    this.h.onCaughtUp?.(seq);
  }
  reconnect() {
    this.h.onState?.("reconnecting", "live");
  }
}

function build(graphFetcher?: GraphFetcher) {
  let fake!: FakeStream;
  const ctrl = new RunController((h) => (fake = new FakeStream(h)), graphFetcher);
  ctrl.start();
  return { ctrl, fake: () => fake };
}

describe("RunController", () => {
  it("publishes the snapshot immediately and batches backfill at caught_up", () => {
    const { ctrl, fake } = build();
    const f = fake();
    f.snapshot(makeRun({ event_seq: 0 }));
    // Snapshot is visible right away.
    expect(ctrl.getSnapshot().run?.run.status).toBe("running");
    expect(ctrl.getSnapshot().run?.steps.get("a")?.status).toBe("running");

    // Backfill events buffer: derived state not yet applied, but the timeline is.
    f.event(makeEnv("step_succeeded", 1, { attempt: 1 }, "a"), makeEnv("step_succeeded", 2, { attempt: 1 }, "b"));
    expect(ctrl.getSnapshot().events).toHaveLength(2);
    expect(ctrl.getSnapshot().run?.steps.get("a")?.status).toBe("running"); // not applied yet

    f.caughtUp(2);
    expect(ctrl.getSnapshot().connection).toBe("live");
    expect(ctrl.getSnapshot().run?.steps.get("a")?.status).toBe("succeeded"); // applied in batch
    expect(ctrl.getSnapshot().run?.steps.get("b")?.status).toBe("succeeded");
    expect(ctrl.getSnapshot().lastSeq).toBe(2);
  });

  it("applies live events individually after caught_up", () => {
    const { ctrl, fake } = build();
    const f = fake();
    f.snapshot(makeRun({ event_seq: 0 }));
    f.caughtUp(0);
    f.event(makeEnv("run_succeeded", 1, {}));
    expect(ctrl.getSnapshot().run?.run.status).toBe("succeeded");
    expect(ctrl.getSnapshot().lastSeq).toBe(1);
  });

  it("counts a reconnect and resumes the feed gap-free (no dupes)", () => {
    const { ctrl, fake } = build();
    const f = fake();
    // First connection: snapshot + events 1,2, go live.
    f.snapshot(makeRun({ event_seq: 0 }));
    f.event(makeEnv("step_ready", 1, {}, "a"), makeEnv("step_claimed", 2, { attempt: 1 }, "a"));
    f.caughtUp(2);
    expect(ctrl.getSnapshot().reconnects).toBe(0);

    // Drop → reconnect. A fresh snapshot re-seeds (as-of = highest seq seen),
    // then the tail (3,4) resumes. The RunStream never re-delivers ≤ lastSeq,
    // so the timeline is gap-free and dup-free.
    f.reconnect();
    expect(ctrl.getSnapshot().reconnects).toBe(1);
    f.snapshot(makeRun({ event_seq: 2, status: "running" }));
    f.event(makeEnv("step_succeeded", 3, { attempt: 1 }, "a"), makeEnv("run_succeeded", 4, {}));
    f.caughtUp(4);

    const seqs = ctrl.getSnapshot().events.map((e) => e.seq);
    expect(seqs).toEqual([1, 2, 3, 4]); // gap-free, dup-free
    expect(ctrl.getSnapshot().run?.run.status).toBe("succeeded");
    expect(ctrl.getSnapshot().lastSeq).toBe(4);
  });

  it("closes the underlying stream on stop", () => {
    const { ctrl, fake } = build();
    const f = fake();
    expect(f.started).toBe(true);
    ctrl.stop();
    expect(f.closed).toBe(true);
  });

  it("merges the graph read into the topology (union with the snapshot)", async () => {
    const graph: RunGraphResponse = {
      run_id: "run-1",
      graph_version: 2,
      steps_total: 3,
      event_seq: 5,
      nodes: [
        { id: "a", type: "planner", status: "succeeded", depth: 0, graph_version: 1, origin: { kind: "definition" }, added_at: "", position: { x: 5, y: 6 } },
        { id: "b", type: "join", status: "pending", depth: 0, graph_version: 1, origin: { kind: "definition" }, added_at: "" },
        { id: "w#1", type: "llm", status: "succeeded", depth: 1, graph_version: 2, origin: { kind: "planner", step: "a" }, added_at: "" },
      ],
      edges: [],
      expansions: [],
    };
    let resolve!: (g: RunGraphResponse) => void;
    const { ctrl, fake } = build(() => new Promise<RunGraphResponse>((r) => (resolve = r)));
    fake().snapshot(makeRun({ event_seq: 0 }));
    fake().caughtUp(0);
    // Snapshot topology has a and b only.
    expect(ctrl.getSnapshot().topology.nodes.has("w#1")).toBe(false);
    // Graph read lands late → union brings the injected node + provenance + hint.
    resolve(graph);
    await Promise.resolve();
    expect(ctrl.getSnapshot().topology.nodes.has("w#1")).toBe(true);
    expect(ctrl.getSnapshot().topology.nodes.get("a")?.position).toEqual({ x: 5, y: 6 });
    expect(ctrl.getSnapshot().graphError).toBeUndefined();
  });

  it("degrades to the snapshot topology when the graph read fails", async () => {
    const { ctrl, fake } = build(() => Promise.reject(new Error("boom")));
    fake().snapshot(makeRun({ event_seq: 0 }));
    fake().caughtUp(0);
    await Promise.resolve();
    await Promise.resolve();
    expect(ctrl.getSnapshot().graphError).toBe("boom");
    // The snapshot topology is still there (a, b) — the canvas is not blank.
    expect(ctrl.getSnapshot().topology.nodes.size).toBe(2);
  });

  it("folds graph_expanded into the topology (backfill + live)", () => {
    const { ctrl, fake } = build();
    const f = fake();
    f.snapshot(makeRun({ event_seq: 0 }));
    f.event(
      makeEnv("graph_expanded", 1, {
        origin_step: "a",
        origin_kind: "planner",
        from_version: 1,
        to_version: 2,
        depth: 1,
        delta: { schema_version: 1, steps: [{ id: "w#1", type: "llm", config: {} }], edges: [{ from: "a", to: "w#1" }] },
        readied: ["w#1"],
      }),
    );
    f.caughtUp(1);
    expect(ctrl.getSnapshot().topology.nodes.has("w#1")).toBe(true);
    expect(ctrl.getSnapshot().topology.nodes.get("w#1")?.origin.kind).toBe("planner");
  });
});
