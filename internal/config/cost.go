package config

import "fmt"

// Environment variables read by CostConfig.
const (
	EnvPricing             = "AGENTLOOM_PRICING"
	EnvPricingFile         = "AGENTLOOM_PRICING_FILE"
	EnvCostUnknownModelPol = "AGENTLOOM_COST_UNKNOWN_MODEL_POLICY"
)

// Unknown-model policy values (ADR-012). Kept as local string constants so
// config stays a leaf — the typed policy enum and its semantics live in
// internal/cost, which cmd/worker maps this string onto (the internal/limits
// precedent: config validates, the domain package owns behavior).
const (
	// UnknownModelPolicyEstimate prices an unknown model at the catalog
	// fallback rate and emits a warning event. The default.
	UnknownModelPolicyEstimate = "estimate"
	// UnknownModelPolicyFail refuses to price an unknown model (fail-closed
	// pre-flight in 10.3).
	UnknownModelPolicyFail = "fail"
	// DefaultUnknownModelPolicy is the fleet default.
	DefaultUnknownModelPolicy = UnknownModelPolicyEstimate
)

// CostConfig points the worker at the M10 pricing catalog (ticket 10.1,
// ADR-012): the versioned $/1M-token catalog the cost ledger prices attempts
// against. The catalog itself is parsed and validated by internal/cost — a
// config leaf must not import it, so this struct only carries the override
// source (and enforces the at-most-one rule) plus the unknown-model policy
// string. Both source fields empty means the embedded default catalog is used
// unchanged. cmd/api never prices (it reads ledger rows from Postgres in
// 10.2), so only cmd/worker consumes this.
type CostConfig struct {
	// Inline is a pricing-catalog override as an inline JSON document
	// (AGENTLOOM_PRICING). Mutually exclusive with File. cmd/worker merges it
	// onto the embedded defaults via cost.Load.
	Inline string
	// File is a path to a pricing-catalog override JSON file
	// (AGENTLOOM_PRICING_FILE). Mutually exclusive with Inline. cmd/worker
	// reads it via cost.Load; the config package never touches the filesystem.
	File string
	// UnknownModelPolicy selects pre-flight behavior for a model with no
	// catalog entry: UnknownModelPolicyEstimate (fallback rate + warning) or
	// UnknownModelPolicyFail (fail-closed). Validated at load.
	UnknownModelPolicy string
}

func defaultCostConfig() CostConfig {
	return CostConfig{
		UnknownModelPolicy: DefaultUnknownModelPolicy,
	}
}

// applyEnv overrides fields from the environment, rejecting both sources set
// at once (cost.Load also rejects it, but failing at config load surfaces the
// mistake before any backend is opened) and validating the policy value.
func (c *CostConfig) applyEnv(fn LookupFunc) []error {
	var errs []error
	applyString(fn, EnvPricing, &c.Inline)
	applyString(fn, EnvPricingFile, &c.File)
	applyString(fn, EnvCostUnknownModelPol, &c.UnknownModelPolicy)
	if c.Inline != "" && c.File != "" {
		errs = append(errs, fmt.Errorf("%s and %s are mutually exclusive — set only one", EnvPricing, EnvPricingFile))
	}
	switch c.UnknownModelPolicy {
	case UnknownModelPolicyEstimate, UnknownModelPolicyFail:
	default:
		errs = append(errs, fmt.Errorf("%s: invalid value %q (want %q or %q)",
			EnvCostUnknownModelPol, c.UnknownModelPolicy, UnknownModelPolicyEstimate, UnknownModelPolicyFail))
	}
	return errs
}
