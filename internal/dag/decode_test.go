package dag_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mathcslearner/agentloom/internal/dag"
)

// readFixture loads a testdata fixture.
func readFixture(t *testing.T, parts ...string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(append([]string{"testdata"}, parts...)...))
	if err != nil {
		t.Fatalf("reading fixture: %v", err)
	}
	return data
}

// decodeInvalidCases maps every testdata/invalid fixture to substrings its
// decode error must contain. The corpus-coverage test pins this table to
// the fixture directory in both directions.
var decodeInvalidCases = map[string][]string{
	"syntax.json":                     {"invalid JSON"},
	"root_array.json":                 {"expected object, got array"},
	"missing_schema_version.json":     {"schema_version: required field is missing"},
	"schema_version_string.json":      {"schema_version: expected integer, got string"},
	"schema_version_float.json":       {"schema_version: expected integer, got 1.5"},
	"schema_version_unsupported.json": {"schema_version: unsupported schema_version 2"},
	"missing_name_steps_edges.json": {
		"name: required field is missing",
		"steps: required field is missing",
		"edges: required field is missing",
	},
	"unknown_top_field.json":                 {"descriptionz: unknown field"},
	"wrong_type_name.json":                   {"name: expected string, got number"},
	"steps_not_array.json":                   {"steps: expected array, got object"},
	"step_missing_id_type.json":              {"steps[0].id: required field is missing", "steps[0].type: required field is missing"},
	"unknown_step_type.json":                 {`steps[0].type: unknown step type "llmz"`},
	"unknown_step_field.json":                {"steps[0].retires: unknown field"},
	"config_unknown_field.json":              {"steps[0].config.modle: unknown field"},
	"config_wrong_type.json":                 {"steps[0].config.max_tokens: expected integer, got string"},
	"config_nested_unknown.json":             {"steps[0].config.messages[0].contnt: unknown field"},
	"config_not_object.json":                 {"steps[0].config: expected object, got array"},
	"join_bad_mode.json":                     {`steps[0].config.mode: unknown join mode "eventually"`},
	"bad_on_failure.json":                    {`on_failure: unknown failure policy "explode"`},
	"retry_not_object.json":                  {"steps[0].retry: expected object, got number"},
	"retry_unknown_field.json":               {"steps[0].retry.max_tries: unknown field"},
	"retry_backoff_unknown_field.json":       {"steps[0].retry.backoff.floor: unknown field"},
	"retry_bad_jitter.json":                  {`steps[0].retry.jitter: unknown jitter mode "half"`},
	"retry_bad_class.json":                   {`steps[0].retry.retry_on[1]: unknown error class "flaky"`},
	"retry_wrong_type.json":                  {"steps[0].retry.max_attempts: expected integer, got string"},
	"timeout_wrong_type.json":                {"steps[0].timeout: expected string, got number"},
	"cache_not_object.json":                  {"steps[0].cache: expected object, got string"},
	"cache_unknown_field.json":               {"steps[0].cache.expiry: unknown field"},
	"cache_bad_mode.json":                    {`steps[0].cache.mode: unknown cache mode "always"`},
	"cache_bad_scope.json":                   {`steps[0].cache.scope: unknown cache scope "tenant"`},
	"cache_wrong_type.json":                  {"steps[0].cache.ttl: expected string, got number"},
	"validation_not_object.json":             {"steps[0].validation: expected object, got string"},
	"validation_unknown_field.json":          {"steps[0].validation.unknown: unknown field"},
	"validation_feedback_unknown_field.json": {"steps[0].validation.feedback.tone: unknown field"},
	"validation_entry_unknown_field.json":    {"steps[0].validation.validators[0].weight: unknown field"},
	"blackboard_unknown_field.json":          {"steps[0].blackboard.writes: unknown field"},
	"max_wall_clock_wrong_type.json":         {"max_wall_clock: expected string, got number"},
	"edge_missing_from.json":                 {"edges[0].from: required field is missing"},
	"unknown_edge_field.json":                {"edges[0].whenever: unknown field"},
	"unknown_edge_type.json":                 {`edges[0].type: unknown edge type "loopy"`},
	"param_missing_type.json":                {"params.goal.type: required field is missing"},
	"bad_param_type.json":                    {`params.goal.type: unknown param type "text"`},
	"param_entry_not_object.json":            {"params.goal: expected object, got string"},
	"ui_not_object.json":                     {"ui: expected object, got array"},
	"multi_error.json": {
		"name: expected string, got number",
		`steps[0].type: unknown step type "llmz"`,
		"steps[1].retires: unknown field",
		"steps[1].config.modle: unknown field",
		"steps[1].config.max_tokens: expected integer, got string",
		"edges[0].from: required field is missing",
		"edges[0].whenever: unknown field",
		"extra: unknown field",
	},
}

