// DoD-2: unknown future fields survive the round-trip untouched.
//
// Two layers: (1) a hand-written definition with unknown keys at every object
// level and a junk-laden `ui` block; (2) a seeded property test that injects
// random `x_future_*` keys and random layout perturbations across the whole
// backend corpus and asserts the round-trip is still lossless. No external
// dependency — a small deterministic PRNG drives the mutation (the codebase
// prefers deterministic generators; see engine-client's emitter rationale).
import { describe, expect, it } from "vitest";
import { toDefinition, toFlow } from "../src/index.js";
import { roundTripFixtures } from "./fixtures.js";

describe("explicit unknown-field passthrough", () => {
  const def = {
    schema_version: 1,
    name: "forward-compat",
    x_future_top: { anything: [1, 2, 3], nested: { deep: true } },
    on_failure: "fail_fast",
    steps: [
      {
        id: "a",
        type: "llm",
        config: { model: "mock/sim-1", prompt: "hi", x_future_config: 42 },
        retry: { max_attempts: 2, x_future_envelope: "keep" },
        x_future_step: ["survives"],
      },
      { id: "b", type: "noop", x_only: null },
    ],
    edges: [{ from: "a", to: "b", x_future_edge: { weight: 0.5 } }],
    ui: {
      nodes: {
        a: { position: { x: 10, y: 20 }, collapsed: true, x_future_node_ui: "hint" },
        b: { position: { x: 200, y: 20 } },
        ghost: { position: { x: 999, y: 999 }, note: "orphan entry, no step" },
      },
      viewport: { x: -5, y: -5, zoom: 1.25 },
      version: 7,
      x_future_ui: ["junk", { goes: "here" }],
    },
  };

  it("round-trips every unknown key and the orphan ui entry", () => {
    const back = toDefinition(toFlow(def));
    expect(back).toEqual(def);
  });

  it("keeps per-node ui hints beyond position on the node data", () => {
    const flow = toFlow(def);
    const a = flow.nodes.find((n) => n.id === "a")!;
    expect(a.data.positioned).toBe(true);
    expect(a.position).toEqual({ x: 10, y: 20 });
    expect(a.data.ui).toEqual({ collapsed: true, x_future_node_ui: "hint" });
    const ghost = flow.ui["nodes"] as Record<string, unknown>;
    expect(ghost["ghost"]).toEqual({ position: { x: 999, y: 999 }, note: "orphan entry, no step" });
  });
});

// --- seeded property test -------------------------------------------------

/** Mulberry32 — a tiny deterministic PRNG (public-domain algorithm). */
function makeRng(seed: number): () => number {
  let a = seed >>> 0;
  return () => {
    a |= 0;
    a = (a + 0x6d2b79f5) | 0;
    let t = Math.imul(a ^ (a >>> 15), 1 | a);
    t = (t + Math.imul(t ^ (t >>> 7), 61 | t)) ^ t;
    return ((t ^ (t >>> 14)) >>> 0) / 4294967296;
  };
}

type Json = null | boolean | number | string | Json[] | { [k: string]: Json };

function randomValue(rng: () => number, depth: number): Json {
  const r = rng();
  if (depth > 2 || r < 0.4) {
    if (r < 0.1) return null;
    if (r < 0.2) return rng() < 0.5;
    if (r < 0.3) return Math.floor(rng() * 1000);
    return `v${Math.floor(rng() * 10000)}`;
  }
  if (r < 0.7) return [randomValue(rng, depth + 1), randomValue(rng, depth + 1)];
  return { k: randomValue(rng, depth + 1), n: Math.floor(rng() * 100) };
}

/**
 * Recursively inject `x_future_*` keys into objects with probability p. A
 * `position` value is a builder-owned closed `{x, y}` leaf graphdef normalizes —
 * not an arbitrary passthrough object — so it is left untouched (a future builder
 * that added `position.z` would update graphdef too; the backend never writes it).
 */
function inject(v: Json, rng: () => number, p: number, key?: string): Json {
  if (key === "position") return v;
  if (Array.isArray(v)) return v.map((e) => inject(e, rng, p));
  if (v !== null && typeof v === "object") {
    const out: { [k: string]: Json } = {};
    for (const k of Object.keys(v)) out[k] = inject(v[k]!, rng, p, k);
    if (rng() < p) out[`x_future_${Math.floor(rng() * 100000)}`] = randomValue(rng, 0);
    return out;
  }
  return v;
}

describe("property: injected future fields survive across the corpus", () => {
  const fixtures = roundTripFixtures();

  it("round-trip stays lossless under random field injection (many cases)", () => {
    const rng = makeRng(0x51ed);
    let cases = 0;
    // Several independent mutations per fixture → thousands of cases total.
    for (const f of fixtures) {
      for (let iter = 0; iter < 12; iter++) {
        const mutated = inject(structuredClone(f.value) as Json, rng, 0.5);
        // The mutation may add unknown keys to steps/edges but keeps id/type/
        // from/to intact (spread preserves them), so it stays mappable.
        const back = toDefinition(toFlow(mutated));
        expect(back).toEqual(mutated);
        cases++;
      }
    }
    expect(cases).toBeGreaterThan(300);
  });
});
