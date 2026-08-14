package config

// Environment variables read by LLMConfig.
const (
	EnvAnthropicAPIKey = "AGENTLOOM_ANTHROPIC_API_KEY" //nolint:gosec // G101 false positive: the variable's NAME, not a credential
	EnvOpenAIAPIKey    = "AGENTLOOM_OPENAI_API_KEY"    //nolint:gosec // G101 false positive: the variable's NAME, not a credential
)

// LLMConfig configures the model providers (tickets 8.3/8.4,
// internal/llm). Provider keys arrive via config/env only (CLAUDE.md
// invariant) and an empty key means "provider not configured" — that is
// not a load error, because a worker running no llm steps must boot
// keyless, and 8.4's routing requires each provider to be absent
// independently without breaking startup (llm.NewRegistryFromKeys
// constructs only the providers whose key is present). Whoever needs a
// specific provider enforces presence: the llm executor wiring (8.6)
// fails a step when its model routes to an unconfigured provider, and
// the 8.4 registry surfaces that as a typed *ProviderUnavailableError.
type LLMConfig struct {
	// AnthropicAPIKey is the Anthropic API key; empty leaves the
	// Anthropic provider unconfigured.
	AnthropicAPIKey string
	// OpenAIAPIKey is the OpenAI API key; empty leaves the OpenAI
	// provider unconfigured.
	OpenAIAPIKey string
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
	if raw, ok := lookup(fn, EnvOpenAIAPIKey); ok {
		c.OpenAIAPIKey = raw
	}
	return nil
}
