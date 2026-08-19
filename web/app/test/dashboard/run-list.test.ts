import { describe, expect, it } from "vitest";
import {
  applyRunListEvent,
  filtersFromSearchParams,
  filtersToQuery,
  filtersToSearchParams,
  matchesFilters,
  presetCutoff,
} from "@/lib/pure/dashboard/run-list";
import { makeEnv, makeRun } from "./helpers";

describe("applyRunListEvent", () => {
  it("ignores an event at or below the row cursor", () => {
    const row = makeRun({ event_seq: 5 });
    expect(applyRunListEvent(row, makeEnv("step_succeeded", 5, {}, "a"))).toBe(row);
  });

  it("increments counters on terminal step events (once per seq)", () => {
    let row = makeRun({ event_seq: 0 });
    row = applyRunListEvent(row, makeEnv("step_succeeded", 1, {}, "a"));
    expect(row.steps_succeeded).toBe(1);
    // A redelivery of the same seq does not double-count.
    row = applyRunListEvent(row, makeEnv("step_succeeded", 1, {}, "a"));
    expect(row.steps_succeeded).toBe(1);
    row = applyRunListEvent(row, makeEnv("step_dead_lettered", 2, {}, "b"));
    expect(row.steps_failed).toBe(1);
  });

  it("maps run lifecycle onto status", () => {
    let row = makeRun({ event_seq: 0 });
    row = applyRunListEvent(row, makeEnv("run_succeeded", 3, {}));
    expect(row.status).toBe("succeeded");
    expect(row.event_seq).toBe(3);
  });

  it("grows steps_total on a graph expansion", () => {
    const row = makeRun({ event_seq: 0, steps_total: 3 });
    const next = applyRunListEvent(
      row,
      makeEnv("graph_expanded", 1, {
        delta: { schema_version: 1, steps: [{ id: "x", type: "llm" }, { id: "y", type: "llm" }] },
      }),
    );
    expect(next.steps_total).toBe(5);
  });
});

describe("matchesFilters", () => {
  it("applies status/definition/time predicates", () => {
    const row = makeRun({ status: "running", definition_id: "def-1", created_at: "2026-08-19T12:00:00Z" });
    expect(matchesFilters(row, { status: "running" })).toBe(true);
    expect(matchesFilters(row, { status: "failed" })).toBe(false);
    expect(matchesFilters(row, { definitionId: "def-1" })).toBe(true);
    expect(matchesFilters(row, { definitionId: "other" })).toBe(false);
    expect(matchesFilters(row, { createdAfter: "2026-08-19T11:00:00Z" })).toBe(true);
    expect(matchesFilters(row, { createdAfter: "2026-08-19T13:00:00Z" })).toBe(false);
  });
});

describe("filter URL codec", () => {
  it("round-trips through search params", () => {
    const f = { status: "failed" as const, definitionId: "def-9", createdAfter: "2026-08-19T00:00:00.000Z" };
    const p = filtersToSearchParams(f);
    expect(filtersFromSearchParams(p)).toEqual(f);
  });

  it("maps to the GET /v1/runs query shape", () => {
    expect(filtersToQuery({ status: "running", definitionId: "d" })).toEqual({
      status: "running",
      definition_id: "d",
    });
  });

  it("computes a preset cutoff relative to now", () => {
    const now = Date.parse("2026-08-19T12:00:00Z");
    expect(presetCutoff("all", now)).toBeUndefined();
    expect(presetCutoff("1h", now)).toBe("2026-08-19T11:00:00.000Z");
  });
});
