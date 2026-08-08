# Edge condition expressions

Workflow definitions use [CEL](https://cel.dev) (Common Expression
Language) predicates to route execution along edges:

- **`when`** on a normal edge — evaluated when the edge's source step
  completes; `true` fires the edge, `false` skips the target (subject to
  the readiness and skip-propagation rules in
  [ADR-003](adr/003-workflow-definition-format.md)). On a `branch` step's
  out-edges, the first `when` that evaluates `true` in declaration order
  fires and all others skip.
- **`condition`** on a loop edge — evaluated when the loop source
  completes; `true` means "iterate again" (bounded by `max_iterations`).

Both are compiled and typechecked when the definition is validated —
before anything runs. A syntactically invalid or ill-typed expression is
rejected with `invalid_expression` issues carrying the `line:col`
position inside the expression; an expression whose checked type is not
boolean is rejected with `expression_not_boolean`, which reports the
offending type but no position (the finding is about the expression as a
whole).

A future step type will also take CEL expressions (`map.items`, arriving
with runtime expansion in M13); this document will grow with it.

## Environment

Expressions evaluate in an environment with exactly two variables:

| Variable | CEL type | Contents |
|---|---|---|
| `output` | `dyn` | The completed source step's output. Its shape depends on the step type and is not statically known, so field selection typechecks permissively and is enforced at evaluation time. |
| `run` | `map(string, dyn)` | Run-scoped context. Today only one key is populated: `run.params` — the parameter values submitted when the run was created, matching the definition's `params` declarations. |

The standard CEL built-ins and macros are available (`has()`, `size()`,
`exists()`, `all()`, `in`, string/list/map operators, and so on). There
are no agentloom-specific custom functions.

Expressions must be **boolean predicates**: an expression whose checked
type is anything other than `bool` (or `dyn`, which defers the check to
evaluation) is rejected at validation time. A `dyn` expression that
produces a non-bool at runtime is an evaluation error.

## Evaluation errors are failures, never `false`

Per ADR-003: if evaluating a predicate errors at runtime — a missing
field, a type mismatch, a non-bool result — the error is recorded as a
step-level failure of the completing step's transition. It is **never
silently coerced to `false`**, because "the edge didn't fire" and "the
workflow author's predicate is broken" must stay distinguishable. How
that failure is classified and retried is the engine's failure policy
(ADR-006, M5).

Common error sources and how to guard against them:

| Expression | Output | Result |
|---|---|---|
| `output.score > 0.5` | `{"score": 0.9}` | `true` |
| `output.score > 0.5` | `{}` | **error** — no such key `score` |
| `has(output.score) && output.score > 0.5` | `{}` | `false` — `has()` guards the selection |
| `output.n < 'x'` | `{"n": 1}` | **error** — no `int < string` overload |
| `output.flag` | `{"flag": "yes"}` | **error** — predicate produced a string, not a bool |

Use `has(...)` when a field is genuinely optional; leave it off when the
field is part of the step's contract, so a malformed output fails loudly
instead of silently routing down the "false" path.

## Examples

Branch routing on a classifier's output:

```json
{"edges": [
  {"from": "route", "to": "refund_flow",  "when": "output.category == 'refund'"},
  {"from": "route", "to": "billing_flow", "when": "output.category == 'billing'"},
  {"from": "route", "to": "generic_flow"}
]}
```

A parameter-controlled critic loop:

```json
{"from": "critique", "to": "draft", "type": "loop",
 "condition": "output.verdict == 'revise' && run.params.strict == true",
 "max_iterations": 5}
```

Optional-field guard with a fallback edge:

```json
{"edges": [
  {"from": "score", "to": "publish",  "when": "has(output.score) && output.score >= run.params.max_score"},
  {"from": "score", "to": "escalate", "when": "!has(output.score)"}
]}
```

## Limits and reserved syntax

- Expressions are capped at **1,024 bytes** (ADR-003 limits table).
- Step IDs may not contain `.` or `#` — reserved so future CEL paths and
  loop-instance names (`{id}#k`, M13/M14) can reference steps
  unambiguously.

## Go API (`internal/dag`)

`CompileExpr(src)` compiles a predicate against this environment and
returns a `*CompiledExpr`; compile failures are `*ExprError` values
(with 1-based `Line`/`Col`) or `*ExprNotBoolError`. `Validate` calls it
for every `when`/`condition`. `CompiledExpr.Eval(output, params)`
returns the routing boolean or a `*EvalError` (`errors.As`-reachable,
wrapping the CEL cause); compiled expressions are safe for concurrent
reuse. The engine's completion transaction (M4) owns calling `Eval` and
recording failures.
