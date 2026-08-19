// Problems derivation & node marking (ticket 17.4, DoD-2): an invalid config
// marks the node and blocks submit; fixing it clears the mark; the issue codes
// and paths match the backend's.
import { act, renderHook } from "@testing-library/react";
import { beforeEach, describe, expect, it } from "vitest";
import { toFlow } from "@agentloom/graphdef";
import { emptyDefinition, useBuilderStore } from "@/lib/builder/store";
import { useProblems } from "@/lib/builder/problems";
import { mapIssuesToNodes, stepIndexOfPath } from "@/lib/builder/problems";

const store = useBuilderStore;

beforeEach(() => {
  store.getState().load(toFlow(emptyDefinition()));
});

describe("mapIssuesToNodes", () => {
  it("maps steps[i] paths onto the i-th node id", () => {
    const issues = [
      { code: "config_field_required", severity: "error" as const, path: "steps[0].config.model", msg: "x" },
      { code: "config_field_required", severity: "error" as const, path: "steps[2].config.query", msg: "y" },
    ];
    const byNode = mapIssuesToNodes(issues, ["a", "b", "c"]);
    expect(byNode.get("a")).toHaveLength(1);
    expect(byNode.get("c")).toHaveLength(1);
    expect(byNode.has("b")).toBe(false);
  });
  it("parses the step index", () => {
    expect(stepIndexOfPath("steps[3].config.model")).toBe(3);
    expect(stepIndexOfPath("edges[0].to")).toBeNull();
  });
});

describe("useProblems over the canvas", () => {
  it("marks a fresh llm step invalid, then clears when required config is filled", () => {
    const { result } = renderHook(() => useProblems());

    // A fresh llm step has an empty config → model required + prompt/messages.
    act(() => {
      store.getState().addStep("llm", { x: 0, y: 0 });
    });
    const id = store.getState().nodes[0]!.id;

    expect(result.current.hasErrors).toBe(true);
    const codes = result.current.issues.map((i) => `${i.code} ${i.path}`);
    expect(codes).toContain("config_field_required steps[0].config.model");
    expect(codes).toContain("config_field_required steps[0].config");
    expect(result.current.byNode.get(id)?.length).toBeGreaterThan(0);

    // Fill model + prompt → the step becomes valid.
    act(() => {
      store.getState().patchStep(id, { config: { model: "mock/sim-1", prompt: "hi" } });
    });
    expect(result.current.hasErrors).toBe(false);
    expect(result.current.byNode.has(id)).toBe(false);
  });

  it("attributes issues to the right node in a two-step graph", () => {
    act(() => {
      store.getState().addStep("llm", { x: 0, y: 0 }); // llm_1 (invalid)
      store.getState().addStep("retrieve", { x: 0, y: 0 }); // retrieve_1 (invalid)
    });
    const [n0, n1] = store.getState().nodes;
    act(() => {
      // Connect them so neither is an isolated step (the 17.5 graph validator
      // warns on isolated nodes), then fix the llm's config.
      store.getState().connect({ source: n0!.id, target: n1!.id, sourceHandle: "out", targetHandle: "in" });
      store.getState().patchStep(n0!.id, { config: { model: "mock/sim-1", prompt: "hi" } });
    });
    const { result } = renderHook(() => useProblems());
    // llm fixed; retrieve still missing retriever+query.
    expect(result.current.byNode.has(n0!.id)).toBe(false);
    expect(result.current.byNode.get(n1!.id)?.map((i) => i.path).sort()).toEqual([
      "steps[1].config.query",
      "steps[1].config.retriever",
    ]);
  });
});
