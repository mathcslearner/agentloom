/**
 * Step-status skins for the live DAG (ticket 18.2). Maps a step's live status
 * (run-state.ts) onto the visual a node paints — a tone token, a header badge,
 * a run-oriented body summary, and whether it pulses (active) — and maps an
 * edge onto its resolution + whether it animates. Pure and exhaustive over
 * `DisplayStepStatus`: a new backend status is a typecheck error here until it
 * is given a skin.
 *
 * No React/UI imports — under the app's `pure/` eslint boundary.
 */
import type { DisplayStepStatus, StepState } from "./run-state";
import type { TopoEdge } from "./graph-topology";

/** A coarse tone family the node/badge palette keys on (mapped to CSS in the
 * component). */
export type SkinTone =
  | "neutral" // pending
  | "ready"
  | "active" // running
  | "success"
  | "danger" // failed / dead_lettered
  | "warn" // retrying / throttled
  | "attention" // awaiting_human
  | "muted"; // skipped / cancelled / collected

export interface SkinSpec {
  tone: SkinTone;
  /** Short header badge text (the status, condensed). */
  badge: string;
  /** Run-oriented one-line body summary (attempt/retry state). */
  summary: string;
  /** Whether the node animates as active (running). */
  pulse: boolean;
  /** An extra corner marker (e.g. a reclaim indicator), when applicable. */
  marker?: string;
}

const TONE: Record<DisplayStepStatus, SkinTone> = {
  pending: "neutral",
  ready: "ready",
  running: "active",
  succeeded: "success",
  failed: "danger",
  dead_lettered: "danger",
  retrying: "warn",
  throttled: "warn",
  awaiting_human: "attention",
  cancelled: "muted",
  skipped: "muted",
  collected: "muted",
};

const BADGE: Record<DisplayStepStatus, string> = {
  pending: "pending",
  ready: "ready",
  running: "running",
  succeeded: "done",
  failed: "failed",
  dead_lettered: "dead-lettered",
  retrying: "retrying",
  throttled: "throttled",
  awaiting_human: "awaiting decision",
  cancelled: "cancelled",
  skipped: "skipped",
  collected: "collected",
};

/** The visual skin for a step in its current live state. */
export function skinFor(step: StepState): SkinSpec {
  const status = step.status;
  const tone = TONE[status];
  const badge = BADGE[status];
  const pulse = status === "running";
  const marker = step.reclaims > 0 ? `↻ reclaimed ×${step.reclaims}` : undefined;
  return { tone, badge, summary: summaryFor(step), pulse, marker };
}

function summaryFor(step: StepState): string {
  const a = step.attempt;
  switch (step.status) {
    case "running":
      return a > 1 ? `attempt ${a}` : "running";
    case "retrying":
      return `retry scheduled${a > 0 ? ` (after attempt ${a})` : ""}`;
    case "throttled":
      return "throttled — waiting on capacity";
    case "awaiting_human":
      return "waiting on you";
    case "succeeded":
      return a > 1 ? `succeeded on attempt ${a}` : "succeeded";
    case "failed":
    case "dead_lettered":
      return a > 0 ? `failed after ${a} attempt${a === 1 ? "" : "s"}` : "failed";
    case "cancelled":
      return "cancelled";
    case "skipped":
      return "skipped (branch not taken)";
    case "collected":
      return "failure tolerated (collect_errors)";
    case "ready":
      return "ready to run";
    case "pending":
    default:
      return "waiting on dependencies";
  }
}

export interface EdgeVisual {
  resolution: "unresolved" | "fired" | "skipped";
  /** Whether the edge animates (an active hand-off: source just completed, the
   * target is running / ready). */
  active: boolean;
}

/**
 * The edge's live visual: its resolution (from the topology, refined by the
 * endpoints' live status so a snapshot-less edge still reads correctly) and
 * whether it is an active hand-off worth animating.
 */
export function edgeVisual(edge: TopoEdge, source?: StepState, target?: StepState): EdgeVisual {
  let resolution = edge.resolution;
  // Refine from live status when the stored resolution is stale/absent.
  if (resolution === "unresolved") {
    if (target?.status === "skipped") resolution = "skipped";
    else if (source && terminalSucceeded(source.status) && target && started(target.status)) resolution = "fired";
  }
  const active =
    !!source &&
    terminalSucceeded(source.status) &&
    !!target &&
    (target.status === "running" || target.status === "ready");
  return { resolution, active };
}

function terminalSucceeded(s: DisplayStepStatus): boolean {
  return s === "succeeded" || s === "collected";
}

function started(s: DisplayStepStatus): boolean {
  return s === "running" || s === "succeeded" || s === "failed" || s === "dead_lettered" || s === "collected";
}
