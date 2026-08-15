package validate

// Validator registry (ticket 11.1, ADR-013). Registry is the typed facade
// over plugin.Registry for kind validator — the pattern exec.Registry,
// tools.Registry, and llm.Registry established: type-safe lookup at the
// call site, the one shared registration discipline (invalid / duplicate →
// boot error) and listing shape underneath, plus the validator-specific job
// of compiling each validator's config JSON Schema at registration so
// ValidateConfig can enforce it pre-flight (the tools.Registry precedent).

import (
	"bytes"
	"errors"
	"fmt"
	"strings"

	jsonschema "github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/mathcslearner/agentloom/internal/plugin"
)

// registered is one validator and its compiled config schema.
type registered struct {
	validator Validator
	schema    *jsonschema.Schema
}

// Registry maps validator names to their Validator implementations. It is a
// typed facade over the generic plugin.Registry (kind validator, name =
// validator name; ADR-009), populated at startup and read-only afterwards,
// so lookups need no lock; Register is not safe for concurrent use.
type Registry struct {
	plugins *plugin.Registry
	byName  map[string]registered
}

// NewRegistry builds a registry from the given validators. It fails on the
// same conditions as Register (nil validator, invalid manifest, wrong kind,
// missing/uncompilable config schema, duplicate name).
func NewRegistry(vs ...Validator) (*Registry, error) {
	r := &Registry{plugins: plugin.NewRegistry(), byName: make(map[string]registered)}
	for _, v := range vs {
		if err := r.Register(v); err != nil {
			return nil, err
		}
	}
	return r, nil
}

// NewBuiltins returns the registry of built-in validators. In 11.1 it is
// empty — the SPI ships with no built-ins; 11.2 registers json_schema,
// regex, contains, cel, and numeric_range, and 11.5 adds llm_judge. It
// exists now so cmd/worker and cmd/api wire the same constructor across the
// milestone, and the empty registry is a valid one (a step naming a
// validator then fails permanent at resolve time, ADR-013).
func NewBuiltins() (*Registry, error) {
	return NewRegistry()
}

// Register adds one validator under its manifest's name, rejecting a nil
// validator, a manifest that fails validation or does not declare kind
// validator, a missing or uncompilable config schema, and a duplicate name
// — each a wiring bug worth failing startup over (ADR-009). The config
// schema is compiled once here so ValidateConfig takes no compilation cost
// per attempt (11.2's "compiled artifacts reused across attempts").
func (r *Registry) Register(v Validator) error {
	if v == nil {
		return errors.New("validate: Register called with nil validator")
	}
	m := v.Manifest()
	if m.Kind != plugin.KindValidator {
		return fmt.Errorf("validate: validator %q declares plugin kind %q — validators must register as %q", m.Name, m.Kind, plugin.KindValidator)
	}
	if m.ConfigSchema == nil {
		// Every validator must declare its config schema — it is the SPI's
		// validation contract, and a validator that takes no config declares
		// the empty object schema explicitly (EmptyConfigSchema) rather than
		// nil.
		return fmt.Errorf("validate: validator %q has no config schema", m.Name)
	}
	schema, err := compileSchema(m.Name, m.ConfigSchema)
	if err != nil {
		return err
	}
	if err := r.plugins.Register(m, v); err != nil {
		var dup *plugin.DuplicateError
		if errors.As(err, &dup) {
			return fmt.Errorf("validate: validator %q registered twice", m.Name)
		}
		return fmt.Errorf("validate: registering validator %q: %w", m.Name, err)
	}
	r.byName[m.Name] = registered{validator: v, schema: schema}
	return nil
}

// Get returns the validator registered under name, or an
// *UnknownValidatorError (unwrapping to ErrUnknownValidator) when none is
// registered.
func (r *Registry) Get(name string) (Validator, error) {
	e, ok := r.byName[name]
	if !ok {
		return nil, &UnknownValidatorError{Name: name}
	}
	return e.validator, nil
}

// ValidateConfig checks a chain entry's config against the named
// validator's compiled config schema — the pre-flight gate (ADR-013): a
// violation is a *ConfigValidationError (permanent) and the engine must not
// run the validator. An unknown validator is an *UnknownValidatorError.
// Absent config (nil/empty) validates as the empty object {} — "no config
// key" means "empty config": a no-config validator (EmptyConfigSchema)
// accepts it, while a validator with required fields still rejects it.
func (r *Registry) ValidateConfig(name string, config []byte) error {
	e, ok := r.byName[name]
	if !ok {
		return &UnknownValidatorError{Name: name}
	}
	raw := config
	if len(bytes.TrimSpace(raw)) == 0 {
		raw = []byte("{}")
	}
	inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		return &ConfigValidationError{Validator: name, Detail: fmt.Sprintf("config is not valid JSON: %v", err)}
	}
	if err := e.schema.Validate(inst); err != nil {
		return &ConfigValidationError{Validator: name, Detail: validationDetail(err)}
	}
	return nil
}

// Manifests returns every registered validator's manifest, sorted — the
// slice the API folds into GET /v1/plugins.
func (r *Registry) Manifests() []plugin.Manifest {
	return r.plugins.ManifestsByKind(plugin.KindValidator)
}

// Names returns every registered validator name, sorted.
func (r *Registry) Names() []string {
	ms := r.Manifests()
	out := make([]string, 0, len(ms))
	for _, m := range ms {
		out = append(out, m.Name)
	}
	return out
}

// compileSchema compiles a validator's config schema (2020-12) once at
// registration. An uncompilable schema is a build bug — the schema is
// generated from the validator's own Go struct — so it fails startup.
func compileSchema(name string, raw []byte) (*jsonschema.Schema, error) {
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("validate: validator %q config schema is not valid JSON: %w", name, err)
	}
	c := jsonschema.NewCompiler()
	// A stable synthetic URL; the schema is self-contained (no external
	// $refs), so the loader is never consulted.
	const loc = "mem://validator-config/schema.json"
	if err := c.AddResource(loc, doc); err != nil {
		return nil, fmt.Errorf("validate: validator %q config schema: %w", name, err)
	}
	schema, err := c.Compile(loc)
	if err != nil {
		return nil, fmt.Errorf("validate: compiling validator %q config schema: %w", name, err)
	}
	return schema, nil
}

// validationDetail renders a jsonschema validation failure into a compact
// single-line, structure-only description (missing/typed/unknown fields),
// never echoing instance values — the tools.validationDetail convention.
func validationDetail(err error) string {
	var ve *jsonschema.ValidationError
	if !errors.As(err, &ve) {
		return err.Error()
	}
	return strings.Join(strings.Fields(ve.Error()), " ")
}
