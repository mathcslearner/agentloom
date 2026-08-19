// The client CEL checker (validate/cel.ts): accept/reject + code over a curated
// table, plus the invariant that it never over-rejects — every `when`/`condition`
// in the accepted backend corpus must pass. The backend is the authority
// (internal/dag/cel.go), so the client may only under-reject.
import { describe, expect, it } from "vitest";
import { checkExpr } from "../src/index.js";
import { exampleDefinitions, validTestdata } from "./fixtures.js";

describe("checkExpr", () => {
  const accepts: string[] = [
    "output.x == 1",
    "output.ok",
    "output.ok == true",
    "!has(output.score)",
    "has(output.score) && output.score >= run.params.max_score",
    "output.verdict == 'revise' && run.params.strict == true",
    "output.json.verdict == 'revise'",
    "has(output.json.verdict) && output.json.verdict == 'revise'",
    "!has(run.params.dry_run) || run.params.dry_run != true",
    "has(output.docs) && size(output.docs) > 0",
    "output.category == 'refund'",
    "output.n in [1, 2, 3]",
    "output.name.startsWith('a')",
    "output.a ? output.b : output.c", // ternary → dyn (accepted)
    "output.x > 0 && output.x < 10",
    "output.x + output.y * 2", // dyn arithmetic → dyn, deferred to runtime (accepted)
  ];
  for (const src of accepts) {
    it(`accepts ${JSON.stringify(src)}`, () => {
      expect(checkExpr(src)).toEqual({ ok: true });
    });
  }

  const invalid: string[] = [
    "output.verdict ==", // trailing operator
    "output.category ==",
    "outputs.category == 'billing'", // undeclared root
    "not_declared == 'x'",
    "output.x +", // trailing operator
    "output.x == 'unterminated", // unterminated string
    "(output.x == 1", // unbalanced paren
    "@", // stray character
  ];
  for (const src of invalid) {
    it(`rejects ${JSON.stringify(src)} as invalid_expression`, () => {
      const r = checkExpr(src);
      expect(r.ok).toBe(false);
      if (!r.ok) expect(r.code).toBe("invalid_expression");
    });
  }

  const notBool: string[] = ["1 + 2", "3 * 4", "'a string'", "42"];
  for (const src of notBool) {
    it(`rejects ${JSON.stringify(src)} as expression_not_boolean`, () => {
      const r = checkExpr(src);
      expect(r.ok).toBe(false);
      if (!r.ok) expect(r.code).toBe("expression_not_boolean");
    });
  }

  it("never rejects a predicate from the accepted corpus", () => {
    const fixtures = [...exampleDefinitions(), ...validTestdata()];
    for (const f of fixtures) {
      const v = f.value as { edges?: Array<Record<string, unknown>> };
      for (const e of v.edges ?? []) {
        for (const key of ["when", "condition"] as const) {
          const expr = e[key];
          if (typeof expr === "string" && expr !== "") {
            expect(checkExpr(expr), `${f.name}: ${expr}`).toEqual({ ok: true });
          }
        }
      }
    }
  });
});
