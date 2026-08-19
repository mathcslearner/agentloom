import { beforeEach, describe, expect, it } from "vitest";
import { toFlow } from "@agentloom/graphdef";
import { emptyDefinition, selectIsDirty, useBuilderStore } from "@/lib/builder/store";

const store = useBuilderStore;

beforeEach(() => {
  store.getState().load(toFlow(emptyDefinition()));
});

describe("builder persistence state (17.6)", () => {
  it("a freshly loaded canvas is clean", () => {
    expect(selectIsDirty(store.getState())).toBe(false);
    expect(store.getState().source).toBeNull();
  });

  it("an edit marks the canvas dirty", () => {
    store.getState().addStep("noop");
    expect(selectIsDirty(store.getState())).toBe(true);
  });

  it("markSaved records the source and clears the dirty flag", () => {
    store.getState().addStep("noop");
    store.getState().markSaved({ id: "def-1", name: "wf", version: 3 });
    expect(store.getState().source).toEqual({ id: "def-1", name: "wf", version: 3 });
    expect(selectIsDirty(store.getState())).toBe(false);
  });

  it("editing after a save marks dirty again without touching the source", () => {
    store.getState().addStep("noop");
    store.getState().markSaved({ id: "def-1", name: "wf", version: 1 });
    store.getState().addStep("echo");
    expect(selectIsDirty(store.getState())).toBe(true);
    expect(store.getState().source).toEqual({ id: "def-1", name: "wf", version: 1 });
  });

  it("load with a source opens clean at that version", () => {
    store.getState().load(toFlow({ ...emptyDefinition(), name: "opened" }), { id: "d", name: "opened", version: 2 });
    expect(selectIsDirty(store.getState())).toBe(false);
    expect(store.getState().source).toEqual({ id: "d", name: "opened", version: 2 });
    expect(store.getState().doc["name"]).toBe("opened");
  });

  it("patchDoc edits document meta and dirties", () => {
    store.getState().patchDoc({ name: "renamed" });
    expect(store.getState().doc["name"]).toBe("renamed");
    expect(selectIsDirty(store.getState())).toBe(true);
  });

  it("the tracked snapshot ignores selection churn (not dirtied by selecting)", () => {
    const id = store.getState().addStep("noop");
    store.getState().markSaved({ id: "d", name: "wf", version: 1 });
    store.getState().selectOnly("node", id);
    expect(selectIsDirty(store.getState())).toBe(false);
  });
});
