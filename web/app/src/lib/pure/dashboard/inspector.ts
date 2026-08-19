/**
 * Pure derivations for the step inspector (ticket 18.3). Each function turns a
 * step's `StepView` (its attempt history, config, verdicts) plus the live event
 * feed into the rows a tab renders. No React/UI imports — under the app's
 * `pure/` boundary — so every derivation is unit-tested against fixtures.
 *
 * Two provenance sources are unioned where both exist: the `StepView` (the
 * durable snapshot/refresh) and the live event feed (what has happened since
 * the last snapshot). The claim/worker history in particular reads from both —
 * a `step_claimed`/`step_reclaimed` event names the worker before the next
 * detail refetch lands its attempt row.
 */
import type { StepView } from "@agentloom/api-client";
import type { EventEnvelope } from "@agentloom/engine-client";
import { diffLines, type DiffHunk } from "./diff";

/** A model-call config shape the inspector reads for prompt reconstruction. */
interface LlmConfigShape {
  model?: string;
  prompt?: string;
  messages?: { role: string; content: string }[];
  model_fallbacks?: { model: string; at_budget_fraction?: number }[];
}

interface FeedbackShape {
  text?: string;
  semantic_attempt?: number;
  max_attempts?: number;
  prior_attempt?: number;
}

/** Parse a JSON-ish value defensively; undefined on any error. */
function parse<T>(raw: unknown): T | undefined {
  if (raw == null) return undefined;
  if (typeof raw === "string") {
    try {
      return JSON.parse(raw) as T;
    } catch {
      return undefined;
    }
  }
  return raw as T;
}

/** One attempt row for the Overview timeline. */
export interface AttemptRow {
  attempt: number;
  claimId: string;
  workerId?: string;
  outcome?: string;
  startedAt?: string;
  finishedAt?: string;
  /** Milliseconds start→finish, when both are present. */
  durationMs?: number;
  /** True when this attempt was lost to a lease takeover (outcome `lost`). */
  reclaimed: boolean;
  hasVerdict: boolean;
  hasFeedback: boolean;
}

function durationMs(a?: string, b?: string): number | undefined {
  if (!a || !b) return undefined;
  const d = Date.parse(b) - Date.parse(a);
  return Number.isFinite(d) ? d : undefined;
}

/** The per-attempt timeline (ascending attempt number). */
export function attemptTimeline(step: StepView): AttemptRow[] {
  const attempts = step.attempts ?? [];
  return attempts.map((a) => ({
    attempt: a.attempt,
    claimId: a.claim_id,
    workerId: a.worker_id,
    outcome: a.outcome,
    startedAt: a.started_at,
    finishedAt: a.finished_at,
    durationMs: durationMs(a.started_at, a.finished_at),
    reclaimed: a.outcome === "lost",
    hasVerdict: (a.verdict ?? null) != null,
    hasFeedback: (a.feedback ?? null) != null,
  }));
}

/** A step's wall-clock span (first claim → terminal), in ms. */
export function stepDuration(step: StepView): number | undefined {
  return durationMs(step.started_at, step.finished_at);
}

/** One claim/worker history row (DoD-3 renders both workers of a reclaim). */
export interface ClaimRow {
  attempt: number;
  claimId?: string;
  workerId?: string;
  /** Where the row came from: the durable attempt, or a live claim event. */
  source: "attempt" | "event";
  /** True when this claim was displaced by a takeover (the `lost` attempt). */
  displaced: boolean;
  eventType?: string;
}

/**
 * The claim/worker history: the step's attempts unioned with any live
 * `step_claimed`/`step_reclaimed` events not yet reflected in a refetched
 * attempt row. Ordered by attempt, then durable-before-live.
 */
export function claimHistory(step: StepView, events: EventEnvelope[]): ClaimRow[] {
  const rows: ClaimRow[] = [];
  const seen = new Set<number>();
  for (const a of step.attempts ?? []) {
    rows.push({
      attempt: a.attempt,
      claimId: a.claim_id,
      workerId: a.worker_id,
      source: "attempt",
      displaced: a.outcome === "lost",
    });
    seen.add(a.attempt);
  }
  for (const env of events) {
    if (env.step_id !== step.id) continue;
    if (env.type !== "step_claimed" && env.type !== "step_reclaimed") continue;
    const p = env.payload as { attempt?: number; claim_id?: string; worker_id?: string };
    const attempt = typeof p.attempt === "number" ? p.attempt : -1;
    if (seen.has(attempt)) continue;
    seen.add(attempt);
    rows.push({
      attempt,
      claimId: p.claim_id,
      workerId: p.worker_id,
      source: "event",
      displaced: env.type === "step_reclaimed",
      eventType: env.type,
    });
  }
  rows.sort((x, y) => x.attempt - y.attempt || (x.source === y.source ? 0 : x.source === "attempt" ? -1 : 1));
  return rows;
}