func TestDecodeInvalidFixtures(t *testing.T) {
	t.Parallel()

	for name, wantInErr := range decodeInvalidCases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			def, err := dag.Decode(readFixture(t, "invalid", name))
			if err == nil {
				t.Fatal("Decode: want error, got nil")
			}
			if def != nil {
				t.Errorf("Decode: want nil definition on error, got %+v", def)
			}
			for _, want := range wantInErr {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error does not mention %q; full error:\n%v", want, err)
				}
			}
		})
	}
}

// TestDecodeInvalidCorpusIsCovered pins the expectation table to the
// fixture directory in both directions, so a fixture added without
// expectations (or an expectation whose fixture is gone) fails loudly.
func TestDecodeInvalidCorpusIsCovered(t *testing.T) {
	t.Parallel()

	files, err := filepath.Glob(filepath.Join("testdata", "invalid", "*.json"))
	if err != nil || len(files) == 0 {
		t.Fatalf("globbing invalid fixtures: %v (found %d)", err, len(files))
	}
	onDisk := make(map[string]bool, len(files))
	for _, f := range files {
		name := filepath.Base(f)
		onDisk[name] = true
		if _, covered := decodeInvalidCases[name]; !covered {
			t.Errorf("%s: fixture has no expectations in decodeInvalidCases", name)
		}
	}
	for name := range decodeInvalidCases {
		if !onDisk[name] {
			t.Errorf("%s: expected fixture is missing from testdata/invalid", name)
		}
	}
}

// TestDecodeVersionGate verifies an unsupported schema_version is reported
// alone: the rest of the document is not interpreted under an unknown
// format.
func TestDecodeVersionGate(t *testing.T) {
	t.Parallel()

	_, err := dag.Decode([]byte(`{"schema_version": 2, "steps": 7, "bogus": true}`))
	if err == nil {
		t.Fatal("Decode: want error, got nil")
	}
	if !strings.Contains(err.Error(), "unsupported schema_version 2") {
		t.Errorf("error %q does not mention the unsupported version", err)
	}
	for _, absent := range []string{"steps", "bogus"} {
		if strings.Contains(err.Error(), absent) {
			t.Errorf("version-gated error should not mention %q; full error:\n%v", absent, err)
		}
	}
}

// TestFixtureKitchenSinkCoversCatalog pins the decode-corpus kitchen sink
// to the step-type catalog, like examples_test does for the canonical
// example (post-M4 audit: this fixture silently went stale when 4.1/4.7
// added step types).
func TestFixtureKitchenSinkCoversCatalog(t *testing.T) {
	t.Parallel()

	def, err := dag.Decode(readFixture(t, "valid", "kitchen_sink.json"))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	used := make(map[dag.StepType]bool)
	for _, s := range def.Steps {
		used[s.Type] = true
	}
	for _, st := range dag.StepTypes() {
		if !used[st] {
			t.Errorf("step type %q is in the catalog but unused in testdata/valid/kitchen_sink.json", st)
		}
	}
}

