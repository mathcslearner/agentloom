package dag_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/mathcslearner/agentloom/internal/dag"
)

// TestCompileExprValid covers expressions that must compile: bool-typed
// predicates, dyn-typed selections (checked at eval time), the has()
// macro, and run.params access.
func TestCompileExprValid(t *testing.T) {
	t.Parallel()

	exprs := []string{
		"output.category == 'billing'",
		"output.verdict == 'revise' && run.params.strict == true",
		"has(output.score) && output.score > 0.5",
		"run.params.retries >= 3",
		"output.flag", // dyn: bool-ness enforced at evaluation time
		"true",
		"!(output.tags.exists(t, t == 'urgent'))",
	}
	for _, src := range exprs {
		t.Run(src, func(t *testing.T) {
			t.Parallel()
			c, err := dag.CompileExpr(src)
			if err != nil {
				t.Fatalf("CompileExpr(%q): %v", src, err)
			}
			if c.Source() != src {
				t.Errorf("Source() = %q, want %q", c.Source(), src)
			}
		})
	}
}

// TestCompileExprErrors covers the rejection classes: syntax errors and
// undeclared references (position-carrying *ExprError) and well-typed
// non-boolean expressions (*ExprNotBoolError).
func TestCompileExprErrors(t *testing.T) {
	t.Parallel()

	t.Run("syntax error carries position", func(t *testing.T) {
		t.Parallel()
		_, err := dag.CompileExpr("output.category ==")
		if err == nil {
			t.Fatal("CompileExpr: want error, got nil")
		}
		var ee *dag.ExprError
		if !errors.As(err, &ee) {
			t.Fatalf("error does not unwrap to *dag.ExprError: %v", err)
		}
		if ee.Line != 1 || ee.Col <= 0 {
			t.Errorf("position = %d:%d, want line 1 and a positive column", ee.Line, ee.Col)
		}
		if !strings.Contains(err.Error(), "1:") {
			t.Errorf("rendered error lacks position info: %v", err)
		}
	})

	t.Run("undeclared reference", func(t *testing.T) {
		t.Parallel()
		_, err := dag.CompileExpr("outputs.category == 'x'")
		if err == nil {
			t.Fatal("CompileExpr: want error, got nil")
		}
		var ee *dag.ExprError
		if !errors.As(err, &ee) {
			t.Fatalf("error does not unwrap to *dag.ExprError: %v", err)
		}
		if !strings.Contains(ee.Msg, "undeclared reference") {
			t.Errorf("message = %q, want an undeclared-reference error", ee.Msg)
		}
	})

	t.Run("multiple errors are all reported", func(t *testing.T) {
		t.Parallel()
		_, err := dag.CompileExpr("nope == 1 && also_nope == 2")
		if err == nil {
			t.Fatal("CompileExpr: want error, got nil")
		}
		joined, ok := err.(interface{ Unwrap() []error })
		if !ok || len(joined.Unwrap()) != 2 {
			t.Fatalf("want 2 joined errors, got: %v", err)
		}
	})

	t.Run("non-boolean result type", func(t *testing.T) {
		t.Parallel()
		for expr, wantType := range map[string]string{
			"1 + 2":       "int",
			"'abc'":       "string",
			"[1, 2]":      "list(int)",
			"size('abc')": "int",
		} {
			_, err := dag.CompileExpr(expr)
			if err == nil {
				t.Errorf("CompileExpr(%q): want error, got nil", expr)
				continue
			}
			var nb *dag.ExprNotBoolError
			if !errors.As(err, &nb) {
				t.Errorf("CompileExpr(%q): error does not unwrap to *dag.ExprNotBoolError: %v", expr, err)
				continue
			}
			if nb.Type != wantType {
				t.Errorf("CompileExpr(%q): result type = %q, want %q", expr, nb.Type, wantType)
			}
		}
	})
}

