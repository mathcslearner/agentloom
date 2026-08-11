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
	"unknown_top_field.json":      {"descriptionz: unknown field"},
	"wrong_type_name.json":        {"name: expected string, got number"},
	"steps_not_array.json":        {"steps: expected array, got object"},
	"step_missing_id_type.json":   {"steps[0].id: required field is missing", "steps[0].type: required field is missing"},
	"unknown_step_type.json":      {`steps[0].type: unknown step type "llmz"`},
	"unknown_step_field.json":     {"steps[0].retires: unknown field"},
	"config_unknown_field.json":   {"steps[0].config.modle: unknown field"},
	"config_wrong_type.json":      {"steps[0].config.max_tokens: expected integer, got string"},
	"config_nested_unknown.json":  {"steps[0].config.messages[0].contnt: unknown field"},
	"config_not_object.json":      {"steps[0].config: expected object, got array"},
	"join_bad_mode.json":          {`steps[0].config.mode: unknown join mode "eventually"`},
	"edge_missing_from.json":      {"edges[0].from: required field is missing"},
	"unknown_edge_field.json":     {"edges[0].whenever: unknown field"},
	"unknown_edge_type.json":      {`edges[0].type: unknown edge type "loopy"`},
	"param_missing_type.json":     {"params.goal.type: required field is missing"},
	"bad_param_type.json":         {`params.goal.type: unknown param type "text"`},
	"param_entry_not_object.json": {"params.goal: expected object, got string"},
	"ui_not_object.json":          {"ui: expected object, got array"},
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

	join, ok := byID["gather"].Config.(*dag.JoinConfig)
	if !ok || join.Mode != dag.JoinAll {
		t.Errorf("gather config = %#v, want *dag.JoinConfig{Mode: all}", byID["gather"].Config)
	}

	if cfg := byID["start"].Config; cfg != nil {
		t.Errorf("start (noop, no config key) config = %#v, want nil", cfg)
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
