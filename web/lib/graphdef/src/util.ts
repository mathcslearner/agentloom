// Small pure helpers shared by the mapping. No dependencies.

/** A plain JSON object (not null, not an array). */
export function isPlainObject(v: unknown): v is Record<string, unknown> {
  return typeof v === "object" && v !== null && !Array.isArray(v);
}

/** A finite JS number (rejects NaN/Infinity, which JSON cannot carry anyway). */
export function isFiniteNumber(v: unknown): v is number {
  return typeof v === "number" && Number.isFinite(v);
}

/** Shallow copy of an object with the named keys removed. */
export function omit(obj: Record<string, unknown>, keys: readonly string[]): Record<string, unknown> {
  const drop = new Set(keys);
  const out: Record<string, unknown> = {};
  for (const k of Object.keys(obj)) {
    if (!drop.has(k)) out[k] = obj[k];
  }
  return out;
}
