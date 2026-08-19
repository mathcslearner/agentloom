// DoD-1: definition → flow → definition is lossless over the backend fixture
// corpus (M1.6 files consumed directly), plus determinism and flow-idempotence.
import { describe, expect, it } from "vitest";
import { GraphdefError, toDefinition, toFlow } from "../src/index.js";
import {
  codecInvalidObjectFixtures,
  duplicateIdFixtures,
  roundTripFixtures,
} from "./fixtures.js";

describe("round-trip over the backend corpus", () => {
  const fixtures = roundTripFixtures();

  it("has a non-trivial corpus (examples + testdata all present)", () => {
    // Sanity: if the corpus failed to load, the losslessness claim is empty.
    expect(fixtures.length).toBeGreaterThan(25);
    expect(fixtures.some((f) => f.name.includes("examples/definitions/kitchen_sink"))).toBe(true);
    expect(fixtures.some((f) => f.name.includes("research-critic-writer"))).toBe(true);
  });

  for (const f of fixtures) {
    describe(f.name, () => {
      it("definition → flow → definition is lossless", () => {
        const flow = toFlow(f.value);
        const back = toDefinition(flow);
        expect(back).toEqual(f.value);
      });

      it("flow → definition → flow is idempotent", () => {
        const flow = toFlow(f.value);
        const flow2 = toFlow(toDefinition(flow));
        expect(flow2).toEqual(flow);
      });

      it("mapping is deterministic", () => {
        expect(toFlow(f.value)).toEqual(toFlow(f.value));
        expect(toDefinition(toFlow(f.value))).toEqual(toDefinition(toFlow(f.value)));
      });
    });
  }
});

describe("duplicate step ids are rejected (not canvas-representable)", () => {
  const dups = duplicateIdFixtures();

  it("finds the duplicate-id fixtures", () => {
    expect(dups.length).toBeGreaterThanOrEqual(2);
  });

  for (const f of dups) {
    it(`${f.name} throws GraphdefError(duplicate_step_id)`, () => {
      try {
        toFlow(f.value);
        expect.unreachable("toFlow should have thrown");
      } catch (err) {
        expect(err).toBeInstanceOf(GraphdefError);
        expect((err as GraphdefError).code).toBe("duplicate_step_id");
      }
    });
  }
});

describe("codec-invalid but object-shaped fixtures never crash", () => {
  const fixtures = codecInvalidObjectFixtures();

  for (const f of fixtures) {
    it(`${f.name} round-trips or throws GraphdefError`, () => {
      let flow;
      try {
        flow = toFlow(f.value);
      } catch (err) {
        expect(err).toBeInstanceOf(GraphdefError);
        return;
      }
      // If it mapped, it must still round-trip losslessly.
      expect(toDefinition(flow)).toEqual(f.value);
    });
  }
});
