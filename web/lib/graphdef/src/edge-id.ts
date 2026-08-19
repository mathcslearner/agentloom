// Synthesized, deterministic edge identity (ADR-019).
//
// The definition has no edge ids and duplicate (from, to) pairs are legal, so
// the canvas needs a stable id per edge. It is a function of (from, to,
// duplicate-ordinal): `from->to` for the first occurrence, `from->to#n`
// (n >= 2) for later duplicates of the same pair, assigned in scan order. Step
// ids match `^[a-z][a-z0-9_-]{0,63}$`, so they contain neither `>` nor `#` —
// which makes both `->` and `#` unambiguous delimiters and the id collision-free
// across distinct pairs.

/** The id for the n-th edge (1-based) of a given (from, to) pair. */
export function edgeId(from: string, to: string, n: number): string {
  const base = `${from}->${to}`;
  return n <= 1 ? base : `${base}#${n}`;
}

/**
 * Assign ids to a list of edges in order, disambiguating duplicate (from, to)
 * pairs with an ordinal. Returns one id per input edge, in the same order.
 */
export function assignEdgeIds(edges: readonly { from: string; to: string }[]): string[] {
  const counts = new Map<string, number>();
  return edges.map((e) => {
    const base = `${e.from}->${e.to}`;
    const n = (counts.get(base) ?? 0) + 1;
    counts.set(base, n);
    return edgeId(e.from, e.to, n);
  });
}
