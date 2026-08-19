import { describe, expect, it } from "vitest";
import { requeueable, runControls } from "@/lib/pure/dashboard/run-controls";

describe("runControls", () => {
  it("offers cancel + park while running", () => {
    expect(runControls("running")).toEqual({ cancel: true, park: true, unpark: false });
  });
  it("offers cancel + unpark while parked", () => {
    expect(runControls("parked")).toEqual({ cancel: true, park: false, unpark: true });
  });
  it("offers nothing on a terminal or cancelling run", () => {
    for (const s of ["succeeded", "failed", "cancelling", "cancelled"] as const) {
      expect(runControls(s)).toEqual({ cancel: false, park: false, unpark: false });
    }
  });
});

describe("requeueable", () => {
  it("is true for a dead-lettered step on a live run", () => {
    expect(requeueable("dead_lettered", "failed")).toBe(true);
    expect(requeueable("dead_lettered", "running")).toBe(true);
  });
  it("is false for a non-dead-lettered step", () => {
    expect(requeueable("running", "running")).toBe(false);
    expect(requeueable("succeeded", "failed")).toBe(false);
  });
  it("is false on a cancelled run (the server refuses it)", () => {
    expect(requeueable("dead_lettered", "cancelled")).toBe(false);
  });
});
