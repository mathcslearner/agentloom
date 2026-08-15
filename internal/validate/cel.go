package validate

// The cel validator (ticket 11.2, ADR-013): evaluates a CEL boolean
// predicate over the parsed output. The expression compiles once per
// distinct config through a compileCache, and pre-flight (CompileConfig)
// rejects a syntax/typecheck error, or a non-boolean predicate, as a
// permanent config error before any spend. A *runtime* evaluation failure
// (a missing field on this particular output) is by contrast a fail verdict,
// not a transport error — it is a deterministic content problem the semantic
// retry can fix, not a broken configuration.
//
// The environment is distinct from dag's edge-predicate environment (which
// exposes `output` and `run`): a validator judges a targeted value, so it
// binds `value` (the targeted sub-tree), `output` (the whole output), and
// `step_type` (the producing step's type).

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/google/cel-go/cel"

	"github.com/mathcslearner/agentloom/internal/plugin"
)

// celValidatorEnv builds the shared CEL environment once; cel.Env and
// cel.Program are safe for concurrent use, so compiled expressions are
// freely reusable across goroutines.
var celValidatorEnv = sync.OnceValues(func() (*cel.Env, error) {
	return cel.NewEnv(
		cel.Variable("value", cel.DynType),
		cel.Variable("output", cel.DynType),
		cel.Variable("step_type", cel.StringType),
	)
})

// celConfig is cel's config. Expr is a required boolean predicate; ParseJSON
// re-parses a string `value` as JSON before binding it (so a predicate can
// reach into an LLM's JSON-as-text answer).
type celConfig struct {
	// Expr is the CEL boolean predicate over value/output/step_type.
	Expr string `json:"expr"`
	// ParseJSON, when true, parses a string `value` target as JSON before
	// binding it, so `value.field` reaches into JSON emitted as text.
	ParseJSON bool `json:"parse_json,omitempty"`
}

// celProgram is the compiled artifact: the program plus the source text (for
// diagnostics).
type celProgram struct {
	prog cel.Program
	src  string
}

// CEL is the built-in cel validator: cacheable, holding a compileCache of
// compiled programs keyed by config bytes.
type CEL struct {
	cache *compileCache[celProgram]
}

// NewCEL builds the cel validator with its compile cache.
func NewCEL() *CEL {
	v := &CEL{}
	v.cache = newCompileCache(v.compile)
	return v
}

// Manifest implements Validator: cacheable only.
func (v *CEL) Manifest() plugin.Manifest {
	return plugin.Manifest{
		Kind:         plugin.KindValidator,
		Name:         celName,
		Version:      celVersion,
		Description:  "Evaluate a CEL boolean predicate over the parsed output (value/output/step_type).",
		Capabilities: deterministicCaps,
		ConfigSchema: builtinConfigSchema(&celConfig{}),
	}
}

// CompileConfig implements ConfigCompiler: the pre-flight gate. A missing
// expression, a syntax/typecheck error, or a predicate that cannot be
// boolean is a permanent config error; success warms the cache.
func (v *CEL) CompileConfig(config json.RawMessage) error {
	_, err := v.cache.get(config)
	return err
}

// compile is the pure builder behind the cache: decode the config, require
// an expression, compile it against the validator environment, and enforce
// that it is a boolean predicate (bool or dyn, deferring dyn to eval — dag's
// rule).
func (v *CEL) compile(config []byte) (celProgram, error) {
	var cfg celConfig
	if err := strictDecodeConfig(config, &cfg); err != nil {
		return celProgram{}, fmt.Errorf("decoding config: %v", err)
	}
	if cfg.Expr == "" {
		return celProgram{}, fmt.Errorf("%q is required", "expr")
	}
	env, err := celValidatorEnv()
	if err != nil {
		return celProgram{}, fmt.Errorf("building CEL environment: %v", err)
	}
	ast, iss := env.Compile(cfg.Expr)
	if iss.Err() != nil {
		return celProgram{}, fmt.Errorf("compiling expr: %v", compactLine(iss.Err().Error()))
	}
	if t := ast.OutputType(); !t.IsExactType(cel.BoolType) && !t.IsExactType(cel.DynType) {
		return celProgram{}, fmt.Errorf("expr must be a boolean predicate, but evaluates to %s", t.String())
	}
	prog, err := env.Program(ast)
	if err != nil {
		return celProgram{}, fmt.Errorf("building CEL program: %v", err)
	}
	return celProgram{prog: prog, src: cfg.Expr}, nil
}

// Validate evaluates the predicate over the target value. A string `value`
// is optionally re-parsed as JSON (parse_json) — a parse failure is an
// invalid_json fail verdict. A runtime evaluation error, or a predicate that
// yields false, is a fail verdict (cel_eval_error / predicate_false); only a
// broken *config* is a transport error, and that was caught pre-flight.
func (v *CEL) Validate(_ context.Context, in Input) (Verdict, error) {
	program, err := v.cache.get(in.Config)
	if err != nil {
		return Verdict{}, Permanentf(celName, err, "config no longer compiles")
	}
	var conf celConfig
	_ = strictDecodeConfig(in.Config, &conf)

	value, derr := decodeValue(in.Value)
	if derr != nil {
		return FailVerdict(Issue{
			Validator: celName, Code: "invalid_json", Path: "",
			Message: "target is not valid JSON, so the predicate cannot be evaluated",
		}), nil
	}
	if conf.ParseJSON {
		if s, ok := value.(string); ok {
			var inner any
			if jerr := json.Unmarshal([]byte(s), &inner); jerr != nil {
				return FailVerdict(Issue{
					Validator: celName, Code: "invalid_json", Path: "",
					Message: "target string is not valid JSON, so the predicate cannot be evaluated",
				}), nil
			}
			value = inner
		}
	}

	output, _ := decodeValue(in.Output)
	out, _, evalErr := program.prog.Eval(map[string]any{
		"value":     value,
		"output":    output,
		"step_type": string(in.StepType),
	})
	if evalErr != nil {
		return FailVerdict(Issue{
			Validator: celName, Code: "cel_eval_error", Path: "",
			Message: "predicate could not be evaluated against this output",
		}), nil
	}
	b, ok := out.Value().(bool)
	if !ok {
		// A dyn-typed predicate produced a non-boolean at runtime — treat as
		// a content failure (the expression did not decide true/false here).
		return FailVerdict(Issue{
			Validator: celName, Code: "cel_eval_error", Path: "",
			Message: "predicate did not evaluate to a boolean",
		}), nil
	}
	if !b {
		return FailVerdict(Issue{
			Validator: celName, Code: "predicate_false", Path: "",
			Message: "predicate evaluated to false",
		}), nil
	}
	return PassVerdict(), nil
}
