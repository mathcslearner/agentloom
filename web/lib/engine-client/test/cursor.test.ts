import { describe, expect, it } from "vitest";

import { RunCursors } from "../src/cursor.js";

describe("RunCursors", () => {
  it("classifies new / duplicate / gap by seq", () => {
    const c = new RunCursors();
    expect(c.classify("r", 1)).toEqual({ kind: "new" });
    c.advance("r", 1);
    expect(c.classify("r", 1)).toEqual({ kind: "duplicate" });
    expect(c.classify("r", 2)).toEqual({ kind: "new" });
    expect(c.classify("r", 5)).toEqual({ kind: "gap", expected: 2 });
  });

  it("advances only forward", () => {
    const c = new RunCursors();
    c.advance("r", 5);
    c.advance("r", 3); // ignored
    expect(c.lastSeq("r")).toBe(5);
  });

  it("tracks per run independently and snapshots cursors", () => {
    const c = new RunCursors({ a: 2 });
    c.advance("b", 7);
    expect(c.lastSeq("a")).toBe(2);
    expect(c.lastSeq("b")).toBe(7);
    expect(c.lastSeq("c")).toBe(0);
    expect(c.snapshot()).toEqual({ a: 2, b: 7 });
    expect(c.runIds()).toEqual(["a", "b"]);
    expect(c.size()).toBe(2);
  });

  it("forgets a run", () => {
    const c = new RunCursors({ a: 1, b: 2 });
    c.forget("a");
    expect(c.has("a")).toBe(false);
    expect(c.snapshot()).toEqual({ b: 2 });
  });
});
