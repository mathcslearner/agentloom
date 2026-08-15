package validate

// The json_schema validator (ticket 11.2, ADR-013): checks a step output
// (or a targeted sub-tree) against a JSON Schema the author supplies,
// reporting a structured issue per violating location. The author's schema
// is compiled once per distinct config through a compileCache, so no attempt
// pays a recompilation, and an uncompilable schema is caught pre-flight
// (CompileConfig) as a permanent config error before any spend.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	jsonschema "github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/mathcslearner/agentloom/internal/plugin"
)

// jsonSchemaConfig is json_schema's config. Schema is the JSON Schema
// (2020-12) document the output must satisfy — an arbitrary schema object,
// so it reflects to the permissive `true` schema (any JSON accepted at the
// config-schema layer; CompileConfig does the real check that it compiles).
type jsonSchemaConfig struct {
	// Schema is the JSON Schema document the target value must satisfy.
	Schema json.RawMessage `json:"schema"`
}

// JSONSchema is the built-in json_schema validator. It is cacheable (a pure
// function of output + config) and holds a compileCache of author schemas
// keyed by config bytes.
type JSONSchema struct {
	cache *compileCache[*jsonschema.Schema]
}

// NewJSONSchema builds the json_schema validator with its compile cache.
func NewJSONSchema() *JSONSchema {
	v := &JSONSchema{}
	v.cache = newCompileCache(v.compile)
	return v
}

// Manifest implements Validator: kind validator, cacheable only.
func (v *JSONSchema) Manifest() plugin.Manifest {
	return plugin.Manifest{
		Kind:         plugin.KindValidator,
		Name:         jsonSchemaName,
		Version:      jsonSchemaVersion,
		Description:  "Validate a step output (or a targeted sub-tree) against a JSON Schema.",
		Capabilities: deterministicCaps,
		ConfigSchema: builtinConfigSchema(&jsonSchemaConfig{}),
	}
}

// CompileConfig implements ConfigCompiler: the pre-flight gate. It requires a
// non-empty `schema` and that the schema compiles; a failure is a permanent
// config error (the same config fails identically every attempt). Success
// warms the cache.
func (v *JSONSchema) CompileConfig(config json.RawMessage) error {
	_, err := v.cache.get(config)
	return err
}

// compile is the pure builder behind the cache: decode the config, require a
// schema, and compile it into a reusable *jsonschema.Schema.
func (v *JSONSchema) compile(config []byte) (*jsonschema.Schema, error) {
	var cfg jsonSchemaConfig
	if err := strictDecodeConfig(config, &cfg); err != nil {
		return nil, fmt.Errorf("decoding config: %v", err)
	}
	if len(bytes.TrimSpace(cfg.Schema)) == 0 {
		return nil, fmt.Errorf("%q is required", "schema")
	}
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(cfg.Schema))
	if err != nil {
		return nil, fmt.Errorf("schema is not valid JSON: %v", err)
	}
	c := jsonschema.NewCompiler()
	// A stable synthetic URL; author schemas are self-contained (external
	// $ref resolution is disabled by never registering a loader), so the
	// loader is never consulted.
	const loc = "mem://json-schema-validator/schema.json"
	if err := c.AddResource(loc, doc); err != nil {
		return nil, fmt.Errorf("schema: %v", err)
	}
	schema, err := c.Compile(loc)
	if err != nil {
		return nil, fmt.Errorf("compiling schema: %v", err)
	}
	return schema, nil
}

// Validate checks the target value against the compiled schema. The value is
// decoded as JSON; if it is itself a JSON string (the /text default for
// llm-family steps), its contents are parsed as JSON and validated — an LLM
// that was asked for JSON emits it as text, so json_schema judges the parsed
// document. A string that is not valid JSON is a fail verdict (code
// invalid_json), never a panic. A schema violation is a fail verdict with
// one issue per violating instance location.
func (v *JSONSchema) Validate(_ context.Context, in Input) (Verdict, error) {
	schema, err := v.cache.get(in.Config)
	if err != nil {
		// Unreachable on the normal path: the pre-flight gate compiled this
		// config already. If it somehow errors here, it is a permanent config
		// problem — surface it as a transport error so the engine fails the
		// step permanently rather than silently passing.
		return Verdict{}, Permanentf(jsonSchemaName, err, "config no longer compiles")
	}

	instance, ferr := jsonInstance(in.Value)
	if ferr != nil {
		return FailVerdict(Issue{
			Validator: jsonSchemaName, Code: "invalid_json", Path: "",
			Message: "target is a string but not valid JSON, so it cannot be schema-checked",
		}), nil
	}

	if verr := schema.Validate(instance); verr != nil {
		return FailVerdict(schemaIssues(verr)...), nil
	}
	return PassVerdict(), nil
}

