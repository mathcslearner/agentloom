// Field hints for the config form (ticket 17.4): the small hand-maintained
// layer the JSON Schema cannot supply — which fields are required (the backend
// encodes required-ness only in internal/dag/validate.go, not the schema),
// which fields want a specialized widget (model picker, tool/retriever/agent
// picker, prompt editor, duration input), and which are engine-populated and
// hidden. Keyed by (step type, field name). Pure — no React.

import type { StepType } from "@agentloom/graphdef";

/** A specialized widget hint for a config field. */
export type FieldHint = "model" | "model-list" | "tool" | "retriever" | "agent" | "template" | "prompt" | "duration" | "hidden";

// Required top-level config fields per step type, mirroring checkStepConfig.
// (llm/planner also require exactly-one-of prompt|messages — a cross-field rule
// the validator owns; not a single-field asterisk.)
const REQUIRED: Partial<Record<StepType, readonly string[]>> = {
  llm: ["model"],
  planner: ["model"],
  agent: ["agent"],
  retrieve: ["retriever", "query"],
  map: ["items", "body"],
  join: ["mode"],
  human_approval: ["title"],
  tool: ["tool"],
  sleep: ["duration"],
  fail_n_times: ["n"],
  counter: ["path"],
  effectful_echo: ["path"],
  blackboard_write: ["key", "value"],
};

/** True if a top-level config field is required for the step type. */
export function isRequiredField(type: StepType, field: string): boolean {
  return (REQUIRED[type] ?? []).includes(field);
}

// Specialized widget hints keyed by (step type, field name).
const HINTS: Partial<Record<StepType, Record<string, FieldHint>>> = {
  llm: { model: "model", prompt: "prompt", system: "prompt" },
  planner: { model: "model", prompt: "prompt" },
  agent: { agent: "agent", model: "model", prompt: "prompt", system: "prompt", role: "hidden" },
  retrieve: { retriever: "retriever", query: "prompt" },
  map: { body: "template" },
  tool: { tool: "tool" },
  human_approval: { title: "prompt", description: "prompt", timeout: "duration" },
  sleep: { duration: "duration" },
};

/** The widget hint for a config field, if any. */
export function fieldHint(type: StepType, field: string): FieldHint | undefined {
  return HINTS[type]?.[field];
}

// Field names that hold free prose and benefit from a multi-line, autocomplete-
// aware editor regardless of step type (e.g. an llm message's content).
const PROMPTISH = new Set(["content", "rubric", "expr", "text", "description"]);

/** True if a field (by name) is prose worth a prompt editor. */
export function isPromptField(name: string): boolean {
  return PROMPTISH.has(name);
}

/** Humanize a snake_case field name for a form label. */
export function humanizeField(name: string): string {
  return name
    .split("_")
    .map((w) => (w.length > 0 ? w[0]!.toUpperCase() + w.slice(1) : w))
    .join(" ");
}
