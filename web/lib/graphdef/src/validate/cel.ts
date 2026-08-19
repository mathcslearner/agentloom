// A lenient client-side checker for the CEL edge predicates (`when` on normal
// edges, `condition` on loop edges), mirroring the accept/reject decision of
// internal/dag/cel.go CompileExpr — NOT its exact cel-go message. It exists so
// the four corpus fixtures that reject only on a bad expression
// (condition_syntax_error, when_syntax_error, when_undeclared_ref,
// when_not_boolean) reject client-side too, and so a well-formed predicate gets
// immediate green feedback.
//
// Design stance (ADR-019 §"Validation parity"): the backend is the authority,
// so the client may only UNDER-reject. It reports two of the backend's codes —
// `invalid_expression` (a syntax error or an undeclared root reference) and
// `expression_not_boolean` (a well-typed expression whose result can never be
// bool) — using coarse type inference: `output` is dyn, `run` is a map, any
// other bare identifier is an undeclared reference; comparisons/logic/`has`
// yield bool, arithmetic yields a number, and anything unrecognised yields dyn
// (which the backend's DynType also accepts, so the client never over-rejects).

import { Code, err, type Issue } from "./issue.js";

/** The result of checking one expression. */
export type ExprCheck = { ok: true } | { ok: false; code: string; msg: string };

type CelType = "bool" | "int" | "double" | "string" | "bytes" | "list" | "map" | "null" | "dyn";

const DECLARED_ROOTS = new Set(["output", "run"]);
// Functions whose return type we know; everything else returns dyn (lenient).
const FUNC_RETURN: Record<string, CelType> = {
  has: "bool",
  size: "int",
  matches: "bool",
  contains: "bool",
  startsWith: "bool",
  endsWith: "bool",
  int: "int",
  uint: "int",
  double: "double",
  string: "string",
  bool: "bool",
};

interface Token {
  kind: "id" | "num" | "str" | "op" | "eof";
  text: string;
  isFloat?: boolean;
  pos: number;
}

class CelError extends Error {}

function tokenize(src: string): Token[] {
  const toks: Token[] = [];
  let i = 0;
  const n = src.length;
  const two = new Set(["==", "!=", "<=", ">=", "&&", "||"]);
  const one = new Set([
    "(", ")", "[", "]", "{", "}", ".", ",", ":", "?", "!", "<", ">", "+", "-", "*", "/", "%",
  ]);
  while (i < n) {
    const c = src[i]!;
    if (c === " " || c === "\t" || c === "\n" || c === "\r") {
      i += 1;
      continue;
    }
    if (c === '"' || c === "'") {
      const quote = c;
      let j = i + 1;
      let closed = false;
      while (j < n) {
        if (src[j] === "\\") {
          j += 2;
          continue;
        }
        if (src[j] === quote) {
          closed = true;
          break;
        }
        j += 1;
      }
      if (!closed) throw new CelError("unterminated string literal");
      toks.push({ kind: "str", text: src.slice(i, j + 1), pos: i });
      i = j + 1;
      continue;
    }
    if (c >= "0" && c <= "9") {
      let j = i;
      let isFloat = false;
      while (j < n && /[0-9]/.test(src[j]!)) j += 1;
      if (src[j] === ".") {
        isFloat = true;
        j += 1;
        while (j < n && /[0-9]/.test(src[j]!)) j += 1;
      }
      if (src[j] === "e" || src[j] === "E") {
        isFloat = true;
        j += 1;
        if (src[j] === "+" || src[j] === "-") j += 1;
        while (j < n && /[0-9]/.test(src[j]!)) j += 1;
      }
      if (src[j] === "u") j += 1; // uint literal suffix
      toks.push({ kind: "num", text: src.slice(i, j), isFloat, pos: i });
      i = j;
      continue;
    }
    if (/[A-Za-z_]/.test(c)) {
      let j = i;
      while (j < n && /[A-Za-z0-9_]/.test(src[j]!)) j += 1;
      toks.push({ kind: "id", text: src.slice(i, j), pos: i });
      i = j;
      continue;
    }
    const t2 = src.slice(i, i + 2);
    if (two.has(t2)) {
      toks.push({ kind: "op", text: t2, pos: i });
      i += 2;
      continue;
    }
    if (one.has(c)) {
      toks.push({ kind: "op", text: c, pos: i });
      i += 1;
      continue;
    }
    throw new CelError(`unexpected character ${JSON.stringify(c)}`);
  }
  toks.push({ kind: "eof", text: "", pos: n });
  return toks;
}

class Parser {
  private toks: Token[];
  private pos = 0;
  constructor(toks: Token[]) {
    this.toks = toks;
  }
  private peek(): Token {
    return this.toks[this.pos]!;
  }
  private next(): Token {
    return this.toks[this.pos++]!;
  }
  private expect(text: string): void {
    const t = this.next();
    if (t.text !== text) throw new CelError(`expected ${JSON.stringify(text)}`);
  }
  private isOp(text: string): boolean {
    const t = this.peek();
    return t.kind === "op" && t.text === text;
  }

  parse(): CelType {
    const t = this.ternary();
    if (this.peek().kind !== "eof") throw new CelError("trailing tokens");
    return t;
  }