/** The distinct worker ids that claimed a step — the DoD-3 "both workers" set. */
export function workerIds(step: StepView, events: EventEnvelope[]): string[] {
  const ids = new Set<string>();
  for (const r of claimHistory(step, events)) if (r.workerId) ids.add(r.workerId);
  return [...ids];
}

/** The model chain for an llm-family step: authored → downgrades → served. */
export interface ModelHistory {
  authored?: string;
  served?: string;
  downgrades: {
    fromModel: string;
    toModel: string;
    fromResource: string;
    toResource: string;
    trigger: string;
    attempt?: number;
  }[];
}

export function modelHistory(step: StepView, events: EventEnvelope[]): ModelHistory {
  const cfg = parse<LlmConfigShape>(step.config);
  const out = parse<{ model?: string }>(step.output);
  const downgrades: ModelHistory["downgrades"] = [];
  for (const env of events) {
    if (env.step_id !== step.id || env.type !== "model_downgraded") continue;
    const p = env.payload;
    downgrades.push({
      fromModel: p.from_model,
      toModel: p.to_model,
      fromResource: p.from_resource,
      toResource: p.to_resource,
      trigger: p.trigger,
      attempt: p.attempt,
    });
  }
  return { authored: cfg?.model, served: out?.model ?? cfg?.model, downgrades };
}

/**
 * Reconstruct an attempt's *effective* prompt, mirroring the engine's
 * `LLMExecutor.WithFeedback` (ADR-013, ticket 11.4): the feedback text is
 * appended to the authored prompt (or as a trailing user message). The base is
 * the *authored* (unrendered) prompt — identical across a step's semantic
 * attempts — so the diff between two attempts is exactly the feedback
 * augmentation, which is what the killer demo shows.
 */
export interface EffectivePrompt {
  attempt: number;
  /** The single-string prompt view (messages are joined for display/diff). */
  text: string;
  feedbackText?: string;
}

function promptBase(cfg: LlmConfigShape | undefined): string {
  if (!cfg) return "";
  if (cfg.messages && cfg.messages.length > 0) {
    return cfg.messages.map((m) => `${m.role}: ${m.content}`).join("\n");
  }
  return cfg.prompt ?? "";
}

export function effectivePrompts(step: StepView): EffectivePrompt[] {
  const cfg = parse<LlmConfigShape>(step.config);
  const base = promptBase(cfg);
  const messagesForm = !!(cfg?.messages && cfg.messages.length > 0);
  return (step.attempts ?? []).map((a) => {
    const fb = parse<FeedbackShape>(a.feedback);
    const fbText = fb?.text?.trim() ? fb.text : undefined;
    let text = base;
    if (fbText) {
      text = messagesForm ? `${base}\nuser: ${fbText}` : base === "" ? fbText : `${base}\n\n${fbText}`;
    }
    return { attempt: a.attempt, text, feedbackText: fbText };
  });
}

/** The diff between two attempts' effective prompts (default: consecutive). */
export function promptDiff(step: StepView, aAttempt: number, bAttempt: number): DiffHunk[] {
  const prompts = effectivePrompts(step);
  const a = prompts.find((p) => p.attempt === aAttempt)?.text ?? "";
  const b = prompts.find((p) => p.attempt === bAttempt)?.text ?? "";
  return diffLines(a, b);
}

/** A verdict row (per attempt) for the Validation tab. */
export interface VerdictRow {
  attempt: number;
  outcome?: string;
  status?: string;
  score?: number | null;
  issues: { validator?: string; code?: string; path?: string; message?: string }[];
  results: { validator: string; status: string; score?: number | null; rationale?: string }[];
}

export function verdictRows(step: StepView): VerdictRow[] {
  const rows: VerdictRow[] = [];
  for (const a of step.attempts ?? []) {
    const v = parse<{
      status?: string;
      score?: number | null;
      issues?: VerdictRow["issues"];
      results?: VerdictRow["results"];
    }>(a.verdict);
    if (!v) continue;
    rows.push({
      attempt: a.attempt,
      outcome: a.outcome,
      status: v.status,
      score: v.score ?? null,
      issues: v.issues ?? [],
      results: v.results ?? [],
    });
  }
  return rows;
}

/** Whether a step carried any validation chain (drives the tab's visibility). */
export function hasValidation(step: StepView): boolean {
  return (step.attempts ?? []).some((a) => (a.verdict ?? null) != null) || (step.validation ?? null) != null;
}

/** Whether the diff view is meaningful (≥2 attempts, at least one with feedback). */
export function hasPromptDiff(step: StepView): boolean {
  const attempts = step.attempts ?? [];
  return attempts.length >= 2 && attempts.some((a) => (a.feedback ?? null) != null);
}
