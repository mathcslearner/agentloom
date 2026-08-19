import { describe, expect, it } from "vitest";
import { diffLines, diffStats, toLines } from "@/lib/pure/dashboard/diff";

describe("diffLines", () => {
  it("reports no changes for identical text", () => {
    const h = diffLines("a\nb\nc", "a\nb\nc");
    expect(diffStats(h)).toEqual({ same: 3, add: 0, del: 0 });
  });

  it("reports pure additions when text is appended (the feedback case)", () => {
    const before = "Write a blurb.";
    const after = "Write a blurb.\n\nThis is attempt 2 of 3. Fix the problems.";
    const h = diffLines(before, after);
    const stats = diffStats(h);
    expect(stats.del).toBe(0);
    expect(stats.add).toBeGreaterThan(0);
    // The added hunks carry the feedback text.
    expect(h.some((x) => x.kind === "add" && x.text.includes("attempt 2 of 3"))).toBe(true);
  });

  it("reports deletions when lines are removed", () => {
    const h = diffLines("a\nb\nc", "a\nc");
    expect(diffStats(h)).toMatchObject({ del: 1, add: 0 });
    expect(h.find((x) => x.kind === "del")?.text).toBe("b");
  });

  it("handles a replaced region as del-before-add", () => {
    const h = diffLines("x\nold\ny", "x\nnew\ny");
    const kinds = h.map((x) => x.kind);
    expect(kinds).toContain("del");
    expect(kinds).toContain("add");
    expect(diffStats(h).same).toBe(2);
  });

  it("toLines drops a single trailing newline", () => {
    expect(toLines("a\nb\n")).toEqual(["a", "b"]);
    expect(toLines("")).toEqual([]);
  });
});
