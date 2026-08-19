import { describe, expect, it } from "vitest";
import type { EventEnvelope } from "@agentloom/engine-client";
import {
  applyDlqEvent,
  dlqFiltersFromSearchParams,
  dlqFiltersToQuery,
  dlqFiltersToSearchParams,
  dlqKey,
  matchesDlqFilters,
  upsertDlqRow,
  type DlqRow,
} from "@/lib/pure/dashboard/dead-letters";
import { deadLetterListFixture } from "./inspector-fixtures";

// Fold the Go golden into rows — the exact GET /v1/dead-letters wire shape.
const rows: DlqRow[] = deadLetterListFixture.dead_letters.map((d) => ({ ...d, lastSeq: 0 }));

const env = (type: EventEnvelope["type"], seq: number, runId: string, stepId: string): EventEnvelope =>
  ({ schema_version: 1, run_id: runId, seq, ts: "t", step_id: stepId, type, payload: { step_id: stepId } }) as unknown as EventEnvelope;

describe("golden fold", () => {
  it("carries live status + open from the wire", () => {
    expect(rows).toHaveLength(3);
    const flaky = rows.find((r) => r.step_id === "flaky")!;
    expect(flaky.open).toBe(true);
    expect(flaky.source).toBe("retries_exhausted");
    expect(flaky.step_status).toBe("dead_lettered");
    const fetch = rows.find((r) => r.step_id === "fetch")!;
    expect(fetch.open).toBe(false); // succeeded after a requeue
    expect(fetch.class).toBeUndefined(); // poison
  });
});

describe("applyDlqEvent", () => {
  it("closes a row on step_requeued", () => {
    const flaky = rows.find((r) => r.step_id === "flaky")!;
    const next = applyDlqEvent(flaky, env("step_requeued", 10, flaky.run_id, "flaky"));
    expect(next.open).toBe(false);
    expect(next.step_status).toBe("ready");
  });
  it("keeps a row open on a repeated step_dead_lettered", () => {
    const flaky = rows.find((r) => r.step_id === "flaky")!;
    const next = applyDlqEvent(flaky, env("step_dead_lettered", 11, flaky.run_id, "flaky"));
    expect(next.open).toBe(true);
  });
  it("ignores an event for a different step or run", () => {
    const flaky = rows.find((r) => r.step_id === "flaky")!;
    expect(applyDlqEvent(flaky, env("step_requeued", 12, flaky.run_id, "other"))).toBe(flaky);
    expect(applyDlqEvent(flaky, env("step_requeued", 12, "other-run", "flaky"))).toBe(flaky);
  });
  it("is idempotent under the seq guard", () => {
    const flaky = { ...rows.find((r) => r.step_id === "flaky")!, lastSeq: 20 };
    expect(applyDlqEvent(flaky, env("step_requeued", 20, flaky.run_id, "flaky"))).toBe(flaky);
  });
});

describe("filters", () => {
  it("open filter keeps only open rows", () => {
    expect(matchesDlqFilters(rows[0]!, { status: "open" })).toBe(true); // flaky
    expect(matchesDlqFilters(rows[1]!, { status: "open" })).toBe(false); // fetch (closed)
    expect(matchesDlqFilters(rows[1]!, { status: "all" })).toBe(true);
  });
  it("run and source filters", () => {
    expect(matchesDlqFilters(rows[0]!, { status: "all", source: "permanent" })).toBe(false);
    expect(matchesDlqFilters(rows[0]!, { status: "all", source: "retries_exhausted" })).toBe(true);
    expect(matchesDlqFilters(rows[0]!, { status: "all", runId: "nope" })).toBe(false);
  });
});

describe("query + URL codec", () => {
  it("query defaults status", () => {
    expect(dlqFiltersToQuery({ status: "open" })).toEqual({ status: "open" });
    expect(dlqFiltersToQuery({ status: "all", runId: "r", source: "poison" })).toEqual({
      status: "all",
      run_id: "r",
      source: "poison",
    });
  });
  it("URL round-trips (open is the implicit default)", () => {
    const f = { status: "all", runId: "r1", source: "permanent" } as const;
    const p = dlqFiltersToSearchParams(f);
    expect(dlqFiltersFromSearchParams(p)).toEqual(f);
    // Empty params ⇒ open, no filters.
    expect(dlqFiltersFromSearchParams(new URLSearchParams())).toEqual({ status: "open" });
  });
});

describe("upsertDlqRow", () => {
  it("sorts newest-first and replaces by key", () => {
    let list: DlqRow[] = [];
    for (const r of rows) list = upsertDlqRow(list, r);
    // Newest created_at first (flaky t+2 > publish t+0).
    expect(list[0]!.step_id).toBe("flaky");
    // Re-insert flaky (mutated) → still one entry, still first.
    list = upsertDlqRow(list, { ...list[0]!, open: false });
    expect(list.filter((r) => dlqKey(r) === dlqKey(rows[0]!))).toHaveLength(1);
  });
});
