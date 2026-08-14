package tools

// Tool registry (ticket 8.7, ADR-009). Registry is the typed facade over
// plugin.Registry for kind tool — the pattern exec.Registry and
// llm.Registry established: type-safe lookup at the call site, the one
// shared registration discipline (invalid / duplicate → boot error) and
// listing shape underneath, plus the tool-specific job of compiling each
// tool's args JSON Schema at registration so ValidateArgs can enforce it
// pre-invocation.

import (
	"bytes"
	"errors"
	"fmt"
	"strings"

	jsonschema "github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/mathcslearner/agentloom/internal/plugin"
)

// registered is one tool and its compiled args schema.
type registered struct {
	tool   Tool
	schema *jsonschema.Schema
}

// Registry maps tool names to their Tool implementations. It is a typed
// facade over the generic plugin.Registry (kind tool, name = tool name;
// ADR-009), populated at startup and read-only afterwards, so lookups
// need no lock; Register is not safe for concurrent use.
type Registry struct {
	plugins *plugin.Registry
	byName  map[string]registered
}

// NewRegistry builds a registry from the given tools. It fails on the
// same conditions as Register (nil tool, invalid manifest, wrong kind,
// missing/uncompilable args schema, duplicate name).
func NewRegistry(ts ...Tool) (*Registry, error) {
	r := &Registry{plugins: plugin.NewRegistry(), byName: make(map[string]registered)}
	for _, t := range ts {
		if err := r.Register(t); err != nil {
			return nil, err
		}
	}
	return r, nil
}

// Register adds one tool under its manifest's name, rejecting a nil tool,
// a manifest that fails validation or does not declare kind tool, a
// missing or uncompilable args schema, and a duplicate name — each a
// wiring bug worth failing startup over (ADR-009). The args schema is
// compiled once here so ValidateArgs takes no compilation cost per call.
func (r *Registry) Register(t Tool) error {
	if t == nil {
		return errors.New("tools: Register called with nil tool")
	}
	m := t.Manifest()
	if m.Kind != plugin.KindTool {
		return fmt.Errorf("tools: tool %q declares plugin kind %q — tools must register as %q", m.Name, m.Kind, plugin.KindTool)
	}
	if m.ConfigSchema == nil {
		// Every tool must declare its args schema — it is the SPI's
		// validation contract, and a tool that takes no args declares the
		// empty object schema explicitly rather than nil.
		return fmt.Errorf("tools: tool %q has no args schema", m.Name)
	}
	schema, err := compileSchema(m.Name, m.ConfigSchema)
	if err != nil {
		return err
	}
	if err := r.plugins.Register(m, t); err != nil {
		var dup *plugin.DuplicateError
		if errors.As(err, &dup) {
			return fmt.Errorf("tools: tool %q registered twice", m.Name)
		}
		return fmt.Errorf("tools: registering tool %q: %w", m.Name, err)
	}
	r.byName[m.Name] = registered{tool: t, schema: schema}
	return nil
}

// Get returns the tool registered under name, or an *UnknownToolError
// (unwrapping to ErrUnknownTool) when none is registered.
func (r *Registry) Get(name string) (Tool, error) {
	e, ok := r.byName[name]
	if !ok {
		return nil, &UnknownToolError{Name: name}
	}
	return e.tool, nil
}

// ValidateArgs checks a step's rendered args against the named tool's
// compiled args schema — the pre-invocation gate (ticket 8.7): a
// violation is an *ArgsValidationError (permanent) and the caller must
// not invoke the tool. An unknown tool is an *UnknownToolError. Absent
// args (nil) validate as JSON null, which a schema requiring an object
// correctly rejects.
func (r *Registry) ValidateArgs(name string, args []byte) error {
	e, ok := r.byName[name]
	if !ok {
		return &UnknownToolError{Name: name}
	}
	raw := args
	if len(bytes.TrimSpace(raw)) == 0 {
		raw = []byte("null")
	}
	inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		return &ArgsValidationError{Tool: name, Detail: fmt.Sprintf("args are not valid JSON: %v", err)}
	}
	if err := e.schema.Validate(inst); err != nil {
		return &ArgsValidationError{Tool: name, Detail: validationDetail(err)}
	}
	return nil
}

// Manifests returns every registered tool's manifest, sorted — the slice
// the API folds into GET /v1/plugins.
func (r *Registry) Manifests() []plugin.Manifest {
	return r.plugins.ManifestsByKind(plugin.KindTool)
}

// Names returns every registered tool name, sorted.
func (r *Registry) Names() []string {
	ms := r.Manifests()
	out := make([]string, 0, len(ms))
	for _, m := range ms {
		out = append(out, m.Name)
	}
	return out
}

// compileSchema compiles a tool's args schema (2020-12) once at
// registration. An uncompilable schema is a build bug — the schema is
// generated from the tool's own Go struct — so it fails startup.
func compileSchema(name string, raw []byte) (*jsonschema.Schema, error) {
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("tools: tool %q args schema is not valid JSON: %w", name, err)
	}
	c := jsonschema.NewCompiler()
	// A stable synthetic URL; the schema is self-contained (no external
	// $refs), so the loader is never consulted.
	const loc = "mem://tool-args/schema.json"
	if err := c.AddResource(loc, doc); err != nil {
		return nil, fmt.Errorf("tools: tool %q args schema: %w", name, err)
	}
	schema, err := c.Compile(loc)
	if err != nil {
		return nil, fmt.Errorf("tools: compiling tool %q args schema: %w", name, err)
	}
	return schema, nil
}

// validationDetail renders a jsonschema validation failure into a compact
// single-line, structure-only description (missing/typed/unknown fields),
// never echoing instance values.
func validationDetail(err error) string {
	var ve *jsonschema.ValidationError
	if !errors.As(err, &ve) {
		return err.Error()
	}
	// The default multi-line output lists each keyword violation; collapse
	// it to one line so it reads well in an error envelope and a log field.
	return strings.Join(strings.Fields(ve.Error()), " ")
}
