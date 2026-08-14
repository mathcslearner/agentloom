package exec

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/mathcslearner/agentloom/internal/dag"
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

	r := Builtins(nil, nil)
	for _, typ := range []string{"noop", "echo", "sleep", "fail_n_times", "counter", "join", "branch", "llm", "tool", "retrieve"} {
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

// TestCoreBuiltinsExcludesFilesystemExecutors pins the 6.2 split: the
// production default registry is Builtins minus exactly the two test
// executors with filesystem side effects.
func TestCoreBuiltinsExcludesFilesystemExecutors(t *testing.T) {
	t.Parallel()

	core := CoreBuiltins(nil, nil)
	for _, typ := range []string{"counter", "effectful_echo"} {
		if _, err := core.Get(typ); err == nil {
			t.Errorf("CoreBuiltins registers %q — filesystem test executors must be opt-in", typ)
		}
	}
	full := Builtins(nil, nil)
	if got, want := len(core.Types())+2, len(full.Types()); got != want {
		t.Errorf("core (%d) + 2 = %d types, Builtins has %d — the split no longer partitions the set",
			len(core.Types()), got, want)
	}
	for _, typ := range core.Types() {
		if _, err := full.Get(typ); err != nil {
			t.Errorf("core type %q missing from Builtins", typ)
		}
	}
}

// deferredStepTypes are the dag catalog types deliberately without an
// executor yet. A definition using one passes submit-time validation and
// then fails permanently at claim time (registry miss → dead-letter), which
// includes the canonical kitchen_sink.json example — a known M4 gap, not
// an accident. Shrink this set as the executors land; never let it grow
// silently (the sync test below fails on any unlisted mismatch).
var deferredStepTypes = map[dag.StepType]string{
	dag.StepMap:           "M13 (map fan-out expansion)",
	dag.StepPlanner:       "M13 (dynamic planner expansion)",
	dag.StepAgent:         "M12 (multi-agent roles)",
	dag.StepHumanApproval: "M15 (human-in-the-loop approvals)",
}

// TestBuiltinsSyncWithCatalog pins the exec registry to the dag step-type
// catalog exactly (post-M4 audit): every catalog type either has a
// builtin executor or is explicitly deferred with a ticket reference, and
// the registry contains nothing outside the catalog. Adding a 15th step
// type without deciding its executor story fails here.
func TestBuiltinsSyncWithCatalog(t *testing.T) {
	t.Parallel()

	r := Builtins(nil, nil)
	catalog := make(map[string]bool)
	for _, st := range dag.StepTypes() {
		catalog[string(st)] = true
		_, err := r.Get(string(st))
		_, deferred := deferredStepTypes[st]
		switch {
		case err != nil && !deferred:
			t.Errorf("catalog type %q has no executor and is not in deferredStepTypes", st)
		case err == nil && deferred:
			t.Errorf("catalog type %q has an executor but is still listed as deferred (%s) — remove it from deferredStepTypes",
				st, deferredStepTypes[st])
		}
	}
	for _, typ := range r.Types() {
		if !catalog[typ] {
			t.Errorf("registry type %q is not in the dag catalog — executors must be first-class step types", typ)
		}
	}
	if got, want := len(r.Types())+len(deferredStepTypes), len(dag.StepTypes()); got != want {
		t.Errorf("registry (%d) + deferred (%d) = %d types, catalog has %d — the accounting no longer partitions the catalog",
			len(r.Types()), len(deferredStepTypes), got, want)
	}
}
