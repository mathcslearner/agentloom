package validate

// The regex validator (ticket 11.2, ADR-013): asserts the target value
// matches (or, negated, does not match) an RE2 pattern. The pattern compiles
// once per distinct config through a compileCache and pre-flight
// (CompileConfig) rejects an unparseable pattern as a permanent config error
// before any spend.

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"

	"github.com/mathcslearner/agentloom/internal/plugin"
)

// regexConfig is regex's config. Pattern is a required RE2 expression;
// Negate flips the sense (fail when the pattern DOES match).
type regexConfig struct {
	// Pattern is the RE2 expression tested against the target value.
	Pattern string `json:"pattern"`
	// Negate, when true, fails the output when the pattern matches (an
	// output must NOT contain a forbidden shape).
	Negate bool `json:"negate,omitempty"`
}

// Regex is the built-in regex validator: cacheable, holding a compileCache
// of compiled RE2 patterns keyed by config bytes.
type Regex struct {
	cache *compileCache[*regexp.Regexp]
}

// NewRegex builds the regex validator with its compile cache.
func NewRegex() *Regex {
	v := &Regex{}
	v.cache = newCompileCache(v.compile)
	return v
}

// Manifest implements Validator: cacheable only.
func (v *Regex) Manifest() plugin.Manifest {
	return plugin.Manifest{
		Kind:         plugin.KindValidator,
		Name:         regexName,
		Version:      regexVersion,
		Description:  "Assert the target value matches (or, negated, does not match) an RE2 pattern.",
		Capabilities: deterministicCaps,
		ConfigSchema: builtinConfigSchema(&regexConfig{}),
	}
}

// CompileConfig implements ConfigCompiler: the pre-flight gate. A missing or
// unparseable pattern is a permanent config error; success warms the cache.
func (v *Regex) CompileConfig(config json.RawMessage) error {
	_, err := v.cache.get(config)
	return err
}

// compile is the pure builder behind the cache.
func (v *Regex) compile(config []byte) (*regexp.Regexp, error) {
	var cfg regexConfig
	if err := strictDecodeConfig(config, &cfg); err != nil {
		return nil, fmt.Errorf("decoding config: %v", err)
	}
	if cfg.Pattern == "" {
		return nil, fmt.Errorf("%q is required", "pattern")
	}
	re, err := regexp.Compile(cfg.Pattern)
	if err != nil {
		return nil, fmt.Errorf("compiling pattern: %v", err)
	}
	return re, nil
}

// Validate tests the compiled pattern against the target value's string form
// (a JSON string target is its contents; any other value is its compact JSON
// encoding). A non-match (or, negated, a match) is a fail verdict.
func (v *Regex) Validate(_ context.Context, in Input) (Verdict, error) {
	re, err := v.cache.get(in.Config)
	if err != nil {
		return Verdict{}, Permanentf(regexName, err, "config no longer compiles")
	}
	// Negate is a scalar flag off the config, not the reused artifact; the
	// same bytes already decoded in compile, so this cannot fail.
	var conf regexConfig
	_ = strictDecodeConfig(in.Config, &conf)

	matched := re.MatchString(stringOf(in.Value))
	if matched == conf.Negate {
		if conf.Negate {
			return FailVerdict(Issue{
				Validator: regexName, Code: "pattern_matched", Path: "",
				Message: "value matched a forbidden pattern",
			}), nil
		}
		return FailVerdict(Issue{
			Validator: regexName, Code: "pattern_no_match", Path: "",
			Message: "value did not match the required pattern",
		}), nil
	}
	return PassVerdict(), nil
}
