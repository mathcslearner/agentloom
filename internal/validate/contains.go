package validate

// The contains validator (ticket 11.2, ADR-013): asserts the target value's
// string form contains (or, negated, does not contain) a required substring.
// It compiles nothing — the check is a plain substring scan — so it takes no
// compileCache; its config is fully vetted by the config JSON Schema
// (substring non-empty), so it needs no ConfigCompiler either.

import (
	"context"
	"strings"

	"github.com/mathcslearner/agentloom/internal/plugin"
)

// containsConfig is contains's config. Substring is required (min length 1,
// enforced by the config schema). CaseInsensitive folds case before the
// scan; Negate flips the sense.
type containsConfig struct {
	// Substring is the text the output must contain.
	Substring string `json:"substring" jsonschema:"minLength=1"`
	// CaseInsensitive folds case on both sides before comparing.
	CaseInsensitive bool `json:"case_insensitive,omitempty"`
	// Negate, when true, fails the output when the substring is present.
	Negate bool `json:"negate,omitempty"`
}

// Contains is the built-in contains validator: cacheable, no compiled
// artifact.
type Contains struct{}

// NewContains builds the contains validator.
func NewContains() Contains { return Contains{} }

// Manifest implements Validator: cacheable only.
func (Contains) Manifest() plugin.Manifest {
	return plugin.Manifest{
		Kind:         plugin.KindValidator,
		Name:         containsName,
		Version:      containsVersion,
		Description:  "Assert the target value contains (or, negated, does not contain) a substring.",
		Capabilities: deterministicCaps,
		ConfigSchema: builtinConfigSchema(&containsConfig{}),
	}
}

// Validate scans the target value's string form for the substring. A missing
// substring (or, negated, a present one) is a fail verdict.
func (Contains) Validate(_ context.Context, in Input) (Verdict, error) {
	var conf containsConfig
	if err := strictDecodeConfig(in.Config, &conf); err != nil {
		// Schema-validated already; a decode failure here is a config the
		// schema admitted but the struct rejects — permanent.
		return Verdict{}, Permanentf(containsName, err, "decoding config")
	}
	if conf.Substring == "" {
		return Verdict{}, Permanentf(containsName, nil, "%q is required", "substring")
	}

	hay := stringOf(in.Value)
	needle := conf.Substring
	if conf.CaseInsensitive {
		hay = strings.ToLower(hay)
		needle = strings.ToLower(needle)
	}
	present := strings.Contains(hay, needle)
	if present == conf.Negate {
		if conf.Negate {
			return FailVerdict(Issue{
				Validator: containsName, Code: "substring_present", Path: "",
				Message: "value contained a forbidden substring",
			}), nil
		}
		return FailVerdict(Issue{
			Validator: containsName, Code: "substring_missing", Path: "",
			Message: "value did not contain the required substring",
		}), nil
	}
	return PassVerdict(), nil
}
