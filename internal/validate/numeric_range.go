package validate

// The numeric_range validator (ticket 11.2, ADR-013): asserts the target
// value is a number within configured bounds. It compiles no artifact, but
// its config has a cross-field constraint the JSON Schema cannot express (at
// least one bound; a min no greater than a max), so it implements
// ConfigCompiler to reject a bound-less or inverted config as a permanent
// error pre-flight.

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/mathcslearner/agentloom/internal/plugin"
)

// numericRangeConfig is numeric_range's config: any subset of the four
// bounds, at least one required (enforced by CompileConfig). Inclusive
// (min/max) and exclusive (exclusive_min/exclusive_max) bounds may combine.
type numericRangeConfig struct {
	// Min is the inclusive lower bound (value >= min).
	Min *float64 `json:"min,omitempty"`
	// Max is the inclusive upper bound (value <= max).
	Max *float64 `json:"max,omitempty"`
	// ExclusiveMin is the exclusive lower bound (value > exclusive_min).
	ExclusiveMin *float64 `json:"exclusive_min,omitempty"`
	// ExclusiveMax is the exclusive upper bound (value < exclusive_max).
	ExclusiveMax *float64 `json:"exclusive_max,omitempty"`
}

// NumericRange is the built-in numeric_range validator: cacheable, no
// compiled artifact.
type NumericRange struct{}

// NewNumericRange builds the numeric_range validator.
func NewNumericRange() NumericRange { return NumericRange{} }

// Manifest implements Validator: cacheable only.
func (NumericRange) Manifest() plugin.Manifest {
	return plugin.Manifest{
		Kind:         plugin.KindValidator,
		Name:         numericRangeName,
		Version:      numericRangeVersion,
		Description:  "Assert the target value is a number within configured bounds.",
		Capabilities: deterministicCaps,
		ConfigSchema: builtinConfigSchema(&numericRangeConfig{}),
	}
}

// CompileConfig implements ConfigCompiler: the pre-flight gate for the
// cross-field constraints the config schema cannot express — at least one
// bound must be set, and a lower bound may not exceed an upper bound (a
// range that can never be satisfied is an authoring mistake, caught before
// spend).
func (NumericRange) CompileConfig(config json.RawMessage) error {
	var cfg numericRangeConfig
	if err := strictDecodeConfig(config, &cfg); err != nil {
		return fmt.Errorf("decoding config: %v", err)
	}
	if cfg.Min == nil && cfg.Max == nil && cfg.ExclusiveMin == nil && cfg.ExclusiveMax == nil {
		return fmt.Errorf("at least one bound (min, max, exclusive_min, exclusive_max) is required")
	}
	lower, hasLower := effectiveLower(cfg)
	upper, hasUpper, upperExclusive := effectiveUpper(cfg)
	if hasLower && hasUpper {
		// An inclusive lower equal to an exclusive upper (or vice versa) is
		// also unsatisfiable, so a strict combination requires lower < upper.
		if lower > upper || (lower == upper && upperExclusive) {
			return fmt.Errorf("lower bound exceeds upper bound — no value can satisfy the range")
		}
	}
	return nil
}

// Validate parses the target value as a number and checks it against the
// bounds. A value that is neither a JSON number nor a numeric string is a
// not_a_number fail verdict; a number outside a bound is a below_min /
// above_max fail verdict.
func (NumericRange) Validate(_ context.Context, in Input) (Verdict, error) {
	var cfg numericRangeConfig
	if err := strictDecodeConfig(in.Config, &cfg); err != nil {
		return Verdict{}, Permanentf(numericRangeName, err, "decoding config")
	}

	n, ok := numberOf(in.Value)
	if !ok {
		return FailVerdict(Issue{
			Validator: numericRangeName, Code: "not_a_number", Path: "",
			Message: "value is not a number",
		}), nil
	}

	if cfg.Min != nil && n < *cfg.Min {
		return FailVerdict(belowMin()), nil
	}
	if cfg.ExclusiveMin != nil && n <= *cfg.ExclusiveMin {
		return FailVerdict(belowMin()), nil
	}
	if cfg.Max != nil && n > *cfg.Max {
		return FailVerdict(aboveMax()), nil
	}
	if cfg.ExclusiveMax != nil && n >= *cfg.ExclusiveMax {
		return FailVerdict(aboveMax()), nil
	}
	return PassVerdict(), nil
}

func belowMin() Issue {
	return Issue{Validator: numericRangeName, Code: "below_min", Message: "value is below the allowed range"}
}

func aboveMax() Issue {
	return Issue{Validator: numericRangeName, Code: "above_max", Message: "value is above the allowed range"}
}

// numberOf extracts a finite float64 from a target value: a JSON number
// directly, or a JSON string whose trimmed contents parse as a finite float
// (an LLM answer like "42" or " 3.14 "). NaN and ±Inf never satisfy a range,
// so they are reported not-a-number. Reports ok=false for any other value.
func numberOf(value json.RawMessage) (float64, bool) {
	decoded, err := decodeValue(value)
	if err != nil {
		return 0, false
	}
	switch v := decoded.(type) {
	case float64:
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return 0, false
		}
		return v, true
	case string:
		f, perr := strconv.ParseFloat(strings.TrimSpace(v), 64)
		if perr != nil || math.IsNaN(f) || math.IsInf(f, 0) {
			return 0, false
		}
		return f, true
	default:
		return 0, false
	}
}

// effectiveLower returns the tightest configured lower bound and whether one
// exists, for the pre-flight satisfiability check.
func effectiveLower(cfg numericRangeConfig) (float64, bool) {
	switch {
	case cfg.Min != nil && cfg.ExclusiveMin != nil:
		return math.Max(*cfg.Min, *cfg.ExclusiveMin), true
	case cfg.Min != nil:
		return *cfg.Min, true
	case cfg.ExclusiveMin != nil:
		return *cfg.ExclusiveMin, true
	default:
		return 0, false
	}
}

// effectiveUpper returns the tightest configured upper bound, whether one
// exists, and whether that tightest bound is exclusive.
func effectiveUpper(cfg numericRangeConfig) (bound float64, has bool, exclusive bool) {
	switch {
	case cfg.Max != nil && cfg.ExclusiveMax != nil:
		if *cfg.ExclusiveMax <= *cfg.Max {
			return *cfg.ExclusiveMax, true, true
		}
		return *cfg.Max, true, false
	case cfg.Max != nil:
		return *cfg.Max, true, false
	case cfg.ExclusiveMax != nil:
		return *cfg.ExclusiveMax, true, true
	default:
		return 0, false, false
	}
}
