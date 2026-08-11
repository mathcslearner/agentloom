package dag_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mathcslearner/agentloom/internal/dag"
)

// examplesDir locates the canonical example corpus (ticket 1.6): the
// shared golden fixtures for engine tests, the M17 serialization
// round-trip suite, and docs.
var examplesDir = filepath.Join("..", "..", "examples", "definitions")

// exampleFiles pins the corpus contents, in the roadmap's order. A file
// added to examples/definitions/ without an entry here (or vice versa)
// fails TestExamplesCorpusIsPinned.
var exampleFiles = []string{
	"linear.json",
	"fanout.json",
	"conditional_branch.json",
	"critic_loop.json",
	"kitchen_sink.json",
}

// readExample loads one example definition document.
func readExample(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(examplesDir, name)) // #nosec G304 -- committed fixture path, test-only
	if err != nil {
		t.Fatalf("reading example: %v", err)
	}
	return data
}

// TestExamplesCorpusIsPinned fails when the files on disk and the pinned
// list drift apart, so the corpus cannot gain an untested example or
// silently lose a canonical one.
func TestExamplesCorpusIsPinned(t *testing.T) {
	t.Parallel()

	files, err := filepath.Glob(filepath.Join(examplesDir, "*.json"))
	if err != nil || len(files) == 0 {
		t.Fatalf("globbing example definitions: %v (found %d)", err, len(files))
	}
	onDisk := make(map[string]bool, len(files))
	for _, f := range files {
		onDisk[filepath.Base(f)] = true
	}
	for _, name := range exampleFiles {
		if !onDisk[name] {
			t.Errorf("%s: pinned example is missing from %s", name, examplesDir)
		}
		delete(onDisk, name)
	}
	for name := range onDisk {
		t.Errorf("%s: file is not pinned in exampleFiles", name)
	}
}

// TestExamplesAreGolden is the ticket 1.6 acceptance test: every canonical
// example decodes, passes structural validation with zero issues (unlike
// testdata/valid, not even warnings are tolerated), carries its "header
// comment" in the description field (JSON has no comments and the decoder
// rejects unknown fields), and survives a lossless canonical round trip.
func TestExamplesAreGolden(t *testing.T) {
	t.Parallel()

	for _, name := range exampleFiles {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			data := readExample(t, name)

			def, err := dag.Decode(data)
			if err != nil {
				t.Fatalf("Decode: %v", err)
			}
			issues, err := dag.Validate(def)
			if err != nil {
				t.Fatalf("Validate: %v", err)
			}
			if len(issues) != 0 {
				t.Errorf("want zero issues (warnings included), got %v", issues)
			}
			if def.Description == "" {
				t.Error("example has no description explaining what it demonstrates")
			}

			assertLosslessRoundTrip(t, data)
		})
	}
}

// TestExampleKitchenSinkCoversEveryConstruct pins kitchen_sink.json to the
// package's registries and the ADR-003 edge features: a step or param type
// added without extending the example — or an edge construct dropped in an
// edit — fails here, keeping the "exercises every construct" claim true.
func TestExampleKitchenSinkCoversEveryConstruct(t *testing.T) {
	t.Parallel()

	def, err := dag.Decode(readExample(t, "kitchen_sink.json"))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}

	usedSteps := make(map[dag.StepType]bool)
	for _, s := range def.Steps {
		usedSteps[s.Type] = true
	}
	for _, st := range dag.RegisteredStepTypes() {
		if !usedSteps[st] {
			t.Errorf("step type %q is registered but unused in kitchen_sink.json", st)
		}
	}

	usedParams := make(map[dag.ParamType]bool)
	for _, p := range def.Params {
		usedParams[p.Type] = true
	}
	for _, pt := range dag.RegisteredParamTypes() {
		if !usedParams[pt] {
			t.Errorf("param type %q is unused in kitchen_sink.json", pt)
		}
	}

	joinModes := make(map[dag.JoinMode]bool)
	branchSteps := make(map[string]bool)
	for _, s := range def.Steps {
		switch c := s.Config.(type) {
		case *dag.JoinConfig:
			joinModes[c.Mode] = true
		case *dag.BranchConfig:
			branchSteps[s.ID] = true
		}
	}
	for _, m := range []dag.JoinMode{dag.JoinAll, dag.JoinAny} {
		if !joinModes[m] {
			t.Errorf("join mode %q is unused in kitchen_sink.json", m)
		}
	}

	var loopEdge, conditionedEdge, hasGuard bool
	outEdges := make(map[string][]dag.Edge) // normal out-edges, declaration order
	for _, e := range def.Edges {
		if strings.Contains(e.When, "has(") || strings.Contains(e.Condition, "has(") {
			hasGuard = true
		}
		if e.IsLoop() {
			loopEdge = true
			continue
		}
		if e.When != "" && !branchSteps[e.From] {
			conditionedEdge = true
		}
		outEdges[e.From] = append(outEdges[e.From], e)
	}
	if !loopEdge {
		t.Error("no loop edge in kitchen_sink.json")
	}
	if !conditionedEdge {
		t.Error("no when-conditioned non-branch edge in kitchen_sink.json")
	}
	if !hasGuard {
		t.Error("no has() guard in any kitchen_sink.json predicate")
	}

	var fanOut bool
	for from, outs := range outEdges {
		unconditioned := 0
		for _, e := range outs {
			if e.When == "" {
				unconditioned++
			}
		}
		if unconditioned >= 2 && !branchSteps[from] {
			fanOut = true
		}
	}
	if !fanOut {
		t.Error("no unconditioned parallel fan-out (step with >=2 unconditioned out-edges) in kitchen_sink.json")
	}

	var branchWithDefault bool
	for id := range branchSteps {
		outs := outEdges[id]
		if len(outs) < 2 {
			t.Errorf("branch %q should demonstrate >=2 out-edges, has %d", id, len(outs))
			continue
		}
		if outs[len(outs)-1].When == "" {
			branchWithDefault = true
		}
	}
	if len(branchSteps) == 0 {
		t.Error("no branch step in kitchen_sink.json")
	} else if !branchWithDefault {
		t.Error("no branch with a trailing default edge in kitchen_sink.json")
	}

	if len(def.UI) == 0 {
		t.Error("kitchen_sink.json has no ui block")
	}

	// ADR-006 constructs: an explicit failure policy, one full retry
	// policy (backoff + jitter + retry_on) and one partial policy that
	// inherits engine defaults for its absent fields.
	if def.OnFailure == "" {
		t.Error("no explicit on_failure policy in kitchen_sink.json")
	}
	var fullRetry, partialRetry bool
	for _, s := range def.Steps {
		switch r := s.Retry; {
		case r == nil:
		case r.Backoff != nil && r.Jitter != "" && r.RetryOn != nil:
			fullRetry = true
		default:
			partialRetry = true
		}
	}
	if !fullRetry {
		t.Error("no full retry policy (backoff + jitter + retry_on) in kitchen_sink.json")
	}
	if !partialRetry {
		t.Error("no partial retry policy (inheriting engine defaults) in kitchen_sink.json")
	}
}
