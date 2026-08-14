package tools

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/itchyny/gojq"

	"github.com/mathcslearner/agentloom/internal/plugin"
)

// jsonTransformName is the built-in's registered name and step-config
// tool value.
const jsonTransformName = "json_transform"

// jsonTransformVersion is the tool's plugin version (ADR-009). It ships
// at 1.0.0 — the real semantics that replace the 8.7-era `tool` dev stub.
const jsonTransformVersion = "1.0.0"

// jsonTransformArgs is json_transform's argument struct; its JSON Schema
// (generated from these tags) is the tool's validation contract. Expr is
// required; Input is arbitrary JSON (json.RawMessage reflects to the
// permissive `true` schema, so any JSON value is accepted).
type jsonTransformArgs struct {
	// Expr is the gojq (jq-syntax) program run against Input.
	Expr string `json:"expr"`
	// Input is the JSON value the program transforms. Absent means the
	// program runs against JSON null.
	Input json.RawMessage `json:"input,omitempty"`
}

// JSONTransform is the built-in pure tool: it evaluates a gojq (jq) program
// over a JSON input and returns the result. It touches no external state,
// so it is cacheable and neither side-effectful nor cost-bearing — the
// first pure built-in tool. Compile and runtime errors are deterministic
// functions of (expr, input), so both are permanent.
type JSONTransform struct{}

// NewJSONTransform builds the json_transform tool.
func NewJSONTransform() JSONTransform { return JSONTransform{} }

// Manifest implements Tool (ADR-009): cacheable-only.
func (JSONTransform) Manifest() plugin.Manifest {
	schema, err := argsSchema(&jsonTransformArgs{})
	if err != nil {
		// Unreachable: a fixed, reflectable struct.
		panic(err)
	}
	return plugin.Manifest{
		Kind:         plugin.KindTool,
		Name:         jsonTransformName,
		Version:      jsonTransformVersion,
		Description:  "Transform a JSON value with a gojq (jq-syntax) program.",
		Capabilities: plugin.Capabilities{Cacheable: true},
		ConfigSchema: schema,
	}
}

// Invoke evaluates the program. The result contract: exactly one emitted
// value returns that value; zero or multiple emitted values return a JSON
// array of everything the program yielded (so a filter that fans out, or
// one that selects nothing, both have a well-defined output). Evaluation
// runs under ctx so a pathological program is bounded by the step timeout.
func (JSONTransform) Invoke(ctx context.Context, inv Invocation) (json.RawMessage, error) {
	var args jsonTransformArgs
	if err := strictUnmarshal(inv.Args, &args); err != nil {
		// Args passed schema validation; a decode failure here means a
		// shape the schema permits but the struct rejects — permanent.
		return nil, permanentf(jsonTransformName, "decoding args: %v", err)
	}
	if args.Expr == "" {
		return nil, permanentf(jsonTransformName, "%q is required", "expr")
	}

	query, err := gojq.Parse(args.Expr)
	if err != nil {
		return nil, permanentf(jsonTransformName, "parsing expr: %v", err)
	}
	code, err := gojq.Compile(query)
	if err != nil {
		return nil, permanentf(jsonTransformName, "compiling expr: %v", err)
	}

	input, err := decodeJSONValue(args.Input)
	if err != nil {
		return nil, permanentf(jsonTransformName, "decoding input: %v", err)
	}

	var results []any
	iter := code.RunWithContext(ctx, input)
	for {
		v, ok := iter.Next()
		if !ok {
			break
		}
		if err, isErr := v.(error); isErr {
			// gojq surfaces both runtime jq errors and ctx cancellation as
			// yielded error values. A ctx error passes through unwrapped so
			// the engine judges timeout vs. cancelled; a jq runtime error is
			// deterministic → permanent.
			var haltErr *gojq.HaltError
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return nil, err
			}
			if errors.As(err, &haltErr) {
				return nil, permanentf(jsonTransformName, "program halted: %v", err)
			}
			return nil, permanentf(jsonTransformName, "evaluating expr: %v", err)
		}
		results = append(results, v)
	}

	var out any
	switch len(results) {
	case 1:
		out = results[0]
	default:
		// Zero or many: a JSON array (non-nil so an empty result is [] not
		// null).
		if results == nil {
			results = []any{}
		}
		out = results
	}
	data, err := json.Marshal(out)
	if err != nil {
		return nil, permanentf(jsonTransformName, "marshaling result: %v", err)
	}
	return data, nil
}
