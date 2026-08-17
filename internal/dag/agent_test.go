package dag_test

import (
	"testing"

	"github.com/mathcslearner/agentloom/internal/dag"
)

// TestResolveAgentStepPrecedence pins the deterministic merge rules (ADR-016,
// ticket 14.1): a value present on the step wins, otherwise the role's default
// applies; the task (prompt/messages) always comes from the step; and the two
// envelope-level defaults (validation, context) are replaced wholesale by a
// step-level block, else inherited.
func TestResolveAgentStepPrecedence(t *testing.T) {
	t.Parallel()

	roleValidation := &dag.ValidationPolicy{Validators: []dag.ValidatorSpec{{Name: "json_schema"}}}
	roleContext := &dag.ContextSpec{Sources: []dag.ContextSource{{Kind: dag.SourceLiteral, Text: "role"}}}
	stepValidation := &dag.ValidationPolicy{Validators: []dag.ValidatorSpec{{Name: "regex"}}}
	stepContext := &dag.ContextSpec{Sources: []dag.ContextSource{{Kind: dag.SourceLiteral, Text: "step"}}}

	def := &dag.Definition{
		Agents: map[string]*dag.AgentDef{
			"critic": {
				Role:           "critic",
				System:         "role system",
				Model:          "mock/role-model",
				ModelFallbacks: []dag.ModelFallback{{Model: "mock/cheap"}},
				Tools:          []string{"json_transform"},
				MaxTokens:      256,
				Temperature:    f64(0.5),
				OutputFormat:   &dag.OutputFormat{Type: dag.OutputFormatJSON},
				Validation:     roleValidation,
				Context:        roleContext,
			},
		},
	}

	t.Run("inherits role defaults when step overrides nothing", func(t *testing.T) {
		t.Parallel()
		step := dag.Step{ID: "s", Type: dag.StepAgent, Config: &dag.AgentConfig{Agent: "critic", Prompt: "do it"}}
		got, err := dag.ResolveAgentStep(def, step)
		if err != nil {
			t.Fatalf("ResolveAgentStep: %v", err)
		}
		c := got.Config.(*dag.AgentConfig)
		if c.System != "role system" || c.Model != "mock/role-model" || c.MaxTokens != 256 {
			t.Errorf("config = %+v, want role defaults filled", c)
		}
		if c.Prompt != "do it" {
			t.Errorf("prompt = %q, want the step's task", c.Prompt)
		}
		if c.Temperature == nil || *c.Temperature != 0.5 {
			t.Errorf("temperature = %v, want role 0.5", c.Temperature)
		}
		if len(c.ModelFallbacks) != 1 || c.ModelFallbacks[0].Model != "mock/cheap" {
			t.Errorf("model_fallbacks = %+v, want role chain", c.ModelFallbacks)
		}
		if c.OutputFormat == nil || c.OutputFormat.Type != dag.OutputFormatJSON {
			t.Errorf("output_format = %+v, want role json", c.OutputFormat)
		}
		if len(c.Tools) != 1 || c.Tools[0] != "json_transform" {
			t.Errorf("tools = %v, want role toolset", c.Tools)
		}
		if got.Validation != roleValidation {
			t.Errorf("validation = %+v, want inherited role validation", got.Validation)
		}
		if got.Context != roleContext {
			t.Errorf("context = %+v, want inherited role context", got.Context)
		}
	})

	t.Run("step overrides win per field", func(t *testing.T) {
		t.Parallel()
		step := dag.Step{
			ID:   "s",
			Type: dag.StepAgent,
			Config: &dag.AgentConfig{
				Agent:        "critic",
				System:       "step system",
				Model:        "mock/step-model",
				Prompt:       "do it",
				MaxTokens:    999,
				Temperature:  f64(0),
				OutputFormat: &dag.OutputFormat{Type: dag.OutputFormatJSONSchema, Schema: []byte(`{"type":"object"}`)},
				Tools:        []string{"http_request"},
			},
			Validation: stepValidation,
			Context:    stepContext,
		}
		got, err := dag.ResolveAgentStep(def, step)
		if err != nil {
			t.Fatalf("ResolveAgentStep: %v", err)
		}
		c := got.Config.(*dag.AgentConfig)
		if c.System != "step system" || c.Model != "mock/step-model" || c.MaxTokens != 999 {
			t.Errorf("config = %+v, want step overrides", c)
		}
		if c.Temperature == nil || *c.Temperature != 0 {
			t.Errorf("temperature = %v, want explicit step 0", c.Temperature)
		}
		if c.OutputFormat == nil || c.OutputFormat.Type != dag.OutputFormatJSONSchema {
			t.Errorf("output_format = %+v, want step override", c.OutputFormat)
		}
		if len(c.Tools) != 1 || c.Tools[0] != "http_request" {
			t.Errorf("tools = %v, want step toolset", c.Tools)
		}
		if got.Validation != stepValidation {
			t.Errorf("validation not overridden by step block")
		}
		if got.Context != stepContext {
			t.Errorf("context not overridden by step block")
		}
	})

	t.Run("explicit empty toolset forbids all tools", func(t *testing.T) {
		t.Parallel()
		step := dag.Step{ID: "s", Type: dag.StepAgent, Config: &dag.AgentConfig{Agent: "critic", Prompt: "x", Tools: []string{}}}
		got, err := dag.ResolveAgentStep(def, step)
		if err != nil {
			t.Fatalf("ResolveAgentStep: %v", err)
		}
		if c := got.Config.(*dag.AgentConfig); c.Tools == nil || len(c.Tools) != 0 {
			t.Errorf("tools = %v, want an explicit empty (non-nil) list, not the inherited role toolset", c.Tools)
		}
	})

	t.Run("errors on unknown ref", func(t *testing.T) {
		t.Parallel()
		step := dag.Step{ID: "s", Type: dag.StepAgent, Config: &dag.AgentConfig{Agent: "nobody", Prompt: "x"}}
		if _, err := dag.ResolveAgentStep(def, step); err == nil {
			t.Fatal("want error for undeclared agent ref")
		}
	})

	t.Run("errors on non-agent step", func(t *testing.T) {
		t.Parallel()
		step := dag.Step{ID: "s", Type: dag.StepLLM, Config: &dag.LLMConfig{Model: "x", Prompt: "y"}}
		if _, err := dag.ResolveAgentStep(def, step); err == nil {
			t.Fatal("want error for non-agent step")
		}
	})
}

// TestResolveAgentStepIsPure verifies the merge does not mutate the role or the
// authored step's config — a run instantiating many agent steps against one
// role must not have earlier merges leak into later ones.
func TestResolveAgentStepIsPure(t *testing.T) {
	t.Parallel()
	role := &dag.AgentDef{System: "role", Model: "mock/m", Tools: []string{"t"}}
	def := &dag.Definition{Agents: map[string]*dag.AgentDef{"a": role}}
	authored := &dag.AgentConfig{Agent: "a", Prompt: "task"}
	step := dag.Step{ID: "s", Type: dag.StepAgent, Config: authored}

	if _, err := dag.ResolveAgentStep(def, step); err != nil {
		t.Fatalf("ResolveAgentStep: %v", err)
	}
	if authored.System != "" || authored.Model != "" || authored.Tools != nil {
		t.Errorf("authored config was mutated: %+v", authored)
	}
	if role.System != "role" || role.Model != "mock/m" {
		t.Errorf("role was mutated: %+v", role)
	}
}
