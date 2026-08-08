package dag

import (
	"errors"
	"fmt"
	"strconv"
	"sync"

	"github.com/google/cel-go/cel"
)

// This file is the ticket 1.5 seam: CEL compilation and evaluation for the
// edge predicates decided in ADR-003 — `when` on normal edges and
// `condition` on loop edges. Compilation runs at definition-validation
// time (Validate rejects invalid expressions with source positions);
// evaluation is a pure helper the engine's completion transaction calls
// when a step completes (M4). Per ADR-003, an evaluation error is a
// recorded step-level failure — Eval returns a typed *EvalError and never
// coerces errors to false.
//
// The expression environment (documented in docs/expressions.md):
//
//	output — the completed source step's output (dyn)
//	run    — run-scoped context map; only run.params (the submitted run
//	         parameters, dyn map) is populated today
//
// exprEnv builds the shared environment once; cel.Env and cel.Program are
// safe for concurrent use, so compiled expressions are freely reusable
// across goroutines.
var exprEnv = sync.OnceValues(func() (*cel.Env, error) {
	return cel.NewEnv(
		cel.Variable("output", cel.DynType),
		cel.Variable("run", cel.MapType(cel.StringType, cel.DynType)),
	)
})

// ExprError is one CEL compile-time problem (syntax or typecheck failure)
// at a source position within the expression. Line and Col are 1-based,
// matching cel-go's display convention.
type ExprError struct {
	Line int
	Col  int
	Msg  string
}

func (e *ExprError) Error() string {
	return fmt.Sprintf("%d:%d: %s", e.Line, e.Col, e.Msg)
}

// ExprNotBoolError reports an expression that compiles but cannot produce
// a bool: `when` and `condition` are predicates, so the checked result
// type must be bool (or dyn, which defers the check to evaluation time).
type ExprNotBoolError struct {
	Type string // the expression's checked CEL result type, e.g. "int"
}

func (e *ExprNotBoolError) Error() string {
	return "expression must be a boolean predicate, but evaluates to " + e.Type
}

// EvalError is a runtime evaluation failure: a missing field, a type
// error, or a dyn-typed predicate producing a non-bool. Per ADR-003 the
// engine records it as a step-level failure of the completing step's
// transition; it is never treated as the predicate being false.
type EvalError struct {
	Expr  string
	Msg   string // set when there is no underlying CEL error (non-bool result)
	Cause error  // the CEL evaluation error, when present
}

func (e *EvalError) Error() string {
	msg := e.Msg
	if e.Cause != nil {
		msg = e.Cause.Error()
	}
	return "evaluating CEL expression " + strconv.Quote(e.Expr) + ": " + msg
}

func (e *EvalError) Unwrap() error { return e.Cause }

// CompiledExpr is a compiled edge predicate, ready for repeated,
// concurrency-safe evaluation.
type CompiledExpr struct {
	src  string
	prog cel.Program
}

// Source returns the original expression text.
func (c *CompiledExpr) Source() string { return c.src }

// CompileExpr compiles a `when`/`condition` predicate against the edge
// expression environment. Syntax and typecheck failures return joined
// *ExprError values carrying 1-based source positions; a well-typed
// expression whose result cannot be bool returns *ExprNotBoolError.
func CompileExpr(src string) (*CompiledExpr, error) {
	env, err := exprEnv()
	if err != nil {
		return nil, fmt.Errorf("dag: building CEL environment: %w", err)
	}
	ast, iss := env.Compile(src)
	if iss.Err() != nil {
		errs := make([]error, 0, len(iss.Errors()))
		for _, ce := range iss.Errors() {
			errs = append(errs, &ExprError{
				Line: ce.Location.Line(),
				Col:  ce.Location.Column() + 1, // cel-go locations are 0-based columns
				Msg:  ce.Message,
			})
		}
		return nil, errors.Join(errs...)
	}
	if t := ast.OutputType(); !t.IsExactType(cel.BoolType) && !t.IsExactType(cel.DynType) {
		return nil, &ExprNotBoolError{Type: t.String()}
	}
	prog, err := env.Program(ast)
	if err != nil {
		return nil, fmt.Errorf("dag: building CEL program for %q: %w", src, err)
	}
	return &CompiledExpr{src: src, prog: prog}, nil
}

// Eval evaluates the predicate with the completed source step's output and
// the run's submitted parameters. Any evaluation failure — missing field,
// type error, or a non-bool result — returns a typed *EvalError
// (errors.As-reachable); the boolean is meaningful only when err is nil.
func (c *CompiledExpr) Eval(output any, params map[string]any) (bool, error) {
	if params == nil {
		params = map[string]any{}
	}
	val, _, err := c.prog.Eval(map[string]any{
		"output": output,
		"run":    map[string]any{"params": params},
	})
	if err != nil {
		return false, &EvalError{Expr: c.src, Cause: err}
	}
	b, ok := val.Value().(bool)
	if !ok {
		return false, &EvalError{
			Expr: c.src,
			Msg:  fmt.Sprintf("expression evaluated to %s, not bool", val.Type().TypeName()),
		}
	}
	return b, nil
}
