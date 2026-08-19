// Template-reference autocomplete (ticket 17.4): the data behind the prompt
// editor's `${{ … }}` suggestions. Pure — no React, no store. The upstream
// predicate is the exact mirror of the backend's classifyUpstreamRef /
// Graph.Ancestors (internal/dag): ancestors over NORMAL edges only (loop edges
// confer no ancestry), a step is never its own ancestor. So a wrong-direction
// reference is not offered — the DoD-3 guarantee.

import type { StepType } from "@agentloom/graphdef";

/** A minimal node view for reachability. */
export interface RefNode {
  id: string;
  type: StepType;
}

/** A minimal edge view: `loop` marks an M1 loop edge (excluded from ancestry). */
export interface RefEdge {
  from: string;
  to: string;
  loop: boolean;
}

/**
 * The ids of every step upstream of `targetId` via normal-edge paths, sorted.
 * Mirrors dag.Graph.Ancestors: BFS over incoming normal edges, self excluded.
 */
export function upstreamStepIds(nodes: readonly RefNode[], edges: readonly RefEdge[], targetId: string): string[] {
  const incoming = new Map<string, string[]>();
  for (const e of edges) {
    if (e.loop) continue; // loop edges confer no ancestry
    const list = incoming.get(e.to) ?? [];
    list.push(e.from);
    incoming.set(e.to, list);
  }
  const seen = new Set<string>();
  const queue = [...(incoming.get(targetId) ?? [])];
  while (queue.length > 0) {
    const id = queue.shift() as string;
    if (id === targetId || seen.has(id)) continue;
    seen.add(id);
    for (const parent of incoming.get(id) ?? []) {
      if (!seen.has(parent)) queue.push(parent);
    }
  }
  return [...seen].sort();
}

/**
 * The statically-known first-level output field names for a step type (the
 * executor output shapes, internal/exec). Deeper paths (`.json.*`, tool results,
 * `.results[].*`) are genuinely dynamic, so the editor offers these as the first
 * level and allows free-text continuation.
 */
export function outputPaths(type: StepType): string[] {
  switch (type) {
    case "llm":
    case "planner":
    case "agent":
      return ["text", "json", "model", "stop_reason", "tool_calls", "usage"];
    case "retrieve":
      return ["retriever", "query", "top_k", "results"];
    case "map":
      return ["items", "indices", "count"];
    case "human_approval":
      return ["decision", "payload", "approval_id", "comment", "decided_by", "decided_at", "source", "edited"];
    case "sleep":
      return ["slept"];
    case "counter":
      return ["counted", "attempt"];
    case "fail_n_times":
      return ["succeeded_on_attempt"];
    case "blackboard_write":
      return ["wrote", "read"];
    // tool (verbatim), gather (bare array), echo/branch/effectful_echo
    // (passthrough), noop, join — no statically-known field names.
    default:
      return [];
  }
}

/** Declared run-parameter names from the document, sorted. */
export function paramNames(doc: Record<string, unknown>): string[] {
  const params = doc["params"];
  if (typeof params !== "object" || params === null || Array.isArray(params)) return [];
  return Object.keys(params as Record<string, unknown>).sort();
}

/** A single autocomplete suggestion. */
export interface Suggestion {
  /** The text to insert (the full reference, e.g. `steps.a.output.text`). */
  value: string;
  /** A short label (the field/step name). */
  label: string;
  /** A one-word kind for grouping/icons. */
  kind: "step" | "output" | "param" | "root";
}

/** The active `${{ … }}` fragment at the caret, or null if the caret is outside one. */
export function activeExpression(text: string, caret: number): { fragment: string; start: number } | null {
  const open = text.lastIndexOf("${{", caret);
  if (open === -1) return null;
  const close = text.indexOf("}}", open);
  if (close !== -1 && close < caret) return null; // caret is past a closed expression
  const start = open + 3;
  return { fragment: text.slice(start, caret), start };
}

/**
 * Suggestions for the fragment currently under the caret. Given the fragment's
 * last token, offer roots, upstream step refs, output paths, or run params.
 */
export function suggestionsFor(args: {
  fragment: string;
  nodes: readonly RefNode[];
  edges: readonly RefEdge[];
  doc: Record<string, unknown>;
  currentStepId: string;
}): Suggestion[] {
  const { fragment, nodes, edges, doc, currentStepId } = args;
  // The last whitespace/pipe-delimited token is what we are completing.
  const token = fragment.trimStart().split(/[\s|(]+/).pop() ?? "";
  const byId = new Map(nodes.map((n) => [n.id, n]));

  // steps.<id>.output.<path>
  const outputMatch = /^steps\.([a-z][a-z0-9_-]*)\.output\.?(\w*)$/.exec(token);
  if (outputMatch) {
    const stepId = outputMatch[1] as string;
    const node = byId.get(stepId);
    if (!node) return [];
    return outputPaths(node.type).map((p) => ({
      value: `steps.${stepId}.output.${p}`,
      label: p,
      kind: "output" as const,
    }));
  }

  // steps.<partial>
  const stepMatch = /^steps\.([a-z0-9_-]*)$/.exec(token);
  if (stepMatch) {
    const prefix = stepMatch[1] as string;
    return upstreamStepIds(nodes, edges, currentStepId)
      .filter((id) => id.startsWith(prefix))
      .map((id) => ({ value: `steps.${id}.output`, label: id, kind: "step" as const }));
  }

  // run.params.<partial>
  const paramMatch = /^run\.params\.?(\w*)$/.exec(token);
  if (paramMatch) {
    const prefix = paramMatch[1] as string;
    return paramNames(doc)
      .filter((k) => k.startsWith(prefix))
      .map((k) => ({ value: `run.params.${k}`, label: k, kind: "param" as const }));
  }

  // Roots (empty or partial root token).
  const roots: Suggestion[] = [
    { value: "steps.", label: "steps", kind: "root" },
    { value: "run.params.", label: "run.params", kind: "root" },
  ];
  return roots.filter((r) => r.label.startsWith(token) || token === "");
}
