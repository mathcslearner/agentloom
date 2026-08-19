/**
 * Pure step-log page accumulation for the inspector's Logs tab (ticket 18.3).
 * Logs are poll-based (7.4 / the M18 v1 decision — no WS log channel), so the
 * tab pages via `?cursor=` and, in follow mode, re-polls the tail. This module
 * folds pages together gap-free and dedup-free by seq, and carries the
 * truncation signal. No React imports.
 */
import type { StepLogsResponse, StepLogLineView } from "@agentloom/api-client";

export type LogLevel = "debug" | "info" | "warn" | "error";

const LEVEL_ORDER: Record<LogLevel, number> = { debug: 0, info: 1, warn: 2, error: 3 };

export interface LogState {
  /** Lines in ascending seq order, deduplicated. */
  lines: StepLogLineView[];
  /** The cursor to fetch the next page (undefined ⇒ caught up). */
  nextCursor?: string;
  truncated: boolean;
  droppedLines: number;
  /** The attempt these lines belong to; a change resets accumulation. */
  attempt?: number;
}

export function emptyLogState(): LogState {
  return { lines: [], truncated: false, droppedLines: 0 };
}

/** Merge one fetched page into the accumulated state (dedup by seq). */
export function appendLogPage(state: LogState, page: StepLogsResponse): LogState {
  const reset = state.attempt !== undefined && state.attempt !== page.attempt;
  const base = reset ? emptyLogState() : state;
  const bySeq = new Map<number, StepLogLineView>();
  for (const l of base.lines) bySeq.set(l.seq, l);
  for (const l of page.lines) bySeq.set(l.seq, l);
  const lines = [...bySeq.values()].sort((a, b) => a.seq - b.seq);
  return {
    lines,
    nextCursor: page.next_cursor,
    truncated: base.truncated || page.truncated,
    droppedLines: Math.max(base.droppedLines, page.dropped_lines ?? 0),
    attempt: page.attempt,
  };
}

/** Filter accumulated lines to a minimum level. */
export function filterByLevel(lines: StepLogLineView[], min: LogLevel): StepLogLineView[] {
  const floor = LEVEL_ORDER[min];
  return lines.filter((l) => (LEVEL_ORDER[(l.level as LogLevel)] ?? 1) >= floor);
}
