import { describe, expect, it } from "vitest";
import {
  applyInboxEvent,
  inboxFiltersFromSearchParams,
  inboxFiltersToSearchParams,
  matchesInboxFilters,
  upsertRow,
  type InboxRow,
} from "@/lib/pure/dashboard/approval-list";
import { makeEnv } from "./helpers";

const row = (over: Partial<InboxRow> = {}): InboxRow => ({
  id: "a", run_id: "r", step_id: "gate", attempt: 1, status: "pending",
  title: "t", allowed_decisions: ["approve", "reject"], created_at: "2026-08-18T09:00:00Z", lastSeq: 0, ...over,
});

describe("applyInboxEvent", () => {
  it("folds a decision absolutely", () => {
    const r = applyInboxEvent(row(), makeEnv("approval_decided", 3, { approval_id: "a", step_id: "gate", attempt: 1, decision: "reject", decided_by: "u", source: "human" }, "gate"));
    expect(r.status).toBe("rejected");
    expect(r.lastSeq).toBe(3);
  });

  it("is a no-op for a stale seq", () => {
    const base = row({ lastSeq: 5 });
    expect(applyInboxEvent(base, makeEnv("approval_decided", 3, { approval_id: "a", decision: "approve", decided_by: "u", source: "human" }, "gate"))).toBe(base);
  });

  it("park-expired stays pending", () => {
    const r = applyInboxEvent(row(), makeEnv("approval_expired", 2, { approval_id: "a", step_id: "gate", attempt: 1, policy: "park", action: "run_parked" }, "gate"));
    expect(r.status).toBe("pending");
    expect(r.expired_at).toBeDefined();
  });
});

describe("filters codec", () => {
  it("defaults to pending when status absent", () => {
    expect(inboxFiltersFromSearchParams(new URLSearchParams()).status).toBe("pending");
  });
  it("all clears the status filter", () => {
    expect(inboxFiltersFromSearchParams(new URLSearchParams("status=all")).status).toBeUndefined();
  });
  it("round-trips run + status", () => {
    const p = inboxFiltersToSearchParams({ status: "approved", runId: "r1" });
    const f = inboxFiltersFromSearchParams(p);
    expect(f).toEqual({ status: "approved", runId: "r1" });
  });
  it("matchesInboxFilters honors status + run", () => {
    expect(matchesInboxFilters(row(), { status: "pending" })).toBe(true);
    expect(matchesInboxFilters(row(), { status: "approved" })).toBe(false);
    expect(matchesInboxFilters(row({ run_id: "x" }), { runId: "y" })).toBe(false);
  });
});

describe("upsertRow", () => {
  it("keeps oldest-first order and replaces by id", () => {
    let rows: InboxRow[] = [];
    rows = upsertRow(rows, row({ id: "b", created_at: "2026-08-18T09:02:00Z" }));
    rows = upsertRow(rows, row({ id: "a", created_at: "2026-08-18T09:01:00Z" }));
    rows = upsertRow(rows, row({ id: "a", created_at: "2026-08-18T09:01:00Z", status: "approved" }));
    expect(rows.map((r) => r.id)).toEqual(["a", "b"]);
    expect(rows[0]!.status).toBe("approved");
  });
});
