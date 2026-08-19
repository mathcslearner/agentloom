// Field planning (ticket 17.4, DoD-1): every built-in plugin's config is
// editable via generated forms — no hardcoded per-plugin forms. We assert that
// planFields produces a field for every property of every executor config
// schema, and that the specialized widgets land on the right fields.
import { describe, expect, it } from "vitest";
import { STEP_TYPES, fallbackConfigSchemas, resolveRef, type SchemaNode } from "@agentloom/graphdef";
import { planFields } from "@/lib/pure/builder/fields";

const schemas = fallbackConfigSchemas();

function propertyNames(entry: { schema: SchemaNode; defs: Record<string, SchemaNode> }): string[] {
  const s = resolveRef(entry.schema, entry.defs);
  if (s === true || s === false || !s.properties) return [];
  return Object.keys(s.properties);
}

describe("planFields covers every executor config property", () => {
  for (const type of STEP_TYPES) {
    it(`${type}: a field per config property`, () => {
      const entry = schemas[type];
      expect(entry, `${type} has a fallback schema`).toBeDefined();
      const props = propertyNames(entry!);
      const plan = planFields(entry!.schema, { stepType: type, defs: entry!.defs });
      expect(plan.map((f) => f.name).sort()).toEqual([...props].sort());
    });
  }
});

describe("widget selection", () => {
  function plan(type: (typeof STEP_TYPES)[number]) {
    const e = schemas[type]!;
    return planFields(e.schema, { stepType: type, defs: e.defs });
  }
  function widget(type: (typeof STEP_TYPES)[number], name: string) {
    return plan(type).find((f) => f.name === name)?.widget;
  }

  it("uses the model picker for llm/planner model", () => {
    expect(widget("llm", "model")).toBe("model");
    expect(widget("planner", "model")).toBe("model");
  });
  it("uses the prompt editor for llm prompt", () => {
    expect(widget("llm", "prompt")).toBe("prompt");
  });
  it("uses pickers for tool/retriever/agent/map body", () => {
    expect(widget("tool", "tool")).toBe("picker");
    expect(widget("retrieve", "retriever")).toBe("picker");
    expect(widget("agent", "agent")).toBe("picker");
    expect(widget("map", "body")).toBe("picker");
  });
  it("uses a select for enum fields (join mode, output_format.type)", () => {
    expect(widget("join", "mode")).toBe("enum");
    const of = plan("llm").find((f) => f.name === "output_format");
    expect(of?.widget).toBe("object");
    expect(of?.fields?.find((f) => f.name === "type")?.widget).toBe("enum");
  });
  it("uses number/integer for numeric fields", () => {
    expect(widget("retrieve", "top_k")).toBe("integer");
    expect(widget("llm", "temperature")).toBe("number");
  });
  it("uses object-list for llm messages and json for raw fields", () => {
    expect(widget("llm", "messages")).toBe("object-list");
    expect(widget("tool", "input")).toBe("json");
    expect(widget("map", "items")).toBe("json");
    expect(widget("blackboard_write", "value")).toBe("json");
  });
  it("marks required config fields", () => {
    const required = (t: (typeof STEP_TYPES)[number], n: string) => plan(t).find((f) => f.name === n)?.required;
    expect(required("llm", "model")).toBe(true);
    expect(required("retrieve", "retriever")).toBe(true);
    expect(required("retrieve", "query")).toBe(true);
    expect(required("llm", "temperature")).toBe(false);
  });
  it("hides engine-populated agent.role", () => {
    expect(plan("agent").find((f) => f.name === "role")?.hint).toBe("hidden");
  });
});
