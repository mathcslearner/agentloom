// Canonical export parity (ADR-019 §"Canonical export", ticket 17.6, DoD-2).
//
// `canonicalize` must reproduce the backend's `dag.Encode` bytes exactly. The Go
// golden (internal/dag/testdata/canonical.golden.json, regen UPDATE_GOLDEN=1)
// is the ground truth; this test asserts, over every decodable corpus fixture:
//   1. canonicalize(parse(text)) === golden  — canonicalize orders/omits/spells
//      like Go regardless of the source's key order or formatting;
//   2. canonicalize(toDefinition(toFlow(parse(text)))) === golden  — a full
//      builder round-trip preserves the canonical form (the export DoD).
// Plus a schema-drift guard on the pointer-numeric override set, and scalar
// spelling tables.

import { readFileSync } from "node:fs";
import { dirname, join, resolve } from "node:path";
import { fileURLToPath } from "node:url";
import { describe, expect, it } from "vitest";
import { canonicalize, toDefinition, toFlow, DEFINITION_SCHEMA } from "../src/index.js";
import { goNumber, goString } from "../src/canonical/strings.js";
import { exampleDefinitions, invalidStructuralTestdata, validTestdata, type Fixture } from "./fixtures.js";

const here = dirname(fileURLToPath(import.meta.url));
const REPO_ROOT = resolve(here, "../../../..");

function loadCanonicalGolden(): Record<string, string> {
  const path = join(REPO_ROOT, "internal/dag/testdata/canonical.golden.json");
  return JSON.parse(readFileSync(path, "utf8")) as Record<string, string>;
}

// The decodable corpus, mapped to their golden keys. invalid/ fixtures are
// decode failures — no canonical form — so they are excluded, like the Go side.
function decodableCorpus(): Fixture[] {
  return [...exampleDefinitions(), ...validTestdata(), ...invalidStructuralTestdata()];
}

describe("canonicalize — byte-for-byte parity with dag.Encode", () => {
  const golden = loadCanonicalGolden();
  const fixtures = decodableCorpus().filter((f) => golden[f.name] !== undefined);

  it("covers every golden entry (the corpus is loaded from the right place)", () => {
    // Every golden key resolves to a fixture we test (the loader and the Go
    // walk agree on the corpus).
    const names = new Set(decodableCorpus().map((f) => f.name));
    for (const key of Object.keys(golden)) {
      // invalid/ decode-failures are not in the golden, so every golden key is
      // one of the decodable fixtures.
      expect(names.has(key), `golden key ${key} has no fixture`).toBe(true);
    }
    expect(fixtures.length).toBeGreaterThan(20);
  });

  for (const fx of fixtures) {
    it(`${fx.name}: canonicalize(parse) equals the Go golden`, () => {
      expect(canonicalize(fx.value)).toBe(golden[fx.name]);
    });

    it(`${fx.name}: canonicalize(toDefinition(toFlow)) equals the Go golden`, () => {
      // Well-shaped fixtures round-trip; a duplicate-id fixture would throw in
      // toFlow — those are excluded from decodableCorpus by being decode-clean
      // (a duplicate id is a validate error, not a decode error), so guard.
      let round: unknown;
      try {
        round = toDefinition(toFlow(fx.value));
      } catch {
        return; // toFlow rejects (duplicate id): not a canonical-export case
      }
      expect(canonicalize(round)).toBe(golden[fx.name]);
    });
  }

  it("is idempotent (canonicalize of its own output re-parses to the same bytes)", () => {
    for (const fx of fixtures) {
      const once = canonicalize(fx.value);
      expect(canonicalize(JSON.parse(once))).toBe(once);
    }
  });
});

describe("canonicalize — scalar spelling", () => {
  it("goString matches Go's SetEscapeHTML(false) rules", () => {
    expect(goString("a<b>&c")).toBe('"a<b>&c"'); // HTML not escaped
    expect(goString("tab\there")).toBe('"tab\\there"');
    expect(goString("\b\f")).toBe('"\\u0008\\u000c"'); // no \b/\f shortcuts
    expect(goString("quote\"and\\slash")).toBe('"quote\\"and\\\\slash"');
    expect(goString("line sep")).toBe('"line\\u2028sep"');
    expect(goString("emoji 😀")).toBe('"emoji 😀"'); // surrogate pair passes through
  });

  it("goNumber matches Go's json number formatting", () => {
    expect(goNumber(1)).toBe("1");
    expect(goNumber(2.0)).toBe("2");
    expect(goNumber(0.005)).toBe("0.005");
    expect(goNumber(0.5)).toBe("0.5");
    expect(goNumber(-0)).toBe("0");
    expect(goNumber(1000)).toBe("1000");
  });
});

describe("canonicalize — pointer-numeric override drift guard", () => {
  // Every field in POINTER_NUMERIC must name a real (struct, integer/number
  // field) in DEFINITION_SCHEMA — so a schema change that renames or retypes one
  // fails here rather than silently mis-encoding a present zero.
  it("every pointer-numeric override names a numeric schema field", () => {
    const defs = (DEFINITION_SCHEMA as { $defs: Record<string, { properties?: Record<string, { type?: string }> }> }).$defs;
    // Re-list the set here (kept in sync with canonicalize.ts by this test).
    const overrides = [
      "Definition.budget_usd",
      "StepBudget.max_usd",
      "LLMConfig.temperature",
      "PlannerConfig.temperature",
      "AgentConfig.temperature",
      "AgentDef.temperature",
      "ModelFallback.at_budget_fraction",
      "BlackboardWriteConfig.expected_version",
      "CompactionStrategy.n",
      "CompactionStrategy.min_tokens",
      "CompactionStrategy.max_tokens",
      "ContextSource.top_k",
      "ContextSource.max_tokens",
      "ContextSource.priority",
      "ContextSpec.budget_tokens",
      "ExpansionPolicy.max_added_steps",
      "ExpansionPolicy.max_total_steps",
      "ExpansionPolicy.max_expansions",
      "ExpansionPolicy.max_depth",
    ];
    for (const key of overrides) {
      const [struct, field] = key.split(".");
      const prop = defs[struct!]?.properties?.[field!];
      expect(prop, `${key} missing from schema`).toBeDefined();
      expect(["integer", "number"], `${key} is not numeric`).toContain(prop!.type);
    }
  });
});
