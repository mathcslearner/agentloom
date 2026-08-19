import { describe, expect, it } from "vitest";
import type { operations, paths } from "../src/index.js";

/**
 * Sanity checks on the generated types. These are mostly compile-time — if the
 * generator produced a broken or empty file, this file would not typecheck.
 * A couple of runtime asserts keep vitest from reporting an empty suite.
 */
describe("generated openapi types", () => {
  it("exposes the control-plane paths", () => {
    // Compile-time existence checks (the values are types, exercised via
    // assignment below); the runtime assert just anchors the test.
    type _Runs = paths["/v1/runs"]["get"];
    type _Def = paths["/v1/definitions"]["get"];
    type _Health = paths["/healthz"]["get"];
    type _ListRuns = operations["listRuns"];
    type _ListDefs = operations["listDefinitions"];
    const marker: _Runs extends never ? false : true = true;
    expect(marker).toBe(true);
  });
});
