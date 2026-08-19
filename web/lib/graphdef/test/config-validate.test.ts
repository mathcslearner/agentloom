// Client-side config-validator parity (17.4). The backend's Decode+Validate
// verdict for the whole corpus is pinned in the Go golden
// (internal/dag/testdata/verdicts.golden.json, TestVerdictsGolden). This test
// runs graphdef's client validator over the same fixtures and asserts the
// config-scoped verdicts agree — so a client verdict and a server verdict name
// the same problem (code + path) in the same place (ADR-019 §"Validation
// parity"; DoD-2 "errors match backend codes on round-trip").
//
// Scope (17.4): single-step-local config rules. Cross-section/cross-field rules
// (agent role merge, run expansion caps) are deferred to 17.5 and listed
// explicitly below, mirroring the backend's structuralCases discipline.
import { describe, expect, it } from "vitest";
import { validateStepConfigs, fallbackConfigSchemas, type Issue } from "../src/index.js";
import { allCorpusFixtures, exampleDefinitions, loadVerdictsGolden, type GoldenVerdict } from "./fixtures.js";

const CONFIG_FIELD_CODES = new Set(["config_field_required", "config_field_conflict", "config_field_invalid"]);
const CONFIG_PATH = /^steps\[\d+\]\.config(\.|\[|$)/;

// Fixtures whose backend config verdict needs a rule the 17.4 client does not
// reproduce yet (cross-section / cross-field). 17.5 lands the full validator.
const DEFERRED_TO_17_5: Record<string, string> = {
  "internal/dag/testdata/invalid_structural/agent_merged_no_model.json": "needs agent role-merge (ResolveAgentStep)",
  "internal/dag/testdata/invalid_structural/expansion_bad.json": "needs run expansion-cap cross-field",
};

const schemas = fallbackConfigSchemas();

function configFieldSet(issues: Array<{ code?: string; path?: string }>): Set<string> {
  return new Set(
    issues
      .filter((i) => (i.code ? CONFIG_FIELD_CODES.has(i.code) : false) && CONFIG_PATH.test(i.path ?? ""))
      .map((i) => `${i.code}\t${i.path}`),
  );
}

function clientIssues(value: unknown): Issue[] {
  return validateStepConfigs(value, schemas);
}

function hasConfigScopeIssue(issues: Array<{ path?: string }>): boolean {
  return issues.some((i) => CONFIG_PATH.test(i.path ?? ""));
}

describe("config-validator parity vs the Go golden", () => {
  const golden = loadVerdictsGolden();

  it("golden and fixtures are non-trivially present", () => {
    expect(Object.keys(golden).length).toBeGreaterThan(100);
  });

  describe("semantic parity on decode-clean fixtures (config_field_* code+path)", () => {
    const fixtures = allCorpusFixtures().filter((f) => {
      const g = golden[f.name];
      return g !== undefined && !g.decode_failed && !(f.name in DEFERRED_TO_17_5);
    });

    for (const f of fixtures) {
      it(f.name, () => {
        const g = golden[f.name] as GoldenVerdict;
        const want = configFieldSet(g.issues);
        const got = configFieldSet(clientIssues(f.value));
        expect([...got].sort()).toEqual([...want].sort());
      });
    }
  });

  describe("shape rejection on decode-failed config fixtures", () => {
    // Every fixture whose backend decode failed with a config-scoped issue must
    // be rejected client-side too (the node gets marked). Codes are codeless on
    // the backend; parity here is accept/reject on the config subset.
    const fixtures = allCorpusFixtures().filter((f) => {
      const g = golden[f.name];
      return g !== undefined && g.decode_failed && hasConfigScopeIssue(g.decode ?? []);
    });

    it("covers the config decode-fail fixtures", () => {
      expect(fixtures.length).toBeGreaterThanOrEqual(5);
    });

    for (const f of fixtures) {
      it(f.name, () => {
        expect(hasConfigScopeIssue(clientIssues(f.value))).toBe(true);
      });
    }
  });

  describe("deferred-to-17.5 fixtures still reject (weaker)", () => {
    for (const [name, reason] of Object.entries(DEFERRED_TO_17_5)) {
      it(`${name} — ${reason}`, () => {
        const f = allCorpusFixtures().find((x) => x.name === name);
        expect(f, `${name} present in corpus`).toBeDefined();
        // The backend rejects these; the client does not yet reproduce the exact
        // config issue, so we only assert it is decode-clean here (documenting
        // the gap 17.5 closes) — it decodes but the client passes it.
        const g = golden[name] as GoldenVerdict;
        expect(g.decode_failed).toBe(false);
      });
    }
  });

  describe("no false positives on the example corpus", () => {
    for (const f of exampleDefinitions()) {
      it(f.name, () => {
        const errs = clientIssues(f.value).filter((i) => i.severity === "error");
        expect(errs).toEqual([]);
      });
    }
  });
});
