// Package validate defines agentloom's validator SPI (ticket 11.1,
// ADR-013): the Validator interface a step's validation chain runs over its
// output, the verdict model those validators produce, the typed Registry
// the engine resolves a chain against, and the chain runner that aggregates
// per-validator verdicts into one persisted verdict.
//
// A validator is a single, named, schema-described judgment on a step
// output: it declares the JSON Schema of its config (generated from the Go
// struct it decodes, per the ADR-003 invariant), the framework validates a
// chain entry's config against that schema BEFORE the validator runs (bad
// config → permanent failure, never a call — the tool-args gate), and
// Validate renders a pass/fail Verdict with structured issues.
//
// Like internal/tools and internal/llm this is a leaf package: it imports
// internal/plugin (the manifest vocabulary) and internal/dag (the ADR-006
// error-class vocabulary) and stdlib/jsonschema, never internal/exec or the
// engine. The exec package owns nothing here; the engine's validate stage
// (engine/validate.go) resolves a chain, runs it, and routes the verdict.
package validate

import (
	"context"
	"encoding/json"
	"log/slog"

	invopop "github.com/invopop/jsonschema"

	"github.com/mathcslearner/agentloom/internal/dag"
	"github.com/mathcslearner/agentloom/internal/plugin"
)

// Validator is one named output check (ADR-009's validator kind).
// Implementations must be safe for concurrent Validate calls — one
// Validator instance serves every step that names it across the worker's
// consumer goroutines — and must return promptly once ctx is canceled.
type Validator interface {
	// Manifest is the validator's self-description (ADR-009): kind
	// validator, a unique name (^[a-z][a-z0-9_]*$), a semver version, its
	// capability flags (deterministic validators cacheable; the llm_judge
	// cost_bearing — 11.5), and — required for every validator — the JSON
	// Schema of its config in ConfigSchema. The registry validates the
	// identity and compiles the schema at registration.
	Manifest() plugin.Manifest

	// Validate renders a judgment on the input. in.Config has already been
	// validated against the validator's config schema (the engine rejects
	// bad config before calling Validate), so Validate decodes it without
	// re-checking shape. It returns a Verdict (pass or fail with issues);
	// it returns a non-nil error ONLY when it cannot render a judgment at
	// all — a *validate.Error to declare transient/permanent (an llm_judge
	// provider error, 11.5), or ctx cancellation/deadline passed through
	// unwrapped so the engine keeps the timeout/cancelled judgment. A fail
	// verdict is a successful validation with a negative result, NOT an
	// error.
	Validate(ctx context.Context, in Input) (Verdict, error)
}

// ConfigCompiler is an optional extra interface a Validator implements when
// its config compiles into an artifact whose *content* can be invalid in a
// way the config JSON Schema cannot express — an unparseable regex, a CEL
// expression that fails to typecheck (or is not a boolean predicate), a JSON
// Schema document that will not compile, a numeric_range with no bound
// (ticket 11.2, ADR-013).
//
// The registry calls CompileConfig from ValidateConfig, AFTER the schema
// check passes, so a validator that also implements ConfigCompiler gets a
// second, deeper pre-flight gate: an error here becomes a
// *ConfigValidationError (permanent), fired at claim before the executor
// runs and before any money is spent — exactly like a schema violation, so
// the validator's Validate is never reached with config it cannot compile.
// Because it is the pre-flight gate, CompileConfig also warms the
// validator's compile cache, so the artifact Validate later needs is already
// built (no per-attempt recompilation).
//
// The method must be pure in config and safe for concurrent calls (the
// registry is read-only after boot, but ValidateConfig runs on the claim
// path across consumer goroutines). It returns nil for a config the schema
// admitted and the artifact accepts.
type ConfigCompiler interface {
	CompileConfig(config json.RawMessage) error
}

// Input is everything a Validator sees about the output it is judging. The
// engine builds one per validator from the completed step's output and the
// chain entry's config; validators must treat it as read-only.
type Input struct {
	// StepType is the type of the step that produced the output (e.g. "llm")
	// — a validator may adapt (an llm output's answer is under /text). The
	// chain's target selection has already been applied to Value.
	StepType dag.StepType
	// Output is the step's whole output payload — the full JSON the
	// executor produced. Validators that scrutinize the envelope (rather
	// than a targeted sub-tree) read this.
	Output json.RawMessage
	// Value is the sub-tree the chain entry's target JSON pointer selected
	// from Output; equal to Output when the entry had no target. This is
	// what a targeted validator (json_schema on /analysis, cel on /text)
	// judges.
	Value json.RawMessage
	// Config is the validator's decoded config — the chain entry's `config`
	// object, already schema-validated by the registry.
	Config json.RawMessage
	// Attempt is the 1-based semantic attempt number (11.4): a validator may
	// loosen or tighten across semantic re-attempts. 1 in 11.1 (no loop).
	Attempt int
	// Logger carries the run/step/attempt log fields. May be nil (validators
	// fall back to slog.Default()).
	Logger *slog.Logger
}

// ConfigSchema reflects a validator's config struct into a standalone JSON
// Schema (2020-12) document — the same generator and reflector settings
// dag.StepConfigSchema and tools.argsSchema use: strict
// additionalProperties: false mirroring the validators' strict config
// decoding, fields required unless tagged omitempty or pointer-typed.
// Generated from the Go struct the validator decodes, so the served schema
// and the config validation can never drift. A validator with no config
// declares the empty object schema via EmptyConfigSchema.
func ConfigSchema(v any) (json.RawMessage, error) {
	r := &invopop.Reflector{Anonymous: true, ExpandedStruct: true}
	schema := r.Reflect(v)
	schema.Version = invopop.Version
	data, err := json.Marshal(schema)
	if err != nil {
		return nil, err
	}
	return data, nil
}

// EmptyConfigSchema is the config schema a validator taking no config
// declares — the SPI requires an explicit schema (nil is rejected), so a
// no-config validator uses this rather than leaving ConfigSchema nil.
func EmptyConfigSchema() json.RawMessage {
	return json.RawMessage(`{"type":"object","additionalProperties":false}`)
}
