package store

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mathcslearner/agentloom/internal/event"
)

// sanctionedEventWriters are the only functions in the store package permitted
// to physically append an event (call Events().Append / gen AppendEvent). Every
// other event write must route through one of these typed helpers, which derive
// the event type from an event.Payload — so a writer can never emit a
// mismatched (type, shape) pair (ticket 16.1, ADR-018).
var sanctionedEventWriters = map[string]bool{
	"appendEvent": true, // transitions.go: package-level typed helper
	// instantiate.go: (*instantiationPlan).appendEvent — same simple name.
}

// TestNoAdHocEventWrites is the "no ad-hoc event writes" CI check (ADR-018): it
// parses the store package and asserts that the only call sites of the physical
// append primitives (`AppendEvent`, `Events().Append`) are inside the
// sanctioned typed helpers. A new event writer that bypasses the typed helper
// fails this test.
func TestNoAdHocEventWrites(t *testing.T) {
	t.Parallel()

	fset := token.NewFileSet()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		src, err := os.ReadFile(path) // #nosec G304 -- test scans its own package sources
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		f, err := parser.ParseFile(fset, path, src, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		// Walk each top-level func; flag any physical append call inside a
		// non-sanctioned function.
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok {
				continue
			}
			if sanctionedEventWriters[fn.Name.Name] {
				continue
			}
			// eventRepo.Append is the single low-level primitive both typed
			// helpers call — it is the plumbing, not an ad-hoc writer.
			if fn.Name.Name == "Append" && receiverType(fn) == "eventRepo" {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				if sel.Sel.Name == "AppendEvent" {
					t.Errorf("%s: %s calls AppendEvent directly — route event writes through the typed appendEvent helper (ADR-018)",
						fset.Position(call.Pos()), fn.Name.Name)
				}
				// Events().Append — the EventRepo write surface.
				if sel.Sel.Name == "Append" {
					if inner, ok := sel.X.(*ast.CallExpr); ok {
						if innerSel, ok := inner.Fun.(*ast.SelectorExpr); ok && innerSel.Sel.Name == "Events" {
							t.Errorf("%s: %s calls Events().Append directly — route event writes through the typed appendEvent helper (ADR-018)",
								fset.Position(call.Pos()), fn.Name.Name)
						}
					}
				}
				return true
			})
		}
	}
}

// receiverType returns the (possibly pointer-dereferenced) receiver type name
// of a method, or "" for a plain function.
func receiverType(fn *ast.FuncDecl) string {
	if fn.Recv == nil || len(fn.Recv.List) == 0 {
		return ""
	}
	switch t := fn.Recv.List[0].Type.(type) {
	case *ast.Ident:
		return t.Name
	case *ast.StarExpr:
		if id, ok := t.X.(*ast.Ident); ok {
			return id.Name
		}
	}
	return ""
}

// TestEventConstantsMatchVocabulary pins the store's Event* string constants to
// the event package's Type vocabulary: every store constant equals its
// event.Type, so ListByType/event-feed reads and the append helper agree.
func TestEventConstantsMatchVocabulary(t *testing.T) {
	t.Parallel()

	pairs := map[string]event.Type{
		EventRunCreated: event.TypeRunCreated, EventStepReady: event.TypeStepReady,
		EventStepClaimed: event.TypeStepClaimed, EventStepSucceeded: event.TypeStepSucceeded,
		EventStepDeadLettered: event.TypeStepDeadLettered, EventGraphExpanded: event.TypeGraphExpanded,
		EventCostUpdated: event.TypeCostUpdated, EventApprovalRequested: event.TypeApprovalRequested,
		EventApprovalNotificationFailed: event.TypeApprovalNotificationFailed,
	}
	for got, want := range pairs {
		if got != string(want) {
			t.Errorf("store constant %q != event.Type %q", got, want)
		}
		if _, ok := event.Lookup(want); !ok {
			t.Errorf("event.Type %q is not in the catalog", want)
		}
	}
}
