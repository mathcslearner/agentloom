package dag

import (
	"encoding/json"
	"fmt"
	"regexp"
)

// pluginNameRe is the plugin-name shape (ADR-009): validator names must
// match it, the same spelling as step types and every other plugin name.
var pluginNameRe = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

// checkJSONPointer reports whether s is a syntactically valid RFC 6901 JSON
// pointer (the shape a validator's `target` must have). A non-empty pointer
// must start with '/', and every '~' escape must be '~0' or '~1'. It checks
// syntax only — whether the pointer resolves in a given output is a runtime
// content question (a fail verdict), not a definition error.
func checkJSONPointer(s string) error {
	if s == "" {
		return nil
	}
	if s[0] != '/' {
		return fmt.Errorf("must start with '/'")
	}
	for i := 0; i < len(s); i++ {
		if s[i] != '~' {
			continue
		}
		if i+1 >= len(s) || (s[i+1] != '0' && s[i+1] != '1') {
			return fmt.Errorf("'~' must be followed by '0' or '1'")
		}
		i++
	}
	return nil
}

// This file is the definition-contract half of ADR-013: the optional
// per-step `validation` chain an author may carry on the step envelope, and
// the bound Validate enforces on it. The runtime half — the validator SPI,
// the verdict model, the chain runner, the engine's validate stage — lives
// in internal/validate and the engine (M11.1+); this package only defines
// and validates the authored field.

// MaxValidators caps the number of validators in a step's validation chain
// (ADR-013). A bound like every other envelope cap (MaxCacheTTL,
// MaxRetryAttempts): the worst accepted chain is finite, so a pathological
// definition cannot make a single step's completion run an unbounded number
// of checks.
const MaxValidators = 16

// ValidatorSpec is one entry in a step's validation chain (ADR-013): the
// name of a registered validator (kind validator), its opaque config
// object, and an optional RFC 6901 JSON pointer selecting the sub-tree of
// the step output to validate. The config's *content* is validated against
// the named validator's schema at claim, pre-flight — not at submit time,
// since the validator's compiled schema is a runtime artifact (the
// tool-args precedent, ADR-009).
type ValidatorSpec struct {
	// Name is the registered validator's name (^[a-z][a-z0-9_]*$). Required.
	// The validator's existence is resolved at claim (a definition may name
	// validators a given worker build has not registered); submit-time
	// validation only checks the name's shape.
	Name string `json:"name"`

	// Config is the validator's config object, passed opaque to the
	// validator (which decodes and schema-checks it). Empty when the entry
	// had no `config` key — a validator that needs none.
	Config json.RawMessage `json:"config,omitempty"`

	// Target is an optional RFC 6901 JSON pointer into the step output; the
	// validator judges the selected sub-tree. Empty means the whole output,
	// except for llm-family steps whose default target is `/text` (applied
	// at runtime, not stored). A target that does not resolve in a given
	// output is a fail verdict, never an error (ADR-013).
	Target string `json:"target,omitempty"`
}

// ValidationPolicy is a step's authored output-validation chain (ADR-013);
// nil on the step envelope means the `validation` key was absent (no
// validation — the output is accepted as produced). Uniform across step
// types, so it lives on the step envelope, not in the per-type config —
// like retry, timeout, cache, and budget. At least one validator must be
// present when the block is present (Validate rejects an empty chain).
//
// 11.4 will add `max_attempts` (the semantic-retry budget) and `feedback`
// (the critique-prompt template) to this struct; strict decode rejects
// those keys until then, exactly as the ADR-006 reservation discipline
// rejects an unripe field.
type ValidationPolicy struct {
	// Validators is the ordered chain; each entry names a validator and its
	// config. Required and non-empty when a `validation` block is present.
	Validators []ValidatorSpec `json:"validators"`
}
