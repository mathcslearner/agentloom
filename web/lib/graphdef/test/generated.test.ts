// Sanity checks on the generated definition types: the runtime STEP_TYPES array
// matches the schema's StepType enum, and a compile-time assertion pins the
// discriminated Step narrowing. (Drift itself is caught by the CI `git diff`
// against a fresh `pnpm generate`; this just guards the generator's own shape.)
import { readFileSync } from "node:fs";
import { dirname, resolve } from "node:path";
import { fileURLToPath } from "node:url";
import { describe, expect, it } from "vitest";
import { STEP_TYPES, type Step, type StepType } from "../src/index.js";

const here = dirname(fileURLToPath(import.meta.url));
const SCHEMA = resolve(here, "../../../../docs/schema/workflow-definition.v1.json");

describe("generated definition types", () => {
  it("STEP_TYPES equals the schema's StepType enum, in order", () => {
    const schema = JSON.parse(readFileSync(SCHEMA, "utf8")) as {
      $defs: { StepType: { enum: string[] } };
    };
    expect([...STEP_TYPES]).toEqual(schema.$defs.StepType.enum);
  });

  it("covers the M17 palette node types", () => {
    for (const t of ["llm", "tool", "retrieve", "branch", "map", "planner", "agent", "human_approval", "join"]) {
      expect(STEP_TYPES).toContain(t);
    }
  });

  it("Step narrows config by type at compile time", () => {
    // Compile-time: switching on `type` narrows `config`. (No runtime assertion
    // beyond this constructing — the value shows the narrowing type-checks.)
    const llm: Step = { id: "x", type: "llm", config: { model: "mock/sim-1", prompt: "hi" } };
    const branch: Step = { id: "y", type: "branch", config: {} };
    const narrow = (s: Step): string | undefined => {
      switch (s.type) {
        case "llm":
          return s.config?.model; // typed as LLMConfig here
        case "branch":
          return "branch";
        default:
          return undefined;
      }
    };
    expect(narrow(llm)).toBe("mock/sim-1");
    expect(narrow(branch)).toBe("branch");
    const t: StepType = "join";
    expect(STEP_TYPES).toContain(t);
  });
});