// jsonInstance decodes a target value into the Go value the schema validates.
// A JSON string value has its contents re-parsed as JSON (LLM JSON arrives as
// text); a string whose contents are not JSON returns an error the caller
// turns into an invalid_json fail verdict. Any non-string JSON value is used
// as decoded.
func jsonInstance(value json.RawMessage) (any, error) {
	decoded, err := decodeValue(value)
	if err != nil {
		return nil, err
	}
	s, ok := decoded.(string)
	if !ok {
		return decoded, nil
	}
	// The target resolved to a JSON string; parse its contents as JSON.
	inner, err := jsonschema.UnmarshalJSON(strings.NewReader(s))
	if err != nil {
		return nil, err
	}
	return inner, nil
}

// schemaIssues flattens a jsonschema validation error into one Issue per
// leaf violation (the deepest causes), each carrying the offending
// instance-location pointer and a keyword-derived code. Messages are
// structure-only (jsonschema reports types and schema-side constraints, never
// instance values), so no output content reaches a verdict.
func schemaIssues(err error) []Issue {
	var ve *jsonschema.ValidationError
	if !errors.As(err, &ve) {
		return []Issue{{Validator: jsonSchemaName, Code: "schema_violation", Message: compactLine(err.Error())}}
	}
	var issues []Issue
	var walk func(e *jsonschema.ValidationError)
	walk = func(e *jsonschema.ValidationError) {
		if len(e.Causes) == 0 {
			issues = append(issues, Issue{
				Validator: jsonSchemaName,
				Code:      keywordCode(e),
				Path:      instancePointer(e.InstanceLocation),
				Message:   leafMessage(e),
			})
			return
		}
		for _, c := range e.Causes {
			walk(c)
		}
	}
	walk(ve)
	if len(issues) == 0 {
		issues = append(issues, Issue{
			Validator: jsonSchemaName, Code: keywordCode(ve),
			Path: instancePointer(ve.InstanceLocation), Message: leafMessage(ve),
		})
	}
	return issues
}

// keywordCode maps a validation error's failing keyword to a stable issue
// code. A small alias table gives the most common keywords friendly codes;
// anything else falls back to the keyword's snake_case form.
func keywordCode(e *jsonschema.ValidationError) string {
	kw := lastKeyword(e)
	switch kw {
	case "":
		return "schema_violation"
	case "type":
		return "type_mismatch"
	case "additionalProperties":
		return "additional_properties"
	case "minimum", "exclusiveMinimum":
		return "below_minimum"
	case "maximum", "exclusiveMaximum":
		return "above_maximum"
	case "minLength":
		return "too_short"
	case "maxLength":
		return "too_long"
	case "pattern":
		return "pattern_no_match"
	default:
		return snakeCase(kw)
	}
}

// lastKeyword returns the final token of the error's keyword path — the
// keyword that failed (e.g. "type", "required", "minLength"). Empty when the
// path is empty (a whole-schema failure).
func lastKeyword(e *jsonschema.ValidationError) string {
	path := e.ErrorKind.KeywordPath()
	if len(path) == 0 {
		return ""
	}
	return path[len(path)-1]
}

// instancePointer renders a jsonschema instance location (a slice of decoded
// reference tokens) as an RFC 6901 JSON pointer relative to the validated
// target. An empty location is the whole value ("").
func instancePointer(tokens []string) string {
	if len(tokens) == 0 {
		return ""
	}
	var sb strings.Builder
	for _, t := range tokens {
		sb.WriteByte('/')
		sb.WriteString(escapePointerToken(t))
	}
	return sb.String()
}

// leafMessage renders a leaf validation error's human message with the
// instance-location prefix stripped (the pointer already lives in Path) and
// whitespace collapsed. jsonschema's leaf messages describe types and
// schema-side constraints, never the instance value, so they are safe.
func leafMessage(e *jsonschema.ValidationError) string {
	s := compactLine(e.Error())
	// The default rendering prefixes a leaf with: at "/ptr": <message>.
	if strings.HasPrefix(s, `at "`) {
		if i := strings.Index(s, `": `); i >= 0 {
			return s[i+len(`": `):]
		}
	}
	return s
}

// escapePointerToken RFC 6901-escapes a single reference token ("~" → "~0",
// "/" → "~1"), the inverse of unescapePointerToken.
func escapePointerToken(tok string) string {
	if !strings.ContainsAny(tok, "~/") {
		return tok
	}
	r := strings.NewReplacer("~", "~0", "/", "~1")
	return r.Replace(tok)
}

// snakeCase lowercases a camelCase JSON Schema keyword into snake_case
// (minItems → min_items). ASCII only — keywords are ASCII.
func snakeCase(s string) string {
	var sb strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'A' && c <= 'Z' {
			if i > 0 {
				sb.WriteByte('_')
			}
			sb.WriteByte(c - 'A' + 'a')
			continue
		}
		sb.WriteByte(c)
	}
	return sb.String()
}

// compactLine collapses all runs of whitespace (including newlines) into
// single spaces, so a multi-line library error becomes one log-safe line.
func compactLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}
