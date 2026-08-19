import { describe, expect, it } from "vitest";
import { EVENT_TYPES } from "@agentloom/engine-client";
import { eventCategory, EVENT_CATEGORIES, summarizeEvent } from "@/lib/pure/dashboard/events";
import { makeEnv } from "./helpers";

describe("eventCategory", () => {
  it("assigns every event type a known category", () => {
    for (const t of EVENT_TYPES) {
      const c = eventCategory(t);
      expect(EVENT_CATEGORIES).toContain(c);
    }
  });
});

describe("summarizeEvent", () => {
  it("produces a non-empty summary for every event type", () => {
    for (const t of EVENT_TYPES) {
      // A generic payload; the summary must never throw or be empty.
      const env = makeEnv(
        t,
        1,
        {
          name: "wf",
          steps_total: 2,
          step_id: "a",
          attempt: 1,
          class: "transient",
          resource: "mock:sim-1",
          reason: "manual",
          source: "retries_exhausted",
          semantic_attempt: 1,
          max_attempts: 3,
          issue_count: 2,
          cost_nano_usd: 1000,
          run_spent_nano_usd: 5000,
          model: "gpt",
          from_model: "a",
          to_model: "b",
          action: "reject",
          key: "k",
          version: 1,
          context_tokens: 10,
          strategy: "sliding_window",
          tokens_before: 10,
          tokens_after: 5,
          origin_step: "plan",
          origin_kind: "planner",
          to_version: 2,
          delta: { schema_version: 1, steps: [] },
          loop_source_step: "loop",
          guard: "max_wall_clock",
          current: 1,
          cap: 2,
          unit: "steps",
          title: "Approve",
          decision: "approve",
          decided_by: "root",
          status_code: 200,
        },
        "a",
      );
      const s = summarizeEvent(env);
      expect(s.length).toBeGreaterThan(0);
    }
  });

  it("summarizes a step_succeeded with its step + attempt", () => {
    const env = makeEnv("step_succeeded", 4, { step_id: "draft", attempt: 2 }, "draft");
    expect(summarizeEvent(env)).toContain("draft");
    expect(summarizeEvent(env)).toContain("2");
  });
});
