package exec

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// stubExecutor is a minimal Executor with a settable type.
type stubExecutor struct{ typ string }

func (s stubExecutor) Type() string { return s.typ }

func (s stubExecutor) Execute(context.Context, StepContext) (Output, error) {
	return Output{}, nil
}

func TestRegistryLookup(t *testing.T) {
	t.Parallel()

	r, err := NewRegistry(stubExecutor{typ: "a"}, stubExecutor{typ: "b"})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	e, err := r.Get("a")
	if err != nil {
		t.Fatalf("Get(a): %v", err)
	}
	if e.Type() != "a" {
		t.Errorf("Get(a).Type() = %q, want %q", e.Type(), "a")
	}
}

func TestRegistryUnknownType(t *testing.T) {
	t.Parallel()

	r, err := NewRegistry(stubExecutor{typ: "a"})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	_, err = r.Get("bogus")
	if !errors.Is(err, ErrUnknownType) {
		t.Fatalf("Get(bogus) error = %v, want ErrUnknownType", err)
	}
	var ute *UnknownTypeError
	if !errors.As(err, &ute) || ute.Type != "bogus" {
		t.Errorf("error = %#v, want *UnknownTypeError{Type: \"bogus\"}", err)
	}
}

func TestRegistryRejectsBadRegistrations(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		execs []Executor
		want  string // substring of the error
	}{
		{"duplicate type", []Executor{stubExecutor{typ: "a"}, stubExecutor{typ: "a"}}, `type "a" registered twice`},
		{"empty type", []Executor{stubExecutor{}}, "empty type"},
		{"nil executor", []Executor{nil}, "nil executor"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := NewRegistry(tc.execs...)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("NewRegistry error = %v, want substring %q", err, tc.want)
			}
		})
	}
}

func TestBuiltinsRegistersAllTestExecutors(t *testing.T) {
	t.Parallel()

	r := Builtins()
	for _, typ := range []string{"noop", "echo", "sleep", "fail_n_times"} {
		e, err := r.Get(typ)
		if err != nil {
			t.Errorf("Get(%q): %v", typ, err)
			continue
		}
		if e.Type() != typ {
			t.Errorf("Get(%q).Type() = %q", typ, e.Type())
		}
	}
}
