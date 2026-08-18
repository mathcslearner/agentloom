package config

import "fmt"

// Environment variables read by LLMConfig.
const (
	EnvAnthropicAPIKey   = "AGENTLOOM_ANTHROPIC_API_KEY" //nolint:gosec // G101 false positive: the variable's NAME, not a credential
	EnvOpenAIAPIKey      = "AGENTLOOM_OPENAI_API_KEY"    //nolint:gosec // G101 false positive: the variable's NAME, not a credential
	EnvLLMMockEnabled    = "AGENTLOOM_LLM_MOCK_ENABLED"
	EnvLLMMockScript     = "AGENTLOOM_LLM_MOCK_SCRIPT"
	EnvLLMMockScriptFile = "AGENTLOOM_LLM_MOCK_SCRIPT_FILE"
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
	// MockEnabled registers the deterministic offline mock provider
	// (ticket 8.5) under name "mock", addressed as model "mock/...". It
	// needs no key — it makes no external call — so it is a plain toggle,
	// off by default in the binaries. Compose defaults it on so the M8
	// exit-criterion workflow runs on the mock in CI. A deployment enables
	// it to run llm/planner/agent steps offline against scripted or echo
	// responses.
	MockEnabled bool
	// MockScript is an inline mock-provider script (AGENTLOOM_LLM_MOCK_SCRIPT):
	// scripted matching rules and response sequences that replace the mock's
	// config-free echo, so a compose/worker run drives real behavior offline
	// (e.g. the flagship example's writer⇄critic loop, ticket 14.5). Parsed and
	// validated by llm.ParseMockScript + llm.NewMock — the config package never
	// touches it beyond carrying the string. Mutually exclusive with
	// MockScriptFile; only meaningful when MockEnabled is true. Empty leaves the
	// mock in its echo-only default.
	MockScript string
	// MockScriptFile is a path to a mock-provider script JSON file
	// (AGENTLOOM_LLM_MOCK_SCRIPT_FILE). Read by cmd/worker via os.ReadFile; the
	// config package never touches the filesystem. Mutually exclusive with
	// MockScript.
	MockScriptFile string
}

func defaultLLMConfig() LLMConfig {
	return LLMConfig{}
}

// applyEnv overrides fields from the environment. Keys are passed through
// opaquely — a malformed key surfaces as a provider 401, where the error
// names the real problem — so the only failure this can report is a
// non-boolean mock toggle.
func (c *LLMConfig) applyEnv(fn LookupFunc) []error {
	if raw, ok := lookup(fn, EnvAnthropicAPIKey); ok {
		c.AnthropicAPIKey = raw
	}
	if raw, ok := lookup(fn, EnvOpenAIAPIKey); ok {
		c.OpenAIAPIKey = raw
	}
	applyString(fn, EnvLLMMockScript, &c.MockScript)
	applyString(fn, EnvLLMMockScriptFile, &c.MockScriptFile)
	errs := applyBool(nil, fn, EnvLLMMockEnabled, &c.MockEnabled)
	if c.MockScript != "" && c.MockScriptFile != "" {
		errs = append(errs, fmt.Errorf("%s and %s are mutually exclusive — set only one", EnvLLMMockScript, EnvLLMMockScriptFile))
	}
	return errs
}
