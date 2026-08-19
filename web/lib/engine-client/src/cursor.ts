/**
 * Per-run seq cursors — the resume state that makes recovery deterministic.
 * Delivery is at-least-once and ordered by `(run_id, seq)` (ADR-018); the client
 * dedupes and orders by tracking the highest seq seen per run.
 */

/** The outcome of offering a `(runId, seq)` to the cursor. */
export type Offer =
  | { readonly kind: "new" } // seq == last + 1 (or first at last+1): deliver
  | { readonly kind: "duplicate" } // seq <= last: already seen, drop
  | { readonly kind: "gap"; readonly expected: number }; // seq > last + 1: missed events

export class RunCursors {
  private readonly last = new Map<string, number>();

  constructor(initial?: Record<string, number>) {
    if (initial) {
      for (const [runId, seq] of Object.entries(initial)) this.last.set(runId, seq);
    }
  }

  /** The highest contiguous seq delivered for a run (0 if none). */
  lastSeq(runId: string): number {
    return this.last.get(runId) ?? 0;
  }

  /**
   * Classify an incoming seq without mutating state. A `new` seq is exactly one
   * past the last; a `gap` means one or more events were missed (heal via a
   * backfill from `lastSeq`); a `duplicate` is at or below the last.
   */
  classify(runId: string, seq: number): Offer {
    const last = this.lastSeq(runId);
    if (seq <= last) return { kind: "duplicate" };
    if (seq === last + 1) return { kind: "new" };
    return { kind: "gap", expected: last + 1 };
  }

  /** Advance a run's cursor to `seq` (only ever forward). */
  advance(runId: string, seq: number): void {
    if (seq > this.lastSeq(runId)) this.last.set(runId, seq);
  }

  /** Whether a run is being tracked. */
  has(runId: string): boolean {
    return this.last.has(runId);
  }

  /** Drop a run's cursor (e.g. terminal-first eviction to stay within bounds). */
  forget(runId: string): void {
    this.last.delete(runId);
  }

  /** The tracked runs, in insertion order. */
  runIds(): string[] {
    return [...this.last.keys()];
  }

  size(): number {
    return this.last.size;
  }

  /** A snapshot suitable for a firehose `cursors` resume map. */
  snapshot(): Record<string, number> {
    return Object.fromEntries(this.last);
  }
}
