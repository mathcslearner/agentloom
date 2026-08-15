package config

import "fmt"

// Environment variables read by ResourcesConfig.
const (
	EnvResources     = "AGENTLOOM_RESOURCES"
	EnvResourcesFile = "AGENTLOOM_RESOURCES_FILE"
)

// ResourcesConfig points the worker at the ADR-010 resource-limit
// configuration (ticket 9.1): the named external resources whose request and
// token throughput the M9 limiter middleware governs fleet-wide. The limits
// themselves are parsed and validated by internal/limits — a config leaf must
// not import it, so this struct only carries the source and enforces that at
// most one is given. Both empty means no configured limits: every resource is
// unlimited (ADR-010's unknown-resource policy).
type ResourcesConfig struct {
	// Inline is the resource-limit config as an inline JSON document
	// (AGENTLOOM_RESOURCES). Mutually exclusive with File.
	Inline string
	// File is a path to a resource-limit config JSON file
	// (AGENTLOOM_RESOURCES_FILE). Mutually exclusive with Inline. cmd/worker
	// reads it via limits.Load; the config package never touches the
	// filesystem.
	File string
}

func defaultResourcesConfig() ResourcesConfig { return ResourcesConfig{} }

// applyEnv overrides fields from the environment, reporting an error when
// both sources are set (limits.Load also rejects it, but failing at config
// load surfaces the mistake before any backend is opened).
func (c *ResourcesConfig) applyEnv(fn LookupFunc) []error {
	applyString(fn, EnvResources, &c.Inline)
	applyString(fn, EnvResourcesFile, &c.File)
	if c.Inline != "" && c.File != "" {
		return []error{fmt.Errorf("%s and %s are mutually exclusive — set only one", EnvResources, EnvResourcesFile)}
	}
	return nil
}