func TestDecodeEdgeTypeNormalization(t *testing.T) {
	t.Parallel()

	def, err := dag.Decode(readFixture(t, "valid", "kitchen_sink.json"))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	var loops, normals int
	for _, e := range def.Edges {
		switch e.Type {
		case dag.EdgeLoop:
			loops++
			if !e.IsLoop() {
				t.Errorf("edge %s->%s: IsLoop() = false for loop edge", e.From, e.To)
			}
			if e.Condition == "" || e.MaxIterations < 1 {
				t.Errorf("loop edge %s->%s: condition/max_iterations not decoded: %+v", e.From, e.To, e)
			}
		case dag.EdgeNormal:
			normals++
			if e.IsLoop() {
				t.Errorf("edge %s->%s: IsLoop() = true for normal edge", e.From, e.To)
			}
		default:
			t.Errorf("edge %s->%s: unnormalized type %q", e.From, e.To, e.Type)
		}
	}
	if loops != 1 || normals != len(def.Edges)-1 {
		t.Errorf("got %d loop / %d normal edges, want 1 / %d", loops, normals, len(def.Edges)-1)
	}
}

func TestDecodeTypedConfigs(t *testing.T) {
	t.Parallel()

	def, err := dag.Decode(readFixture(t, "valid", "kitchen_sink.json"))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	byID := make(map[string]dag.Step, len(def.Steps))
	for _, s := range def.Steps {
		byID[s.ID] = s
	}

	llm, ok := byID["draft"].Config.(*dag.LLMConfig)
	if !ok {
		t.Fatalf("draft config = %T, want *dag.LLMConfig", byID["draft"].Config)
	}
	if llm.Temperature == nil || *llm.Temperature != 0 {
		t.Errorf("draft temperature = %v, want explicit 0 preserved", llm.Temperature)
	}
	if llm.MaxTokens != 1024 || len(llm.Messages) != 1 {
		t.Errorf("draft config not fully decoded: %+v", llm)
	}
	if len(llm.ModelFallbacks) != 2 {
		t.Fatalf("draft model_fallbacks = %d, want 2", len(llm.ModelFallbacks))
	}
	if llm.ModelFallbacks[0].Model != "anthropic/claude-haiku-4-5" ||
		llm.ModelFallbacks[0].AtBudgetFraction == nil || *llm.ModelFallbacks[0].AtBudgetFraction != 0.8 {
		t.Errorf("draft model_fallbacks[0] = %+v, want haiku @0.8", llm.ModelFallbacks[0])
	}
	if llm.ModelFallbacks[1].Model != "openai/gpt-5-mini" || llm.ModelFallbacks[1].AtBudgetFraction != nil {
		t.Errorf("draft model_fallbacks[1] = %+v, want gpt-5-mini with no threshold", llm.ModelFallbacks[1])
	}

	join, ok := byID["gather"].Config.(*dag.JoinConfig)
	if !ok || join.Mode != dag.JoinAll {
		t.Errorf("gather config = %#v, want *dag.JoinConfig{Mode: all}", byID["gather"].Config)
	}

	if cfg := byID["start"].Config; cfg != nil {
		t.Errorf("start (noop, no config key) config = %#v, want nil", cfg)
	}
}

