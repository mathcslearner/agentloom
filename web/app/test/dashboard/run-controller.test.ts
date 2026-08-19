import { describe, expect, it } from "vitest";
import { RunController, type RunStreamHandlersLike } from "@/lib/dashboard/run-controller";
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

function build() {
  let fake!: FakeStream;
  const ctrl = new RunController((h) => (fake = new FakeStream(h)));
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
});
