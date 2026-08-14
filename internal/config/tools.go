package config

import (
	"strings"
	"time"
)

// Environment variables read by ToolsConfig.
const (
	EnvToolsHTTPAllowlist        = "AGENTLOOM_TOOLS_HTTP_ALLOWLIST"
	EnvToolsHTTPTimeout          = "AGENTLOOM_TOOLS_HTTP_TIMEOUT"
	EnvToolsHTTPMaxResponseBytes = "AGENTLOOM_TOOLS_HTTP_MAX_RESPONSE_BYTES"
)

// Default tool settings.
const (
	defaultToolsHTTPTimeout          = 30 * time.Second
	defaultToolsHTTPMaxResponseBytes = int64(1 << 20) // 1 MiB
)

// ToolsConfig configures the built-in tools (ticket 8.7, internal/tools).
// The http_request tool's allowlist is the SSRF guard: an empty allowlist
// denies every host, so http_request is inert until a deployment names the
// hosts it may reach. json_transform is config-free.
type ToolsConfig struct {
	// HTTPAllowlist is the set of hosts http_request may reach. Each entry
	// is a hostname, optionally with a port ("example.com" or
	// "example.com:8443"); a bare hostname permits any port. Empty means
	// deny all — the safe default.
	HTTPAllowlist []string
	// HTTPTimeout bounds an http_request call whose args omit `timeout`.
	HTTPTimeout time.Duration
	// HTTPMaxResponseBytes caps the response body http_request reads; a
	// larger body is a permanent failure.
	HTTPMaxResponseBytes int64
}

func defaultToolsConfig() ToolsConfig {
	return ToolsConfig{
		HTTPAllowlist:        nil,
		HTTPTimeout:          defaultToolsHTTPTimeout,
		HTTPMaxResponseBytes: defaultToolsHTTPMaxResponseBytes,
	}
}

// applyEnv overrides fields from the environment. The allowlist is a
// comma-separated host list (blanks trimmed and dropped); an empty or
// unset value leaves the default deny-all in place.
func (c *ToolsConfig) applyEnv(fn LookupFunc) []error {
	if raw, ok := lookup(fn, EnvToolsHTTPAllowlist); ok {
		c.HTTPAllowlist = parseHostList(raw)
	}
	var errs []error
	errs = applyPositiveDuration(errs, fn, EnvToolsHTTPTimeout, &c.HTTPTimeout)
	errs = applyPositiveInt64(errs, fn, EnvToolsHTTPMaxResponseBytes, &c.HTTPMaxResponseBytes)
	return errs
}

// parseHostList splits a comma-separated allowlist, trimming whitespace
// and dropping empty entries so a trailing comma or "a, b" spacing is
// tolerated.
func parseHostList(raw string) []string {
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
