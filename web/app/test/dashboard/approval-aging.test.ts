import { describe, expect, it } from "vitest";
import { deriveAging, deriveDeadline, humanizeDuration } from "@/lib/pure/dashboard/approval-aging";
import type { ApprovalRecord } from "@/lib/pure/dashboard/approvals";

const HOUR = 3_600_000;
const base: ApprovalRecord = {
  id: "a", run_id: "r", step_id: "gate", attempt: 1, status: "pending",
  title: "t", allowed_decisions: ["approve"], created_at: "2026-08-18T09:00:00Z", lastSeq: 1, partial: false,
};
const created = Date.parse(base.created_at);

describe("deriveAging", () => {
  it("tiers fresh / aging / stale", () => {
    expect(deriveAging(base, created + 30 * 60_000).tier).toBe("fresh");
    expect(deriveAging(base, created + 2 * HOUR).tier).toBe("aging");
    expect(deriveAging(base, created + 30 * HOUR).tier).toBe("stale");
  });

  it("freezes age at the decision time when decided", () => {
    const decided: ApprovalRecord = { ...base, status: "approved", decided_at: "2026-08-18T09:05:00Z" };
    const a = deriveAging(decided, created + 100 * HOUR);
    expect(a.ageMs).toBe(5 * 60_000);
  });
});

describe("deriveDeadline", () => {
  it("counts down and flags overdue", () => {
    const rec: ApprovalRecord = { ...base, timeout_at: "2026-08-18T10:00:00Z" };
    const at = Date.parse(rec.timeout_at!);
    expect(deriveDeadline(rec, at - HOUR)!.overdue).toBe(false);
    expect(deriveDeadline(rec, at + HOUR)!.overdue).toBe(true);
  });

  it("returns undefined without a timeout", () => {
    expect(deriveDeadline(base, created)).toBeUndefined();
  });
});

describe("humanizeDuration", () => {
  it("formats compactly", () => {
    expect(humanizeDuration(500)).toBe("just now");
    expect(humanizeDuration(90_000)).toBe("1m");
    expect(humanizeDuration(2 * HOUR)).toBe("2h");
    expect(humanizeDuration(50 * HOUR)).toBe("2d");
  });
});
