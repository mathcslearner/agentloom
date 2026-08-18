package dag_test

import (
	"encoding/json"
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
	"echo_pipeline.json",
	"mock_pipeline.json",
	"rag_lite.json",
	"structured_extract.json",
	"semantic_retry.json",
	"llm_judge.json",
	"blackboard.json",
	"context_assembly.json",
	"context_compaction.json",
	"context_summarization.json",
	"context_window.json",
	"planner.json",
	"map_fanout.json",
	"agent_pipeline.json",
	"agent_handoff.json",
	"research-critic-writer.json",
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

	var loopEdge, loopExhaustPolicy, loopNoProgress, conditionedEdge, hasGuard bool
	outEdges := make(map[string][]dag.Edge) // normal out-edges, declaration order
	for _, e := range def.Edges {
		if strings.Contains(e.When, "has(") || strings.Contains(e.Condition, "has(") {
			hasGuard = true
		}
		if e.IsLoop() {
			loopEdge = true
			if e.OnExhausted == dag.ExhaustFail {
				loopExhaustPolicy = true
			}
			if e.NoProgress != nil {
				loopNoProgress = true
			}
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
	if !loopExhaustPolicy {
		t.Error("no explicit on_exhausted policy on the loop edge in kitchen_sink.json")
	}
	if !loopNoProgress {
		t.Error("no loop edge with a no_progress guard in kitchen_sink.json")
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

	// Ticket 5.3 construct: a per-step execution timeout.
	var hasTimeout bool
	for _, s := range def.Steps {
		if s.Timeout != "" {
			hasTimeout = true
		}
	}
	if !hasTimeout {
		t.Error("no step with an execution timeout in kitchen_sink.json")
	}

	// Ticket 5.6 construct: a run wall-clock deadline.
	if def.MaxWallClock == "" {
		t.Error("no max_wall_clock deadline in kitchen_sink.json")
	}

	// Ticket 9.4 construct: an opt-in response-cache policy (ADR-011).
	var hasCache bool
	for _, s := range def.Steps {
		if s.Cache != nil && s.Cache.Mode != "" {
			hasCache = true
		}
	}
	if !hasCache {
		t.Error("no step with a cache policy in kitchen_sink.json")
	}

	// Ticket 10.3 constructs (ADR-012): a run-level spend budget with an
	// explicit exceed policy, and a step with per-step budget caps.
	if def.BudgetUSD == nil {
		t.Error("no budget_usd in kitchen_sink.json")
	}
	if def.OnBudgetExceeded == "" {
		t.Error("no explicit on_budget_exceeded policy in kitchen_sink.json")
	}
	var hasStepBudget, hasStepMaxTokens bool
	for _, s := range def.Steps {
		if s.Budget == nil {
			continue
		}
		if s.Budget.MaxUSD != nil {
			hasStepBudget = true
		}
		if s.Budget.MaxTokens > 0 {
			hasStepMaxTokens = true
		}
	}
	if !hasStepBudget {
		t.Error("no step with a budget.max_usd cap in kitchen_sink.json")
	}
	if !hasStepMaxTokens {
		t.Error("no llm step with a budget.max_tokens cap in kitchen_sink.json")
	}

	// Ticket 13.1 construct (ADR-015): a run-level expansion cap block and a
	// planner carrying a per-expansion max_added_steps override.
	if def.Expansion == nil || def.Expansion.MaxAddedSteps == nil {
		t.Error("no expansion policy with max_added_steps in kitchen_sink.json")
	}
	var hasPlannerCap bool
	for _, s := range def.Steps {
		if c, ok := s.Config.(*dag.PlannerConfig); ok && c.MaxAddedSteps > 0 {
			hasPlannerCap = true
		}
	}
	if !hasPlannerCap {
		t.Error("no planner with a max_added_steps override in kitchen_sink.json")
	}

	// Ticket 8.2 constructs: template expressions referencing a run param
	// and an upstream step output somewhere in the step configs.
	var paramRef, stepRef bool
	for _, s := range def.Steps {
		if s.Config == nil {
			continue
		}
		raw, err := json.Marshal(s.Config)
		if err != nil {
			t.Fatalf("marshaling %s config: %v", s.ID, err)
		}
		if strings.Contains(string(raw), "${{ run.params.") {
			paramRef = true
		}
		if strings.Contains(string(raw), "${{ steps.") {
			stepRef = true
		}
	}
	if !paramRef {
		t.Error("no ${{ run.params.* }} template reference in kitchen_sink.json")
	}
	if !stepRef {
		t.Error("no ${{ steps.*.output }} template reference in kitchen_sink.json")
	}

	// Ticket 11.1 construct (ADR-013): a step with an output-validation
	// chain — at least one that carries an explicit target, so a schema
	// edit that dropped target handling would fail here.
	var hasValidation, hasValidationTarget bool
	for _, s := range def.Steps {
		if s.Validation == nil || len(s.Validation.Validators) == 0 {
			continue
		}
		hasValidation = true
		for _, spec := range s.Validation.Validators {
			if spec.Target != "" {
				hasValidationTarget = true
			}
		}
	}
	if !hasValidation {
		t.Error("no step with a validation chain in kitchen_sink.json")
	}
	if !hasValidationTarget {
		t.Error("no validation chain entry with an explicit target in kitchen_sink.json")
	}

	// Ticket 11.4 construct (ADR-013): a step carrying a semantic-retry
	// policy — a max_attempts budget above one and a feedback template — so a
	// schema edit that dropped the semantic-policy fields would fail here.
	var hasSemanticPolicy bool
	for _, s := range def.Steps {
		if s.Validation != nil && s.Validation.EffectiveMaxAttempts() > 1 && s.Validation.Feedback != nil {
			hasSemanticPolicy = true
		}
	}
	if !hasSemanticPolicy {
		t.Error("no step with a semantic-retry policy (max_attempts + feedback) in kitchen_sink.json")
	}

	// Ticket 11.3 construct (ADR-013): an llm step with a structured-output
	// block — a json_schema type carrying a schema, so a schema edit that
	// dropped output_format handling would fail here.
	var hasOutputFormat bool
	for _, s := range def.Steps {
		if s.Type != dag.StepLLM {
			continue
		}
		of := s.Config.(*dag.LLMConfig).OutputFormat
		if of != nil && of.Type == dag.OutputFormatJSONSchema && len(of.Schema) > 0 {
			hasOutputFormat = true
		}
	}
	if !hasOutputFormat {
		t.Error("no llm step with a json_schema output_format in kitchen_sink.json")
	}

	// Ticket 12.2 construct (ADR-014): a step with a declarative blackboard
	// write carrying a pinned entry, so a schema edit that dropped the
	// blackboard envelope block would fail here.
	var hasPinnedBlackboardWrite bool
	for _, s := range def.Steps {
		if s.Blackboard == nil {
			continue
		}
		for _, w := range s.Blackboard.Write {
			if w.Pinned {
				hasPinnedBlackboardWrite = true
			}
		}
	}
	if !hasPinnedBlackboardWrite {
		t.Error("no step with a pinned declarative blackboard write in kitchen_sink.json")
	}

	// Ticket 12.3/12.4/12.5 construct (ADR-014): a step with a context-assembly
	// spec exercising all four source kinds, a pinned source, a per-source cap,
	// a skip missing-policy, a source priority, a budget, the three
	// deterministic compaction strategies, and the summarize strategy — so a
	// schema edit that dropped the context envelope block or a compaction
	// strategy would fail here.
	var (
		hasContext    bool
		kinds         = map[dag.ContextSourceKind]bool{}
		hasPinnedSrc  bool
		hasSourceCap  bool
		hasSkipPolicy bool
		hasPriority   bool
		hasBudget     bool
		strategies    = map[dag.CompactionStrategyKind]bool{}
	)
	for _, s := range def.Steps {
		if s.Context == nil {
			continue
		}
		hasContext = true
		if s.Context.BudgetTokens != nil {
			hasBudget = true
		}
		for _, st := range s.Context.Compaction {
			strategies[st.Strategy] = true
		}
		for _, src := range s.Context.Sources {
			kinds[src.Kind] = true
			if src.Pinned {
				hasPinnedSrc = true
			}
			if src.MaxTokens != nil {
				hasSourceCap = true
			}
			if src.OnMissing == dag.MissingSkip {
				hasSkipPolicy = true
			}
			if src.Priority != nil {
				hasPriority = true
			}
		}
	}
	if !hasContext {
		t.Error("no step with a context-assembly spec in kitchen_sink.json")
	}
	for _, k := range []dag.ContextSourceKind{dag.SourceStepOutput, dag.SourceBlackboard, dag.SourceRetrieval, dag.SourceLiteral, dag.SourceThread} {
		if !kinds[k] {
			t.Errorf("no context source of kind %q in kitchen_sink.json", k)
		}
	}
	if !hasPinnedSrc {
		t.Error("no pinned context source in kitchen_sink.json")
	}
	if !hasSourceCap {
		t.Error("no context source with a per-source max_tokens cap in kitchen_sink.json")
	}
	if !hasSkipPolicy {
		t.Error("no context source with a skip missing-policy in kitchen_sink.json")
	}
	if !hasPriority {
		t.Error("no context source with a priority in kitchen_sink.json")
	}
	if !hasBudget {
		t.Error("no context spec with a budget_tokens in kitchen_sink.json")
	}
	for _, st := range []dag.CompactionStrategyKind{dag.SlidingWindow, dag.TruncateOldest, dag.DropLowestPriority, dag.SummarizeStrategy} {
		if !strategies[st] {
			t.Errorf("no %q compaction strategy in kitchen_sink.json", st)
		}
	}

	// Ticket 14.1 construct (ADR-016): an `agents` section declaring a role
	// with defaults (system prompt + allowed toolset), and an `agent` step
	// referencing it — so a schema edit that dropped the agents section or the
	// agent step type would fail here.
	if len(def.Agents) == 0 {
		t.Error("no agents section in kitchen_sink.json")
	}
	var roleHasSystem, roleHasTools bool
	for _, a := range def.Agents {
		if a.System != "" {
			roleHasSystem = true
		}
		if len(a.Tools) > 0 {
			roleHasTools = true
		}
	}
	if !roleHasSystem {
		t.Error("no agent role with a system prompt in kitchen_sink.json")
	}
	if !roleHasTools {
		t.Error("no agent role with an allowed toolset in kitchen_sink.json")
	}
	var refsAgent bool
	for _, s := range def.Steps {
		if c, ok := s.Config.(*dag.AgentConfig); ok && c.Agent != "" {
			refsAgent = true
		}
	}
	if !refsAgent {
		t.Error("no agent step referencing a role in kitchen_sink.json")
	}
}

// TestExampleEchoPipelineCoversTemplateConstructs pins echo_pipeline.json
// (ticket 8.2) to the templating feature set: dropping a construct in an
// edit fails here, keeping the fixture the canonical data-flow example.
func TestExampleEchoPipelineCoversTemplateConstructs(t *testing.T) {
	t.Parallel()

	def, err := dag.Decode(readExample(t, "echo_pipeline.json"))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}

	var all strings.Builder
	for _, s := range def.Steps {
		if s.Config == nil {
			continue
		}
		raw, err := json.Marshal(s.Config)
		if err != nil {
			t.Fatalf("marshaling %s config: %v", s.ID, err)
		}
		all.Write(raw)
	}
	configs := all.String()
	for construct, marker := range map[string]string{ //nolint:gosec // G101 false positive: template-syntax markers, not credentials
		"run param reference":            "${{ run.params.",
		"nested output path":             ".output.audience.name",
		"whole-output pass-through":      "${{ steps.compose.output }}",
		"lenient get with default":       "get 'steps.",
		"default function":               "| default ",
		"toJson function":                "| toJson",
		"truncate function":              "| truncate",
		"multi-hop (two-steps-up) input": "${{ steps.compose.output",
	} {
		if !strings.Contains(configs, marker) {
			t.Errorf("echo_pipeline.json no longer demonstrates %s (marker %q missing)", construct, marker)
		}
	}
	if len(def.Params) < 2 {
		t.Errorf("echo_pipeline.json declares %d params, want at least 2 (a scalar and an object)", len(def.Params))
	}
}
