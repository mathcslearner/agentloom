import { describe, expect, it } from "vitest";
import { definitionFilename } from "@/lib/builder/download";

describe("definitionFilename", () => {
  it("slugifies a name into a .json filename", () => {
    expect(definitionFilename("Research Critic Writer")).toBe("Research-Critic-Writer.json");
    expect(definitionFilename("my/wf:v2")).toBe("my-wf-v2.json");
  });

  it("falls back to workflow for empty/non-string names", () => {
    expect(definitionFilename("")).toBe("workflow.json");
    expect(definitionFilename(undefined)).toBe("workflow.json");
    expect(definitionFilename("   ")).toBe("workflow.json");
    expect(definitionFilename("///")).toBe("workflow.json");
  });
});
