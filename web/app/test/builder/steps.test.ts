import { describe, expect, it } from "vitest";
import { allocateStepId, cascadePlacement, newStepForType } from "@/lib/pure/builder/steps";
import { CORE_TYPES, TEST_TYPES, stepMeta } from "@/lib/pure/builder/catalog";
import { STEP_TYPES } from "@agentloom/graphdef";

describe("allocateStepId", () => {
  it("starts at _1", () => {
    expect(allocateStepId("llm", [])).toBe("llm_1");
  });

  it("skips taken ordinals", () => {
    expect(allocateStepId("llm", ["llm_1", "llm_2"])).toBe("llm_3");
  });

  it("fills the lowest free gap", () => {
    expect(allocateStepId("llm", ["llm_2"])).toBe("llm_1");
    expect(allocateStepId("llm", ["llm_1", "llm_3"])).toBe("llm_2");
  });
});

describe("newStepForType", () => {
  it("makes a bare step with an empty config", () => {
    const s = newStepForType("tool", "tool_1");
    expect(s.id).toBe("tool_1");
    expect(s.type).toBe("tool");
    expect(s.config).toEqual({});
  });
});

describe("cascadePlacement", () => {
  it("offsets by count and wraps at the span", () => {
    expect(cascadePlacement({ x: 10, y: 20 }, 0)).toEqual({ x: 10, y: 20 });
    expect(cascadePlacement({ x: 0, y: 0 }, 1)).toEqual({ x: 28, y: 28 });
    expect(cascadePlacement({ x: 0, y: 0 }, 8)).toEqual({ x: 0, y: 0 });
  });
});

describe("catalog", () => {
  it("covers every step type exactly once across the two groups", () => {
    const grouped = [...CORE_TYPES, ...TEST_TYPES];
    expect(new Set(grouped).size).toBe(grouped.length);
    expect(new Set(grouped)).toEqual(new Set(STEP_TYPES));
    expect(CORE_TYPES).toHaveLength(10);
  });

  it("summaries never throw on an empty or malformed config", () => {
    for (const t of STEP_TYPES) {
      expect(() => stepMeta(t).summary({ id: "x", type: t } as never)).not.toThrow();
      expect(() => stepMeta(t).summary({ id: "x", type: t, config: null } as never)).not.toThrow();
    }
  });
});
