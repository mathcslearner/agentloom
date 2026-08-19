// Loaders for the backend's own definition corpus — consumed directly (M1.6),
// so the round-trip and passthrough tests run against exactly what the engine
// accepts. Paths are resolved from this file, like the generators.
import { readdirSync, readFileSync } from "node:fs";
import { basename, dirname, join, resolve } from "node:path";
import { fileURLToPath } from "node:url";

const here = dirname(fileURLToPath(import.meta.url));
const REPO_ROOT = resolve(here, "../../../..");

export interface Fixture {
  name: string; // "examples/definitions/fanout.json"
  path: string;
  text: string;
  value: unknown;
}

function loadDir(rel: string): Fixture[] {
  const dir = join(REPO_ROOT, rel);
  return readdirSync(dir)
    .filter((f) => f.endsWith(".json"))
    .sort()
    .map((f) => {
      const path = join(dir, f);
      const text = readFileSync(path, "utf8");
      return { name: `${rel}/${basename(f)}`, path, text, value: undefined as unknown };
    })
    .map((fx) => ({ ...fx, value: safeParse(fx.text) }));
}

function safeParse(text: string): unknown {
  try {
    return JSON.parse(text);
  } catch {
    return undefined; // deliberately-malformed fixtures (invalid/syntax.json)
  }
}

/** The canonical example workflows (M1.6) — every one is a valid definition. */
export function exampleDefinitions(): Fixture[] {
  return loadDir("examples/definitions").filter((f) => f.name !== "examples/definitions/README.md");
}

/** The valid testdata corpus (kitchen sink, ui formatting, payloads, …). */
export function validTestdata(): Fixture[] {
  return loadDir("internal/dag/testdata/valid");
}

/** The structurally-invalid corpus (cycles, dangling endpoints, bad bounds, …). */
export function invalidStructuralTestdata(): Fixture[] {
  return loadDir("internal/dag/testdata/invalid_structural");
}

/** The codec-invalid corpus (unknown fields, wrong types, syntax errors, …). */
export function invalidTestdata(): Fixture[] {
  return loadDir("internal/dag/testdata/invalid");
}

function isObj(v: unknown): v is Record<string, unknown> {
  return typeof v === "object" && v !== null && !Array.isArray(v);
}

/** A document toFlow accepts: object, well-formed steps/edges, unique ids. */
export function isStrictlyMappable(v: unknown): boolean {
  if (!isObj(v)) return false;
  const steps = v["steps"];
  const edges = v["edges"];
  if (!Array.isArray(steps) || !Array.isArray(edges)) return false;
  const ids = new Set<string>();
  for (const s of steps) {
    if (!isObj(s) || typeof s["id"] !== "string" || typeof s["type"] !== "string") return false;
    if (ids.has(s["id"])) return false; // duplicate id → not canvas-representable
    ids.add(s["id"]);
  }
  for (const e of edges) {
    if (!isObj(e) || typeof e["from"] !== "string" || typeof e["to"] !== "string") return false;
  }
  return true;
}

/**
 * Every fixture the mapping must round-trip losslessly regardless of whether the
 * backend would *validate* it: the canonical examples, the valid testdata, and
 * the structurally-invalid corpus (cycles, dangling endpoints, bad bounds — all
 * still well-shaped). The two duplicate-id fixtures are excluded here and
 * asserted to throw by {@link duplicateIdFixtures}.
 */
export function roundTripFixtures(): Fixture[] {
  return [...exampleDefinitions(), ...validTestdata(), ...invalidStructuralTestdata()].filter((f) =>
    isStrictlyMappable(f.value),
  );
}

/** Well-shaped-but-duplicate-id fixtures — toFlow must throw `duplicate_step_id`. */
export function duplicateIdFixtures(): Fixture[] {
  return invalidStructuralTestdata().filter((f) => {
    const v = f.value;
    if (!isObj(v) || !Array.isArray(v["steps"])) return false;
    if (isStrictlyMappable(v)) return false; // excludes the ok ones
    const ids = v["steps"].map((s) => (isObj(s) ? s["id"] : undefined)).filter((x): x is string => typeof x === "string");
    return new Set(ids).size !== ids.length;
  });
}

/**
 * Codec-invalid fixtures that still parse to an object with array steps/edges:
 * toFlow must either round-trip them or throw a GraphdefError — never crash.
 */
export function codecInvalidObjectFixtures(): Fixture[] {
  return invalidTestdata().filter((f) => {
    const v = f.value;
    return isObj(v) && Array.isArray(v["steps"]) && Array.isArray(v["edges"]);
  });
}