// TestDecodeRetryPolicy covers the ADR-006 step envelope fields: the
// typed retry policy in full and partial spellings, absent-key nils, and
// the top-level failure policy.
func TestDecodeRetryPolicy(t *testing.T) {
	t.Parallel()

	def, err := dag.Decode(readFixture(t, "valid", "kitchen_sink.json"))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if def.OnFailure != dag.ContinueIndependentBranches {
		t.Errorf("OnFailure = %q, want %q", def.OnFailure, dag.ContinueIndependentBranches)
	}
	byID := make(map[string]dag.Step, len(def.Steps))
	for _, s := range def.Steps {
		byID[s.ID] = s
	}

	full := byID["flaky_probe"].Retry
	if full == nil {
		t.Fatal("flaky_probe has no decoded retry policy")
	}
	if full.MaxAttempts != 4 || full.Jitter != dag.JitterFull {
		t.Errorf("flaky_probe retry = %+v, want max_attempts 4, jitter full", full)
	}
	if full.Backoff == nil || full.Backoff.Initial != "100ms" || full.Backoff.Cap != "2s" || full.Backoff.Multiplier != 2 {
		t.Errorf("flaky_probe backoff = %+v, want {100ms 2s 2}", full.Backoff)
	}
	if len(full.RetryOn) != 2 || full.RetryOn[0] != dag.ClassTransient || full.RetryOn[1] != dag.ClassTimeout {
		t.Errorf("flaky_probe retry_on = %v, want [transient timeout]", full.RetryOn)
	}

	partial := byID["fetch"].Retry
	if partial == nil || partial.MaxAttempts != 2 {
		t.Fatalf("fetch retry = %+v, want partial policy with max_attempts 2", partial)
	}
	if partial.Backoff != nil || partial.Jitter != "" || partial.RetryOn != nil {
		t.Errorf("fetch retry decoded absent keys as non-zero: %+v", partial)
	}

	if r := byID["start"].Retry; r != nil {
		t.Errorf("start (no retry key) retry = %+v, want nil", r)
	}
}

// TestDecodeCachePolicy covers the ADR-011 step envelope cache field: a
// full policy, a mode-only spelling with absent-key empties, and the
// absent-block nil.
func TestDecodeCachePolicy(t *testing.T) {
	t.Parallel()

	def, err := dag.Decode(readFixture(t, "valid", "kitchen_sink.json"))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	byID := make(map[string]dag.Step, len(def.Steps))
	for _, s := range def.Steps {
		byID[s.ID] = s
	}

	full := byID["find_similar"].Cache
	if full == nil {
		t.Fatal("find_similar has no decoded cache policy")
	}
	if full.Mode != dag.CacheReadWrite || full.TTL != "1h" || full.Scope != dag.CacheRun {
		t.Errorf("find_similar cache = %+v, want {read_write 1h run}", full)
	}

	partial := byID["draft"].Cache
	if partial == nil || partial.Mode != dag.CacheOff {
		t.Fatalf("draft cache = %+v, want mode off", partial)
	}
	if partial.TTL != "" || partial.Scope != "" {
		t.Errorf("draft cache decoded absent keys as non-zero: %+v", partial)
	}

	if c := byID["start"].Cache; c != nil {
		t.Errorf("start (no cache key) cache = %+v, want nil", c)
	}
}

// TestDecodeBudget covers the run-level budget fields and the step-envelope
// budget caps (ADR-012, ticket 10.3).
func TestDecodeBudget(t *testing.T) {
	t.Parallel()

	def, err := dag.Decode(readFixture(t, "valid", "kitchen_sink.json"))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if def.BudgetUSD == nil || *def.BudgetUSD != 5 {
		t.Errorf("budget_usd = %v, want 5", def.BudgetUSD)
	}
	if def.OnBudgetExceeded != dag.BudgetPark {
		t.Errorf("on_budget_exceeded = %q, want park", def.OnBudgetExceeded)
	}

	byID := make(map[string]dag.Step, len(def.Steps))
	for _, s := range def.Steps {
		byID[s.ID] = s
	}

	fetch := byID["fetch"].Budget
	if fetch == nil || fetch.MaxUSD == nil || *fetch.MaxUSD != 0.5 {
		t.Errorf("fetch budget = %+v, want max_usd 0.5", fetch)
	}
	if fetch != nil && fetch.MaxTokens != 0 {
		t.Errorf("fetch budget decoded absent max_tokens as %d", fetch.MaxTokens)
	}

	draft := byID["draft"].Budget
	if draft == nil || draft.MaxUSD == nil || *draft.MaxUSD != 1.5 || draft.MaxTokens != 8000 {
		t.Errorf("draft budget = %+v, want {max_usd 1.5, max_tokens 8000}", draft)
	}

	if b := byID["start"].Budget; b != nil {
		t.Errorf("start (no budget key) budget = %+v, want nil", b)
	}
}

