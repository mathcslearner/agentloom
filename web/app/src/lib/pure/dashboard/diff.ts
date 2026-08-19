/**
 * A tiny dependency-free line diff for the step inspector's semantic-retry
 * prompt view (ticket 18.3). Prompts are small, so a straightforward LCS over
 * lines is more than fast enough, and keeping it in-repo avoids a dependency
 * under the app's `pure/` (no-React) boundary.
 *
 * `diffLines(a, b)` returns a flat sequence of hunks in output order: `same`
 * lines present in both, `del` lines only in `a`, `add` lines only in `b`. The
 * killer-demo use is diffing attempt k's effective prompt against attempt k+1's
 * — the feedback augmentation shows up as `add` hunks with zero `del`.
 */

export type DiffKind = "same" | "add" | "del";

export interface DiffHunk {
  kind: DiffKind;
  /** The line text (no trailing newline). */
  text: string;
  /** 0-based line number in `a` (del/same) — undefined for `add`. */
  aLine?: number;
  /** 0-based line number in `b` (add/same) — undefined for `del`. */
  bLine?: number;
}

/** Split a string into lines, dropping a single trailing newline's empty tail. */
export function toLines(s: string): string[] {
  if (s === "") return [];
  const lines = s.split("\n");
  if (lines.length > 0 && lines[lines.length - 1] === "") lines.pop();
  return lines;
}

/**
 * Line-level LCS diff. Deterministic; O(n·m) table, fine for prompt-sized
 * inputs. Deletions for a replaced region are emitted before the additions.
 */
export function diffLines(a: string, b: string): DiffHunk[] {
  const al = toLines(a);
  const bl = toLines(b);
  const n = al.length;
  const m = bl.length;

  // LCS length table, flat-indexed as lcs[i * (m + 1) + j] to keep every access
  // in-bounds by construction (the repo enables noUncheckedIndexedAccess).
  const w = m + 1;
  const lcs = new Array<number>((n + 1) * w).fill(0);
  const at = (i: number, j: number): number => lcs[i * w + j] ?? 0;
  for (let i = n - 1; i >= 0; i--) {
    for (let j = m - 1; j >= 0; j--) {
      lcs[i * w + j] = al[i] === bl[j] ? at(i + 1, j + 1) + 1 : Math.max(at(i + 1, j), at(i, j + 1));
    }
  }

  const hunks: DiffHunk[] = [];
  let i = 0;
  let j = 0;
  while (i < n && j < m) {
    const av = al[i] as string;
    const bv = bl[j] as string;
    if (av === bv) {
      hunks.push({ kind: "same", text: av, aLine: i, bLine: j });
      i++;
      j++;
    } else if (at(i + 1, j) >= at(i, j + 1)) {
      hunks.push({ kind: "del", text: av, aLine: i });
      i++;
    } else {
      hunks.push({ kind: "add", text: bv, bLine: j });
      j++;
    }
  }
  for (; i < n; i++) hunks.push({ kind: "del", text: al[i] as string, aLine: i });
  for (; j < m; j++) hunks.push({ kind: "add", text: bl[j] as string, bLine: j });
  return hunks;
}

/** Counts by kind — the e2e/DoD-2 assertion reads `added`. */
export function diffStats(hunks: DiffHunk[]): { same: number; add: number; del: number } {
  const s = { same: 0, add: 0, del: 0 };
  for (const h of hunks) s[h.kind]++;
  return s;
}
