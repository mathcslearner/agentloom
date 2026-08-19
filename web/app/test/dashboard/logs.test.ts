import { describe, expect, it } from "vitest";
import { appendLogPage, emptyLogState, filterByLevel } from "@/lib/pure/dashboard/logs";
import { stepLogsFixture } from "./inspector-fixtures";
import type { StepLogsResponse } from "@agentloom/api-client";

describe("log page accumulation", () => {
  it("merges the golden page and carries truncation", () => {
    const s = appendLogPage(emptyLogState(), stepLogsFixture);
    expect(s.lines.map((l) => l.seq)).toEqual([1, 2, 4]);
    expect(s.truncated).toBe(true);
    expect(s.droppedLines).toBe(1);
    expect(s.attempt).toBe(2);
  });

  it("dedupes by seq across pages and stays sorted", () => {
    let s = appendLogPage(emptyLogState(), {
      run_id: "r", step_id: "a", attempt: 1, lines: [{ seq: 1, level: "info", message: "x", logged_at: "t" }], truncated: false,
    } as StepLogsResponse);
    s = appendLogPage(s, {
      run_id: "r", step_id: "a", attempt: 1,
      lines: [
        { seq: 1, level: "info", message: "x", logged_at: "t" },
        { seq: 2, level: "warn", message: "y", logged_at: "t" },
      ],
      truncated: false,
    } as StepLogsResponse);
    expect(s.lines.map((l) => l.seq)).toEqual([1, 2]);
  });

  it("resets accumulation when the attempt changes", () => {
    let s = appendLogPage(emptyLogState(), {
      run_id: "r", step_id: "a", attempt: 1, lines: [{ seq: 5, level: "info", message: "old", logged_at: "t" }], truncated: false,
    } as StepLogsResponse);
    s = appendLogPage(s, {
      run_id: "r", step_id: "a", attempt: 2, lines: [{ seq: 1, level: "info", message: "new", logged_at: "t" }], truncated: false,
    } as StepLogsResponse);
    expect(s.lines.map((l) => l.seq)).toEqual([1]);
    expect(s.attempt).toBe(2);
  });

  it("filterByLevel drops lines below the minimum", () => {
    const lines = stepLogsFixture.lines;
    expect(filterByLevel(lines, "warn").map((l) => l.level)).toEqual(["warn"]);
    expect(filterByLevel(lines, "debug")).toHaveLength(3);
  });
});