  private ternary(): CelType {
    const cond = this.or();
    if (this.isOp("?")) {
      this.next();
      this.or();
      this.expect(":");
      this.ternary();
      return "dyn";
    }
    return cond;
  }
  private or(): CelType {
    let t = this.and();
    while (this.isOp("||")) {
      this.next();
      this.and();
      t = "bool";
    }
    return t;
  }
  private and(): CelType {
    let t = this.rel();
    while (this.isOp("&&")) {
      this.next();
      this.rel();
      t = "bool";
    }
    return t;
  }
  private rel(): CelType {
    let t = this.add();
    for (;;) {
      const p = this.peek();
      if (p.kind === "op" && ["==", "!=", "<", "<=", ">", ">="].includes(p.text)) {
        this.next();
        this.add();
        t = "bool";
      } else if (p.kind === "id" && p.text === "in") {
        this.next();
        this.add();
        t = "bool";
      } else {
        return t;
      }
    }
  }
  private add(): CelType {
    let t = this.mul();
    while (this.isOp("+") || this.isOp("-")) {
      this.next();
      const r = this.mul();
      t = numeric(t, r);
    }
    return t;
  }
  private mul(): CelType {
    let t = this.unary();
    while (this.isOp("*") || this.isOp("/") || this.isOp("%")) {
      this.next();
      const r = this.unary();
      t = numeric(t, r);
    }
    return t;
  }
  private unary(): CelType {
    if (this.isOp("!")) {
      this.next();
      this.unary();
      return "bool";
    }
    if (this.isOp("-")) {
      this.next();
      const t = this.unary();
      return t === "double" ? "double" : t === "int" ? "int" : "dyn";
    }
    return this.postfix();
  }
  private postfix(): CelType {
    let t = this.primary();
    for (;;) {
      if (this.isOp(".")) {
        this.next();
        const id = this.next();
        if (id.kind !== "id") throw new CelError("expected field name after '.'");
        if (this.isOp("(")) {
          this.args();
          t = "dyn"; // method call: lenient
        } else {
          t = "dyn"; // field access
        }
      } else if (this.isOp("[")) {
        this.next();
        this.ternary();
        this.expect("]");
        t = "dyn";
      } else {
        return t;
      }
    }
  }
  private args(): void {
    this.expect("(");
    if (this.isOp(")")) {
      this.next();
      return;
    }
    this.ternary();
    while (this.isOp(",")) {
      this.next();
      this.ternary();
    }
    this.expect(")");
  }
  private primary(): CelType {
    const t = this.peek();
    if (t.kind === "num") {
      this.next();
      return t.isFloat ? "double" : "int";
    }
    if (t.kind === "str") {
      this.next();
      return "string";
    }
    if (t.kind === "op" && t.text === "(") {
      this.next();
      const inner = this.ternary();
      this.expect(")");
      return inner;
    }
    if (t.kind === "op" && t.text === "[") {
      this.next();
      if (!this.isOp("]")) {
        this.ternary();
        while (this.isOp(",")) {
          this.next();
          if (this.isOp("]")) break;
          this.ternary();
        }
      }
      this.expect("]");
      return "list";
    }
    if (t.kind === "op" && t.text === "{") {
      this.next();
      if (!this.isOp("}")) {
        this.mapEntry();
        while (this.isOp(",")) {
          this.next();
          if (this.isOp("}")) break;
          this.mapEntry();
        }
      }
      this.expect("}");
      return "map";
    }
    if (t.kind === "id") {
      this.next();
      if (t.text === "true" || t.text === "false") return "bool";
      if (t.text === "null") return "null";
      // A function call: the identifier is the callee.
      if (this.isOp("(")) {
        this.args();
        return FUNC_RETURN[t.text] ?? "dyn";
      }
      // A bare value identifier: only the declared roots exist.
      if (!DECLARED_ROOTS.has(t.text)) {
        throw new CelError(`undeclared reference to '${t.text}'`);
      }
      return t.text === "run" ? "map" : "dyn";
    }
    throw new CelError("expected an expression");
  }
  private mapEntry(): void {
    this.ternary();
    this.expect(":");
    this.ternary();
  }
}

function numeric(a: CelType, b: CelType): CelType {
  if (a === "double" || b === "double") return "double";
  if (a === "int" && b === "int") return "int";
  if (a === "string" && b === "string") return "string";
  return "dyn";
}

/** Check a CEL predicate; returns ok or a typed rejection. */
export function checkExpr(src: string): ExprCheck {
  let type: CelType;
  try {
    type = new Parser(tokenize(src)).parse();
  } catch (e) {
    const msg = e instanceof CelError ? e.message : "invalid expression";
    return { ok: false, code: Code.ExprInvalid, msg };
  }
  if (type !== "bool" && type !== "dyn") {
    return { ok: false, code: Code.ExprNotBool, msg: `expression must be a boolean predicate, but evaluates to ${type}` };
  }
  return { ok: true };
}

/** Append an issue for a bad predicate at `path`, or nothing if it checks out. */
export function checkExprInto(path: string, src: string, out: Issue[]): void {
  const r = checkExpr(src);
  if (r.ok) return;
  out.push(err(r.code, path, r.msg));
}
