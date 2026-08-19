import { describe, expect, it } from "vitest";

import { backoffDelay, resolveBackoff } from "../src/backoff.js";

describe("backoffDelay", () => {
  const opts = resolveBackoff({ initialMs: 100, maxMs: 1000, factor: 2, jitter: "none" });

  it("grows exponentially and caps at maxMs (no jitter)", () => {
    expect(backoffDelay(0, opts)).toBe(100);
    expect(backoffDelay(1, opts)).toBe(200);
    expect(backoffDelay(2, opts)).toBe(400);
    expect(backoffDelay(3, opts)).toBe(800);
    expect(backoffDelay(4, opts)).toBe(1000); // 1600 -> capped
    expect(backoffDelay(10, opts)).toBe(1000);
  });

  it("full jitter spreads uniformly over [0, base) using the rng", () => {
    const jit = resolveBackoff({ initialMs: 100, maxMs: 1000, factor: 2, jitter: "full" });
    // rng = 0 -> 0; rng ~1 -> just under base.
    expect(backoffDelay(2, jit, () => 0)).toBe(0);
    expect(backoffDelay(2, jit, () => 0.5)).toBe(200); // 0.5 * 400
    expect(backoffDelay(2, jit, () => 0.999)).toBe(Math.floor(0.999 * 400));
    // Capped base under jitter, too.
    expect(backoffDelay(10, jit, () => 0.5)).toBe(500); // 0.5 * 1000
  });

  it("clamps negative attempts to attempt 0", () => {
    expect(backoffDelay(-3, opts)).toBe(100);
  });

  it("defaults are sane", () => {
    const d = resolveBackoff();
    expect(d.initialMs).toBe(250);
    expect(d.maxMs).toBe(10_000);
    expect(d.jitter).toBe("full");
  });
});
