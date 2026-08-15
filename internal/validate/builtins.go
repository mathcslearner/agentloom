package validate

// The built-in deterministic validators (ticket 11.2, ADR-013): json_schema,
// regex, contains, cel, and numeric_range. All are pure functions of
// (output, config) — cacheable, never side-effectful, never cost-bearing —
// and fast: the ones that compile an artifact (json_schema, regex, cel) do
// so once per distinct config through a compileCache, so no attempt pays a
// recompilation (the "compiled artifacts reused across attempts" criterion).
//
// This file collects the shared decode/stringify helpers and rewrites
// NewBuiltins to register the five. Each validator lives in its own file
// (json_schema.go, regex.go, contains.go, cel.go, numeric_range.go).

import (
	"bytes"
	"encoding/json"
	"fmt"

	"github.com/mathcslearner/agentloom/internal/plugin"
)

// The built-in validator versions (ADR-009). They ship at 1.0.0 — real
// semantics from the first release, no dev-stub phase.
const (
	jsonSchemaVersion   = "1.0.0"
	regexVersion        = "1.0.0"
	containsVersion     = "1.0.0"
	celVersion          = "1.0.0"
	numericRangeVersion = "1.0.0"
)

// The built-in validator names — the values a chain entry's `name` selects.
const (
	jsonSchemaName   = "json_schema"
	regexName        = "regex"
	containsName     = "contains"
	celName          = "cel"
	numericRangeName = "numeric_range"
)

// deterministicCaps is the ADR-009 flag set every deterministic built-in
// carries: a pure function of output and config, so cacheable and nothing
// else. (The llm_judge, 11.5, adds cost_bearing.)
var deterministicCaps = plugin.Capabilities{Cacheable: true}

// builtinConfigSchema reflects a validator's config struct into its JSON
// Schema — the same generator ConfigSchema exposes, wrapped so the built-in
// manifests can panic on the unreachable reflect failure (a fixed struct)
// rather than thread an error out of Manifest.
func builtinConfigSchema(v any) json.RawMessage {
	schema, err := ConfigSchema(v)
	if err != nil {
		panic(fmt.Sprintf("validate: reflecting config schema for %T: %v", v, err))
	}
	return schema
}

// NewBuiltins returns the registry of built-in validators (ticket 11.2): the
// five deterministic validators json_schema, regex, contains, cel, and
// numeric_range. 11.5 adds llm_judge. Both cmd/worker (which runs the
// validate stage) and cmd/api (which lists the catalog) call this, so the
// fleet and the /v1/plugins listing describe the same validators.
func NewBuiltins() (*Registry, error) {
	return NewRegistry(
		NewJSONSchema(),
		NewRegex(),
		NewContains(),
		NewCEL(),
		NewNumericRange(),
	)
}

// strictDecodeConfig decodes a validator's config into dst with
// DisallowUnknownFields — belt-and-suspenders behind the config schema's
// additionalProperties: false, matching the tools/dag decode discipline.
// Empty or nil config decodes to the zero struct (the caller then rejects a
// missing required field, or the pre-flight schema check already did).
func strictDecodeConfig(raw []byte, dst any) error {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return err
	}
	if dec.More() {
		return fmt.Errorf("unexpected trailing data after JSON value")
	}
	return nil
}

// stringOf renders a validated target value as the string a text-oriented
// validator (regex, contains) scrutinizes. A JSON string target is its
// contents (so a regex sees the model's answer, not a quoted string); any
// other JSON value is its compact JSON encoding (resolveTarget already
// marshaled it whitespace-free), so a validator over a structured target has
// well-defined grep-like semantics rather than failing outright.
func stringOf(value json.RawMessage) string {
	var s string
	if err := json.Unmarshal(value, &s); err == nil {
		return s
	}
	return string(value)
}

// decodeValue decodes a validated target value into a native Go value
// (map[string]any / []any / string / float64 / bool / nil) — the form CEL
// activations and numeric parsing consume. A decode failure is returned so
// the caller can raise an invalid_json issue rather than panic.
func decodeValue(value json.RawMessage) (any, error) {
	var v any
	if err := json.Unmarshal(value, &v); err != nil {
		return nil, err
	}
	return v, nil
}
