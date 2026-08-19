import type { EventEnvelope, EventType } from "@agentloom/engine-client";
import type { RunView, StepView } from "@agentloom/api-client";

export function makeRun(overrides: Partial<RunView> = {}): RunView {
  return {
    id: "run-1",
    status: "running",
    on_failure: "fail_fast",
    steps_total: 3,
    steps_succeeded: 0,
    steps_failed: 0,
    steps_skipped: 0,
    steps_cancelled: 0,
    steps_collected: 0,
    created_at: "2026-08-19T00:00:00Z",
    event_seq: 0,
    cost: { spent_nano_usd: 0, saved_nano_usd: 0, spent_usd: "0", saved_usd: "0", on_budget_exceeded: "park" },
    ...overrides,
  };
}

export function makeStep(overrides: Partial<StepView> = {}): StepView {
  return {
    id: "a",
    type: "echo",
    status: "pending",
    remaining_deps: 0,
    fired_deps: 0,
    attempt_count: 0,
    transport_failures: 0,
    validation_failures: 0,
    ...overrides,
  };
}

// Minimal envelope builder — the payload is loosely typed here (tests supply
// only the fields the reducer under test reads).
export function makeEnv(
  type: EventType,
  seq: number,
  payload: Record<string, unknown> = {},
  stepId?: string,
): EventEnvelope {
  return {
    schema_version: 1,
    run_id: "run-1",
    seq,
    ts: "2026-08-19T00:00:01Z",
    ...(stepId ? { step_id: stepId } : {}),
    type,
    payload,
  } as unknown as EventEnvelope;
}
