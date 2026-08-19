import { describe, expect, it } from "vitest";
import type { SystemStatsResponse } from "@agentloom/api-client";
import { deriveStaleness, deriveStatTiles } from "@/lib/pure/dashboard/system-stats";
import { systemStatsFixture } from "./inspector-fixtures";

describe("deriveStatTiles (golden)", () => {
  const tiles = deriveStatTiles(systemStatsFixture);
  const byKey = Object.fromEntries(tiles.map((t) => [t.key, t]));

  it("has the queue + Postgres tiles", () => {
    expect(byKey.ready!.value).toBe(42);
    expect(byKey.pending!.value).toBe(7);
    expect(byKey.delayed!.value).toBe(3);
    expect(byKey.workers!.value).toBe("2/3");
    expect(byKey.dead_letters!.value).toBe(2);
    expect(byKey.outbox!.value).toBe(5);
    expect(byKey.runs!.value).toBe(11);
  });

  it("tiers dead letters as warn (>=1)", () => {
    expect(byKey.dead_letters!.tier).toBe("warn");
  });

  it("flags zero active workers as danger", () => {
    const noWorkers: SystemStatsResponse = {
      ...systemStatsFixture,
      queue: { ...systemStatsFixture.queue!, workers_active: 0 },
    };
    const t = deriveStatTiles(noWorkers).find((x) => x.key === "workers")!;
    expect(t.tier).toBe("danger");
  });

  it("omits queue tiles when queue is null", () => {
    const noQueue: SystemStatsResponse = { ...systemStatsFixture, queue: null };
    const keys = deriveStatTiles(noQueue).map((t) => t.key);
    expect(keys).not.toContain("ready");
    expect(keys).toContain("dead_letters");
    expect(keys).toContain("runs");
  });
});

describe("deriveStaleness", () => {
  it("labels a fresh snapshot", () => {
    const observed = Date.parse(systemStatsFixture.observed_at);
    const s = deriveStaleness(systemStatsFixture, observed + 2000, 5000);
    expect(s.stale).toBe(false);
    expect(s.label).toBe("2s ago");
  });
  it("flags stale beyond 3× the interval", () => {
    const observed = Date.parse(systemStatsFixture.observed_at);
    const s = deriveStaleness(systemStatsFixture, observed + 20000, 5000);
    expect(s.stale).toBe(true);
  });
});
