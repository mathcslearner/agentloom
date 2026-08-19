// Full validator parity (17.5, ADR-019 §"Validation parity" / DoD-1). The
// backend's Decode+Validate verdict for the whole corpus is pinned in the Go
// golden (internal/dag/testdata/verdicts.golden.json, TestVerdictsGolden). This
// runs graphdef's client validator over the same fixtures and asserts:
//
//   1. identical accept/reject over ALL fixtures (the literal DoD-1);
//   2. exact (severity, code, path) equality over the VALIDATE stage
//      (decode-clean fixtures), so a client verdict and a server verdict name
//      the same problems in the same places — the strong guarantee;
//   3. every decode-failed fixture rejects client-side (a codeless decode
//      finding), and no example workflow produces any error.
//
// Client-only advisory warnings (advisory_*) are excluded from the coded-set
// comparison; the backend does not model them.

import { describe, expect, it } from "vitest";
import { hasErrors, validateDefinition, type Issue } from "../src/index.js";
import { allCorpusFixtures, exampleDefinitions, loadVerdictsGolden, type GoldenVerdict } from "./fixtures.js";

interface GoldenIssue {
  code?: string;
  severity: string;
  path?: string;
  msg: string;
}

function goldenRejects(g: GoldenVerdict): boolean {
  if (g.decode_failed) return true;
  return (g.issues ?? []).some((i) => i.severity === "error");
}

function codedSet(issues: readonly (Issue | GoldenIssue)[]): Set<string> {
  return new Set(
    issues
      .filter((i) => !(i.code ?? "").startsWith("advisory_"))
      .map((i) => `${i.severity}\t${i.code ?? ""}\t${i.path ?? ""}`),
  );
}

describe("full-validator parity vs the Go golden", () => {
  const golden = loadVerdictsGolden();
  const fixtures = allCorpusFixtures();

  it("golden and corpus are non-trivially present", () => {
    expect(Object.keys(golden).length).toBeGreaterThan(120);
    expect(fixtures.length).toBeGreaterThan(120);
  });

  describe("accept/reject parity over the whole corpus (DoD-1)", () => {
    for (const f of fixtures) {
      const g = golden[f.name];
      if (g === undefined) continue;
      it(f.name, () => {
        expect(hasErrors(validateDefinition(f.value))).toBe(goldenRejects(g));
      });
    }
  });

  describe("exact (severity, code, path) parity over the validate stage", () => {
    const decodeClean = fixtures.filter((f) => {
      const g = golden[f.name];
      return g !== undefined && !g.decode_failed;
    });
    it("covers a substantial validate-stage set", () => {
      expect(decodeClean.length).toBeGreaterThan(70);
    });
    for (const f of decodeClean) {
      it(f.name, () => {
        const g = golden[f.name] as GoldenVerdict;
        const want = [...codedSet(g.issues ?? [])].sort();
        const got = [...codedSet(validateDefinition(f.value))].sort();
        expect(got).toEqual(want);
      });
    }
  });

  describe("decode-failed fixtures reject client-side", () => {
    const decodeFailed = fixtures.filter((f) => golden[f.name]?.decode_failed);
    it("covers the decode-failed corpus", () => {
      expect(decodeFailed.length).toBeGreaterThan(30);
    });
    for (const f of decodeFailed) {
      it(f.name, () => {
        expect(hasErrors(validateDefinition(f.value))).toBe(true);
      });
    }
  });

  describe("no false positives on the example corpus", () => {
    for (const f of exampleDefinitions()) {
      it(f.name, () => {
        expect(validateDefinition(f.value).filter((i) => i.severity === "error")).toEqual([]);
      });
    }
  });
});
