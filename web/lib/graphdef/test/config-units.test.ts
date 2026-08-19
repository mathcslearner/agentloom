// Focused unit tests for the config validator's building blocks (17.4): the
// JSON-Schema subset shape checker, the Go-duration parser, and full
// (code,path,msg) equality on representative rules — the corpus parity test
// compares code+path; these lock the messages too.
import { describe, expect, it } from "vitest";
import {
  checkShape,
  fallbackConfigSchemas,
  parseGoDuration,
  validateStepConfigs,
  type Issue,
  type SchemaNode,
} from "../src/index.js";

const schemas = fallbackConfigSchemas();

function def(step: Record<string, unknown>): Record<string, unknown> {
  return { schema_version: 1, name: "t", steps: [step], edges: [] };
}
function issues(step: Record<string, unknown>): Issue[] {
  return validateStepConfigs(def(step), schemas);
}

describe("parseGoDuration", () => {
  it("parses valid durations", () => {
    expect(parseGoDuration("0")).toBe(0);
    expect(parseGoDuration("48h")).toBe(48 * 3600);
    expect(parseGoDuration("1h30m")).toBe(90 * 60);
    expect(parseGoDuration("500ms")).toBeCloseTo(0.5, 9);
    expect(parseGoDuration("9000h")).toBe(9000 * 3600);
    expect(parseGoDuration("1m0s")).toBe(60);
  });
  it("rejects invalid durations", () => {
    expect(parseGoDuration("")).toBeNull();
    expect(parseGoDuration("soon")).toBeNull();
    expect(parseGoDuration("10")).toBeNull(); // no unit
    expect(parseGoDuration("1h30")).toBeNull(); // trailing garbage
  });
});

describe("checkShape", () => {
  const s: SchemaNode = {
    type: "object",
    additionalProperties: false,
    properties: { model: { type: "string" }, max_tokens: { type: "integer" } },
  };
  it("passes a well-shaped object", () => {
    const out: Issue[] = [];
    checkShape({ model: "m", max_tokens: 8 }, s, {}, "steps[0].config", out);
    expect(out).toEqual([]);
  });
  it("flags a wrong primitive type with the backend message", () => {
    const out: Issue[] = [];
    checkShape({ model: "m", max_tokens: "many" }, s, {}, "steps[0].config", out);
    expect(out).toEqual([{ code: "", severity: "error", path: "steps[0].config.max_tokens", msg: "expected integer, got string" }]);
  });
  it("flags an unknown field", () => {
    const out: Issue[] = [];
    checkShape({ modle: "m" }, s, {}, "steps[0].config", out);
    expect(out).toEqual([{ code: "", severity: "error", path: "steps[0].config.modle", msg: "unknown field" }]);
  });
  it("flags a non-object config", () => {
    const out: Issue[] = [];
    checkShape(["nope"], s, {}, "steps[0].config", out);
    expect(out).toEqual([{ code: "", severity: "error", path: "steps[0].config", msg: "expected object, got array" }]);
  });
});

describe("full-message parity on representative rules", () => {
  it("llm missing model", () => {
    expect(issues({ id: "a", type: "llm", config: { prompt: "hi" } })).toContainEqual({
      code: "config_field_required",
      severity: "error",
      path: "steps[0].config.model",
      msg: "required field is missing",
    });
  });
  it("llm prompt and messages conflict", () => {
    expect(issues({ id: "a", type: "llm", config: { model: "m", prompt: "x", messages: [{ role: "user", content: "y" }] } })).toContainEqual({
      code: "config_field_conflict",
      severity: "error",
      path: "steps[0].config",
      msg: '"prompt" and "messages" are mutually exclusive',
    });
  });
  it("llm neither prompt nor messages", () => {
    expect(issues({ id: "a", type: "llm", config: { model: "m" } })).toContainEqual({
      code: "config_field_required",
      severity: "error",
      path: "steps[0].config",
      msg: 'exactly one of "prompt" or "messages" is required',
    });
  });
  it("output_format enum", () => {
    const got = issues({ id: "a", type: "llm", config: { model: "m", prompt: "p", output_format: { type: "xml" } } });
    expect(got).toContainEqual({
      code: "config_field_invalid",
      severity: "error",
      path: "steps[0].config.output_format.type",
      msg: 'unknown output format "xml" (expected one of "json", "json_schema")',
    });
  });
  it("human_approval timeout bound", () => {
    const got = issues({ id: "a", type: "human_approval", config: { title: "T", timeout: "9000h" } });
    expect(got).toContainEqual({
      code: "config_field_invalid",
      severity: "error",
      path: "steps[0].config.timeout",
      msg: 'must be at most 720h0m0s, got "9000h"',
    });
  });
  it("valid llm produces no errors", () => {
    expect(issues({ id: "a", type: "llm", config: { model: "mock/sim-1", prompt: "hi" } })).toEqual([]);
  });
});