// TestCompiledExprEval covers routing outcomes and the ADR-003 error
// policy: truthy and falsy results route, while missing fields, type
// errors, and non-bool dyn results return a typed *EvalError — never a
// silent false.
func TestCompiledExprEval(t *testing.T) {
	t.Parallel()

	eval := func(t *testing.T, src string, output any, params map[string]any) (bool, error) {
		t.Helper()
		c, err := dag.CompileExpr(src)
		if err != nil {
			t.Fatalf("CompileExpr(%q): %v", src, err)
		}
		return c.Eval(output, params)
	}

	t.Run("truthy and falsy routing", func(t *testing.T) {
		t.Parallel()
		c, err := dag.CompileExpr("output.verdict == 'yes'")
		if err != nil {
			t.Fatalf("CompileExpr: %v", err)
		}
		for verdict, want := range map[string]bool{"yes": true, "no": false} {
			got, err := c.Eval(map[string]any{"verdict": verdict}, nil)
			if err != nil {
				t.Fatalf("Eval(verdict=%q): %v", verdict, err)
			}
			if got != want {
				t.Errorf("Eval(verdict=%q) = %v, want %v", verdict, got, want)
			}
		}
	})

	t.Run("run params access", func(t *testing.T) {
		t.Parallel()
		got, err := eval(t, "run.params.limit > 10", map[string]any{}, map[string]any{"limit": 11})
		if err != nil {
			t.Fatalf("Eval: %v", err)
		}
		if !got {
			t.Error("Eval = false, want true")
		}
	})

	t.Run("has guards missing fields without error", func(t *testing.T) {
		t.Parallel()
		got, err := eval(t, "has(output.score) && output.score > 0.5", map[string]any{}, nil)
		if err != nil {
			t.Fatalf("Eval: %v", err)
		}
		if got {
			t.Error("Eval = true, want false for absent field behind has()")
		}
	})

	t.Run("nil params behave as empty", func(t *testing.T) {
		t.Parallel()
		got, err := eval(t, "has(run.params.x)", map[string]any{}, nil)
		if err != nil {
			t.Fatalf("Eval: %v", err)
		}
		if got {
			t.Error("Eval = true, want false")
		}
	})

	wantEvalError := func(t *testing.T, src string, output any, params map[string]any, wantCause bool) *dag.EvalError {
		t.Helper()
		got, err := eval(t, src, output, params)
		if err == nil {
			t.Fatalf("Eval(%q): want error, got %v", src, got)
		}
		if got {
			t.Errorf("Eval(%q): boolean must be false when erroring", src)
		}
		var ee *dag.EvalError
		if !errors.As(err, &ee) {
			t.Fatalf("Eval(%q): error does not unwrap to *dag.EvalError: %v", src, err)
		}
		if ee.Expr != src {
			t.Errorf("EvalError.Expr = %q, want %q", ee.Expr, src)
		}
		if (ee.Cause != nil) != wantCause {
			t.Errorf("EvalError.Cause = %v, wantCause = %v", ee.Cause, wantCause)
		}
		return ee
	}

	t.Run("missing field is a typed error, not false", func(t *testing.T) {
		t.Parallel()
		_ = wantEvalError(t, "output.missing == 'x'", map[string]any{"present": 1}, nil, true)
	})

	t.Run("runtime type error", func(t *testing.T) {
		t.Parallel()
		_ = wantEvalError(t, "output.n < 'x'", map[string]any{"n": 1}, nil, true)
	})

	t.Run("nil output errors on selection", func(t *testing.T) {
		t.Parallel()
		_ = wantEvalError(t, "output.x == 1", nil, nil, true)
	})

	t.Run("dyn non-bool result", func(t *testing.T) {
		t.Parallel()
		ee := wantEvalError(t, "output.flag", map[string]any{"flag": "yes"}, nil, false)
		if !strings.Contains(ee.Msg, "not bool") {
			t.Errorf("EvalError.Msg = %q, want a non-bool message", ee.Msg)
		}
	})
}

// BenchmarkCompileExpr pins the per-expression compile cost — relevant
// because Validate now compiles every conditioned edge, so a definition
// with thousands of predicates pays this per edge.
func BenchmarkCompileExpr(b *testing.B) {
	for b.Loop() {
		if _, err := dag.CompileExpr("output.category == 'billing' && run.params.strict == true"); err != nil {
			b.Fatal(err)
		}
	}
}
