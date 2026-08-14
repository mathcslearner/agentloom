package tools

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/mathcslearner/agentloom/internal/plugin"
)

// fakeTool is a minimal Tool with a settable manifest, for registration
// and validation coverage.
type fakeTool struct {
	manifest plugin.Manifest
	invoke   func(context.Context, Invocation) (json.RawMessage, error)
}

func (f fakeTool) Manifest() plugin.Manifest { return f.manifest }

func (f fakeTool) Invoke(ctx context.Context, inv Invocation) (json.RawMessage, error) {
	if f.invoke != nil {
		return f.invoke(ctx, inv)
	}
	return json.RawMessage(`null`), nil
}

// objectSchema is a minimal args schema requiring a string field "x".
const objectSchema = `{"type":"object","properties":{"x":{"type":"string"}},"required":["x"],"additionalProperties":false}`

func toolManifest(name, schema string) plugin.Manifest {
	return plugin.Manifest{
		Kind:         plugin.KindTool,
		Name:         name,
		Version:      "1.0.0",
		ConfigSchema: json.RawMessage(schema),
	}
}

// TestBuiltinToolManifests pins ADR-009's tool flag table and that every
// built-in carries a compilable args schema.
func TestBuiltinToolManifests(t *testing.T) {
	t.Parallel()

	reg, err := NewBuiltins(HTTPOptions{})
	if err != nil {
		t.Fatalf("NewBuiltins: %v", err)
	}
	want := map[string]plugin.Capabilities{
		"http_request":   {SideEffectful: true},
		"json_transform": {Cacheable: true},
	}
	manifests := reg.Manifests()
	if len(manifests) != len(want) {
		t.Fatalf("Manifests() = %d, want %d", len(manifests), len(want))
	}
	for _, m := range manifests {
		if m.Kind != plugin.KindTool {
			t.Errorf("%s: kind %q, want tool", m.Name, m.Kind)
		}
		if m.Version != "1.0.0" {
			t.Errorf("%s: version %q, want 1.0.0", m.Name, m.Version)
		}
		w, ok := want[m.Name]
		if !ok {
			t.Errorf("%s: not in the flag table", m.Name)
			continue
		}
		if m.Capabilities != w {
			t.Errorf("%s: capabilities %+v, want %+v", m.Name, m.Capabilities, w)
		}
		if m.ConfigSchema == nil || !json.Valid(m.ConfigSchema) {
			t.Errorf("%s: missing/invalid args schema: %s", m.Name, m.ConfigSchema)
		}
	}
}

// TestRegisterRejections covers ADR-009's registration discipline for
// tools: nil, wrong kind, missing schema, uncompilable schema, and
// duplicate name all fail boot with typed/identifiable errors.
func TestRegisterRejections(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		tool    Tool
		wantSub string
	}{
		{"wrong kind", fakeTool{manifest: plugin.Manifest{Kind: plugin.KindExecutor, Name: "x", Version: "1.0.0", ConfigSchema: json.RawMessage(objectSchema)}}, "must register as"},
		{"missing schema", fakeTool{manifest: plugin.Manifest{Kind: plugin.KindTool, Name: "x", Version: "1.0.0"}}, "no args schema"},
		{"uncompilable schema", fakeTool{manifest: toolManifest("x", `{"type":"nonsense-keyword-value"}`)}, "args schema"},
		{"invalid manifest", fakeTool{manifest: toolManifest("Bad Name", objectSchema)}, "registering tool"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := NewRegistry(tc.tool)
			if err == nil || !strings.Contains(err.Error(), tc.wantSub) {
				t.Errorf("NewRegistry err = %v, want substring %q", err, tc.wantSub)
			}
		})
	}

	// Nil tool.
	if _, err := NewRegistry(nil); err == nil {
		t.Error("NewRegistry(nil) succeeded, want error")
	}

	// Duplicate name.
	dup := fakeTool{manifest: toolManifest("dupe", objectSchema)}
	if _, err := NewRegistry(dup, dup); err == nil || !strings.Contains(err.Error(), "registered twice") {
		t.Errorf("duplicate registration err = %v, want 'registered twice'", err)
	}
}

// TestGetUnknownTool covers the typed lookup miss.
func TestGetUnknownTool(t *testing.T) {
	t.Parallel()

	reg, err := NewRegistry(fakeTool{manifest: toolManifest("known", objectSchema)})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	_, err = reg.Get("missing")
	if !errors.Is(err, ErrUnknownTool) {
		t.Errorf("Get(missing) err = %v, want ErrUnknownTool", err)
	}
	var ute *UnknownToolError
	if !errors.As(err, &ute) || ute.Name != "missing" {
		t.Errorf("Get(missing) err = %v, want *UnknownToolError{missing}", err)
	}
}

// TestValidateArgs covers the pre-invocation gate: valid args pass;
// missing required, wrong type, unknown field, and non-JSON all produce a
// typed *ArgsValidationError; an unknown tool produces *UnknownToolError.
func TestValidateArgs(t *testing.T) {
	t.Parallel()

	reg, err := NewRegistry(fakeTool{manifest: toolManifest("t", objectSchema)})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}

	if err := reg.ValidateArgs("t", []byte(`{"x":"ok"}`)); err != nil {
		t.Errorf("valid args rejected: %v", err)
	}

	bad := map[string]string{
		"missing required": `{}`,
		"wrong type":       `{"x":42}`,
		"unknown field":    `{"x":"ok","extra":1}`,
		"absent (null)":    ``,
		"not an object":    `"scalar"`,
		"malformed json":   `{`,
	}
	for name, args := range bad {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			err := reg.ValidateArgs("t", []byte(args))
			if !errors.Is(err, ErrInvalidArgs) {
				t.Errorf("ValidateArgs(%s) err = %v, want ErrInvalidArgs", name, err)
			}
			var ave *ArgsValidationError
			if !errors.As(err, &ave) || ave.Tool != "t" {
				t.Errorf("ValidateArgs(%s) err = %v, want *ArgsValidationError{t}", name, err)
			}
		})
	}

	if err := reg.ValidateArgs("missing", []byte(`{}`)); !errors.Is(err, ErrUnknownTool) {
		t.Errorf("ValidateArgs(missing) err = %v, want ErrUnknownTool", err)
	}
}
