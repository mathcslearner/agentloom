package config

// Environment variables read by LLMConfig.
const (
	EnvAnthropicAPIKey = "AGENTLOOM_ANTHROPIC_API_KEY" //nolint:gosec // G101 false positive: the variable's NAME, not a credential
)

// LLMConfig configures the model providers (ticket 8.3, internal/llm).
// Provider keys arrive via config/env only (CLAUDE.md invariant) and an
// empty key means "provider not configured" — that is not a load error,
// because a worker running no llm steps must boot keyless (and 8.4's
// routing requires either provider to be absent without breaking
// startup). Whoever constructs a provider enforces presence: the llm
// executor wiring (8.6) fails boot if an llm-executing worker has no
// usable provider.
type LLMConfig struct {
	// AnthropicAPIKey is the Anthropic API key; empty leaves the
	// Anthropic provider unconfigured.
	AnthropicAPIKey string
}

func defaultLLMConfig() LLMConfig {
	return LLMConfig{}
}

// applyEnv overrides fields from the environment. The key is passed
// through opaquely — a malformed key surfaces as a provider 401, where
// the error names the real problem — so today this never fails; the
// signature matches the other sub-configs.
//
//nolint:unparam // see above: uniform sub-config signature
func (c *LLMConfig) applyEnv(fn LookupFunc) []error {
	if raw, ok := lookup(fn, EnvAnthropicAPIKey); ok {
		c.AnthropicAPIKey = raw
	}
	return nil
}
