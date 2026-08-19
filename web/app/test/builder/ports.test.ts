import { describe, expect, it } from "vitest";
import type { Edge } from "@agentloom/graphdef";
import {
  deriveHandles,
  edgeSeedForHandle,
  isValidConnection,
  sourceHandlesFor,
  TARGET_HANDLE,
} from "@/lib/pure/builder/ports";

describe("sourceHandlesFor", () => {
  it("exposes out + loop on an ordinary step", () => {
    expect(sourceHandlesFor("llm")).toEqual(["out", "loop"]);
  });

  it("adds approve + reject on a human_approval step", () => {
    expect(sourceHandlesFor("human_approval")).toEqual(["out", "approve", "reject", "loop"]);
  });
});

describe("deriveHandles / edgeSeedForHandle round-trip", () => {
  it("a plain edge maps to out and seeds nothing", () => {
    const e: Edge = { from: "a", to: "b" };
    expect(deriveHandles(e).sourceHandle).toBe("out");
    expect(deriveHandles(e).targetHandle).toBe(TARGET_HANDLE);
    expect(edgeSeedForHandle("out")).toEqual({});
  });

  it("a loop edge maps to the loop handle; the seed marks a loop edge", () => {
    const e: Edge = { from: "a", to: "b", type: "loop", condition: "x", max_iterations: 2 };
    expect(deriveHandles(e).sourceHandle).toBe("loop");
    expect(edgeSeedForHandle("loop")).toEqual({ type: "loop", condition: "", max_iterations: 3 });
  });

  it("decision edges map to their handles", () => {
    expect(deriveHandles({ from: "a", to: "b", decision: "approve" }).sourceHandle).toBe("approve");
    expect(deriveHandles({ from: "a", to: "b", decision: "reject" }).sourceHandle).toBe("reject");
    expect(edgeSeedForHandle("approve")).toEqual({ decision: "approve" });
    expect(edgeSeedForHandle("reject")).toEqual({ decision: "reject" });
  });
});

describe("isValidConnection", () => {
  const base = { source: "a", target: "b", sourceHandle: "out", targetHandle: "in" };

  it("accepts a well-formed connection", () => {
    expect(isValidConnection(base, [])).toBe(true);
  });

  it("rejects a missing endpoint", () => {
    expect(isValidConnection({ ...base, source: null }, [])).toBe(false);
    expect(isValidConnection({ ...base, target: undefined }, [])).toBe(false);
  });

  it("rejects a self-connection", () => {
    expect(isValidConnection({ ...base, target: "a" }, [])).toBe(false);
  });

  it("rejects attaching to a non-input target handle", () => {
    expect(isValidConnection({ ...base, targetHandle: "out" }, [])).toBe(false);
  });

  it("tolerates a null target handle (single-target node)", () => {
    expect(isValidConnection({ ...base, targetHandle: null }, [])).toBe(true);
  });

  it("rejects an exact duplicate (source, sourceHandle, target)", () => {
    const edges = [{ source: "a", target: "b", sourceHandle: "out" }];
    expect(isValidConnection(base, edges)).toBe(false);
  });

  it("allows a second edge from a different port between the same nodes", () => {
    const edges = [{ source: "a", target: "b", sourceHandle: "out" }];
    expect(isValidConnection({ ...base, sourceHandle: "approve" }, edges)).toBe(true);
  });
});