// TestDecodeStepConfig covers the single-config decode path the execution
// layer uses on run_steps rows (step type and raw config JSONB, no
// surrounding document): same strictness and normalization as Decode.
func TestDecodeStepConfig(t *testing.T) {
	t.Parallel()

	t.Run("typed decode", func(t *testing.T) {
		t.Parallel()
		cfg, err := dag.DecodeStepConfig(dag.StepSleep, []byte(`{"duration": "2s"}`))
		if err != nil {
			t.Fatalf("DecodeStepConfig: %v", err)
		}
		sleep, ok := cfg.(*dag.SleepConfig)
		if !ok || sleep.Duration != "2s" {
			t.Errorf("config = %#v, want *dag.SleepConfig{Duration: \"2s\"}", cfg)
		}

		cfg, err = dag.DecodeStepConfig(dag.StepFailNTimes, []byte(`{"n": 3}`))
		if err != nil {
			t.Fatalf("DecodeStepConfig: %v", err)
		}
		if fnt, fok := cfg.(*dag.FailNTimesConfig); !fok || fnt.N != 3 {
			t.Errorf("config = %#v, want *dag.FailNTimesConfig{N: 3}", cfg)
		}
	})

	t.Run("normalization applied", func(t *testing.T) {
		t.Parallel()
		cfg, err := dag.DecodeStepConfig(dag.StepEcho, []byte("{\"input\": {\n  \"a\": 1\n}}"))
		if err != nil {
			t.Fatalf("DecodeStepConfig: %v", err)
		}
		echo, ok := cfg.(*dag.EchoConfig)
		if !ok || string(echo.Input) != `{"a":1}` {
			t.Errorf("config = %#v, want compacted input {\"a\":1}", cfg)
		}
	})

	t.Run("absent config is nil", func(t *testing.T) {
		t.Parallel()
		cfg, err := dag.DecodeStepConfig(dag.StepNoop, nil)
		if err != nil || cfg != nil {
			t.Errorf("DecodeStepConfig(noop, nil) = %#v, %v, want nil, nil", cfg, err)
		}
	})

	t.Run("errors", func(t *testing.T) {
		t.Parallel()
		cases := []struct {
			name string
			st   dag.StepType
			raw  string
			want string // substring of the error
		}{
			{"unknown step type", "bogus", `{}`, `unknown step type "bogus"`},
			{"unknown field", dag.StepSleep, `{"durations": "2s"}`, "config.durations: unknown field"},
			{"wrong field type", dag.StepFailNTimes, `{"n": "three"}`, "config.n"},
			{"non-object config", dag.StepSleep, `[1]`, "expected object"},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				cfg, err := dag.DecodeStepConfig(tc.st, []byte(tc.raw))
				if err == nil {
					t.Fatalf("DecodeStepConfig = %#v, want error containing %q", cfg, tc.want)
				}
				if !strings.Contains(err.Error(), tc.want) {
					t.Errorf("error = %q, want substring %q", err, tc.want)
				}
			})
		}
	})
}

// TestDecodeCompactsOpaquePayloads verifies opaque payload fields are
// normalized to compact JSON so canonical re-encoding is byte-stable.
func TestDecodeCompactsOpaquePayloads(t *testing.T) {
	t.Parallel()

	def, err := dag.Decode(readFixture(t, "valid", "payloads.json"))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	tool, ok := def.Steps[0].Config.(*dag.ToolConfig)
	if !ok {
		t.Fatalf("steps[0] config = %T, want *dag.ToolConfig", def.Steps[0].Config)
	}
	want := `{"method":"POST","body":{"deep":[1,2,{"k":null}]}}`
	if string(tool.Input) != want {
		t.Errorf("tool input = %s, want compact %s", tool.Input, want)
	}
	echo, ok := def.Steps[1].Config.(*dag.EchoConfig)
	if !ok || string(echo.Input) != `["a","b"]` {
		t.Errorf("echo input = %#v, want compact [\"a\",\"b\"]", def.Steps[1].Config)
	}
}
