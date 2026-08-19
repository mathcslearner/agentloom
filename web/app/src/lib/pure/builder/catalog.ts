// The step-type catalog — display metadata for every catalog step type
// (ADR-003 / ADR-019). Pure data + summary functions; no React, no canvas
// coupling. Consumed by the palette (grouping + labels) and the node component
// (label + one-line config summary). Kept exhaustive over `StepType` so a new
// backend step type is a compile error here until it is given a card.

import type { Step, StepType } from "@agentloom/graphdef";

/** Which palette group a step type belongs to. */
export type StepGroup = "core" | "test";

export interface StepTypeMeta {
  type: StepType;
  /** Human label shown in the palette and node header. */
  label: string;
  /** Palette group: the nine authoring types vs. the test/dev step types. */
  group: StepGroup;
  /** One-line summary of a step's config for the node body (never throws). */
  summary: (step: Step) => string;
}

// A defensive reader: step.config is a discriminated union keyed by step.type,
// but a Flow may carry a well-shaped-but-invalid config (ADR-019: toFlow does
// not validate), so summaries read fields loosely and never assume presence.
function cfg(step: Step): Record<string, unknown> {
  const c = (step as { config?: unknown }).config;
  return c && typeof c === "object" ? (c as Record<string, unknown>) : {};
}

function str(v: unknown): string | undefined {
  return typeof v === "string" && v !== "" ? v : undefined;
}

/** The catalog, keyed by step type. Exhaustive over `StepType`. */
export const STEP_CATALOG: Record<StepType, StepTypeMeta> = {
  llm: {
    type: "llm",
    label: "LLM",
    group: "core",
    summary: (s) => str(cfg(s)["model"]) ?? "model call",
  },
  tool: {
    type: "tool",
    label: "Tool",
    group: "core",
    summary: (s) => str(cfg(s)["tool"]) ?? "tool call",
  },
  retrieve: {
    type: "retrieve",
    label: "Retrieve",
    group: "core",
    summary: (s) => str(cfg(s)["retriever"]) ?? "retrieval",
  },
  map: {
    type: "map",
    label: "Map",
    group: "core",
    summary: (s) => {
      const body = str(cfg(s)["body"]);
      return body ? `over ${body}` : "fan-out";
    },
  },
  gather: {
    type: "gather",
    label: "Gather",
    group: "core",
    summary: () => "fan-in barrier",
  },
  planner: {
    type: "planner",
    label: "Planner",
    group: "core",
    summary: (s) => str(cfg(s)["model"]) ?? "dynamic plan",
  },
  agent: {
    type: "agent",
    label: "Agent",
    group: "core",
    summary: (s) => {
      const role = str(cfg(s)["agent"]);
      return role ? `role ${role}` : "agent turn";
    },
  },
  human_approval: {
    type: "human_approval",
    label: "Approval",
    group: "core",
    summary: (s) => str(cfg(s)["title"]) ?? "human gate",
  },
  join: {
    type: "join",
    label: "Join",
    group: "core",
    summary: (s) => {
      const mode = str(cfg(s)["mode"]);
      return mode ? `${mode} join` : "synchronization";
    },
  },
  branch: {
    type: "branch",
    label: "Branch",
    group: "core",
    summary: () => "exclusive routing",
  },
  noop: { type: "noop", label: "No-op", group: "test", summary: () => "no operation" },
  echo: { type: "echo", label: "Echo", group: "test", summary: () => "echo input" },
  sleep: {
    type: "sleep",
    label: "Sleep",
    group: "test",
    summary: (s) => str(cfg(s)["duration"]) ?? "delay",
  },
  fail_n_times: {
    type: "fail_n_times",
    label: "Fail N×",
    group: "test",
    summary: (s) => {
      const n = cfg(s)["n"];
      return typeof n === "number" ? `fail ${n}×` : "fail then succeed";
    },
  },
  counter: { type: "counter", label: "Counter", group: "test", summary: () => "append-only ledger" },
  effectful_echo: {
    type: "effectful_echo",
    label: "Effectful echo",
    group: "test",
    summary: () => "journaled echo",
  },
  blackboard_write: {
    type: "blackboard_write",
    label: "Blackboard write",
    group: "test",
    summary: (s) => {
      const key = str(cfg(s)["key"]);
      return key ? `write ${key}` : "blackboard write";
    },
  },
};

/** Metadata for a step type. */
export function stepMeta(type: StepType): StepTypeMeta {
  // Defensive: the dashboard (M18) renders whatever step type the wire carries,
  // so an unknown type (a future backend step type) degrades to a neutral card
  // rather than crashing.
  return (
    STEP_CATALOG[type] ?? {
      type,
      label: String(type),
      group: "core",
      summary: () => String(type),
    }
  );
}

/** The nine authoring step types, in catalog order. */
export const CORE_TYPES: readonly StepType[] = (
  ["llm", "tool", "retrieve", "map", "gather", "planner", "agent", "human_approval", "join", "branch"] as const
).filter((t) => STEP_CATALOG[t].group === "core");

/** The test/dev step types, in catalog order. */
export const TEST_TYPES: readonly StepType[] = (
  ["noop", "echo", "sleep", "fail_n_times", "counter", "effectful_echo", "blackboard_write"] as const
).filter((t) => STEP_CATALOG[t].group === "test");
