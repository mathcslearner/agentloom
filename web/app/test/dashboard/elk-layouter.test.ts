import { describe, expect, it } from "vitest";
import { layoutWithElk } from "@/lib/dashboard/elk-layouter";
import { NODE_H, NODE_W } from "@/lib/pure/dashboard/layout";

// A single real-elk smoke: the injected layouter runs under Node and lays a
// two-node chain out left→right (the `computeLayout` sticky logic is covered
// with a fake layouter in layout.test.ts — this proves the elkjs plumbing).
describe("layoutWithElk", () => {
  it("lays a chain out left to right", async () => {
    const out = await layoutWithElk(
      [
        { id: "a", width: NODE_W, height: NODE_H },
        { id: "b", width: NODE_W, height: NODE_H },
      ],
      [{ from: "a", to: "b" }],
    );
    expect(out.size).toBe(2);
    expect(out.get("b")!.x).toBeGreaterThan(out.get("a")!.x);
  });

  it("returns an empty map for no nodes", async () => {
    expect((await layoutWithElk([], [])).size).toBe(0);
  });
});
