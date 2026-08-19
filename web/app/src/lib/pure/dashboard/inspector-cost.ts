/**
 * Pure cost derivations for the inspector's Cost tab (ticket 18.3): filter a
 * run's cost ledger to one step, split productive from overhead
 * (llm_judge/summarization) spend, and surface cache savings. No React imports.
 */
import type { RunCostResponse, CostEntryView, CostByStepView } from "@agentloom/api-client";

export interface CostRow {
  attempt: number;
  entry: string;
  resource: string;
  inputTokens?: number;
  outputTokens?: number;
  rateSource: string;
  cacheHit: boolean;
  overhead: boolean;
  spentNanoUsd: number;
  savedNanoUsd: number;
}

export interface StepCost {
  rows: CostRow[];
  spentNanoUsd: number;
  savedNanoUsd: number;
  overheadNanoUsd: number;
  /** The step's aggregate row, when present in `by_step`. */
  aggregate?: CostByStepView;
}

function tokens(usage: unknown): { input?: number; output?: number } {
  const u = (typeof usage === "string" ? safeParse(usage) : usage) as
    | { input_tokens?: number; output_tokens?: number }
    | undefined;
  return { input: u?.input_tokens, output: u?.output_tokens };
}

function safeParse(s: string): unknown {
  try {
    return JSON.parse(s);
  } catch {
    return undefined;
  }
}

/** Filter the run cost response to one step and roll it up. */
export function stepCost(cost: RunCostResponse | undefined, stepId: string): StepCost {
  const empty: StepCost = { rows: [], spentNanoUsd: 0, savedNanoUsd: 0, overheadNanoUsd: 0 };
  if (!cost) return empty;
  const rows: CostRow[] = [];
  let spent = 0;
  let saved = 0;
  let overhead = 0;
  for (const e of cost.entries as CostEntryView[]) {
    if (e.step_id !== stepId) continue;
    const t = tokens(e.usage);
    rows.push({
      attempt: e.attempt,
      entry: e.entry,
      resource: e.resource,
      inputTokens: t.input,
      outputTokens: t.output,
      rateSource: e.rate_source,
      cacheHit: e.cache_hit,
      overhead: e.overhead,
      spentNanoUsd: e.spent_nano_usd,
      savedNanoUsd: e.saved_nano_usd,
    });
    spent += e.spent_nano_usd;
    saved += e.saved_nano_usd;
    if (e.overhead) overhead += e.spent_nano_usd;
  }
  rows.sort((a, b) => a.attempt - b.attempt || a.entry.localeCompare(b.entry));
  const aggregate = (cost.by_step ?? []).find((s) => s.step_id === stepId);
  return { rows, spentNanoUsd: spent, savedNanoUsd: saved, overheadNanoUsd: overhead, aggregate };
}

/** Render integer nano-USD as a trimmed USD string ("$0.0042"). */
export function nanoUsd(n: number): string {
  const usd = n / 1e9;
  return `$${usd.toFixed(6).replace(/0+$/, "").replace(/\.$/, "") || "0"}`;
}
