package config

import (
	"fmt"
	"strconv"
	"time"
)

// Environment variables read by ResourcesConfig.
const (
	EnvResources          = "AGENTLOOM_RESOURCES"
	EnvResourcesFile      = "AGENTLOOM_RESOURCES_FILE"
	EnvResourcesKeyPrefix = "AGENTLOOM_RESOURCES_KEY_PREFIX"
	EnvResourcesThrFloor  = "AGENTLOOM_RESOURCES_THROTTLE_FLOOR"
	EnvResourcesThrCap    = "AGENTLOOM_RESOURCES_THROTTLE_CAP"
	EnvResourcesThrJitter = "AGENTLOOM_RESOURCES_THROTTLE_JITTER_FRAC"
)

// Throttle requeue-math defaults (ADR-010): the delay before a rate-limited
// step's re-dispatch is clamp(retry_after, floor, cap) plus additive jitter.
const (
	DefaultResourcesThrottleFloor      = 500 * time.Millisecond
	DefaultResourcesThrottleCap        = 5 * time.Minute
	DefaultResourcesThrottleJitterFrac = 0.20
	// DefaultResourcesKeyPrefix is the Redis key namespace for resource
	// buckets; a test-isolation knob, mirroring the API limiter's prefix.
	DefaultResourcesKeyPrefix = "ratelimit:resource"
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
	// KeyPrefix is the Redis key namespace for resource buckets
	// (AGENTLOOM_RESOURCES_KEY_PREFIX). A test-isolation knob; defaults to
	// DefaultResourcesKeyPrefix.
	KeyPrefix string
	// ThrottleFloor / ThrottleCap bound the re-dispatch delay of a throttled
	// step (ADR-010's requeue math). ThrottleFloor guards against a hot
	// requeue loop; ThrottleCap bounds the wait. Both must be positive with
	// floor ≤ cap.
	ThrottleFloor time.Duration
	ThrottleCap   time.Duration
	// ThrottleJitterFrac is the additive-jitter fraction on top of the
	// clamped delay. Must be in [0, 1); 0 disables jitter.
	ThrottleJitterFrac float64
}

func defaultResourcesConfig() ResourcesConfig {
	return ResourcesConfig{
		KeyPrefix:          DefaultResourcesKeyPrefix,
		ThrottleFloor:      DefaultResourcesThrottleFloor,
		ThrottleCap:        DefaultResourcesThrottleCap,
		ThrottleJitterFrac: DefaultResourcesThrottleJitterFrac,
	}
}

// applyEnv overrides fields from the environment, reporting an error when
// both sources are set (limits.Load also rejects it, but failing at config
// load surfaces the mistake before any backend is opened) and validating the
// throttle knobs.
func (c *ResourcesConfig) applyEnv(fn LookupFunc) []error {
	var errs []error
	applyString(fn, EnvResources, &c.Inline)
	applyString(fn, EnvResourcesFile, &c.File)
	applyString(fn, EnvResourcesKeyPrefix, &c.KeyPrefix)
	errs = applyPositiveDuration(errs, fn, EnvResourcesThrFloor, &c.ThrottleFloor)
	errs = applyPositiveDuration(errs, fn, EnvResourcesThrCap, &c.ThrottleCap)
	if raw, ok := lookup(fn, EnvResourcesThrJitter); ok {
		f, err := strconv.ParseFloat(raw, 64)
		if err != nil || f < 0 || f >= 1 {
			errs = append(errs, fmt.Errorf("%s: invalid value %q (want a fraction in [0, 1))", EnvResourcesThrJitter, raw))
		} else {
			c.ThrottleJitterFrac = f
		}
	}
	if c.Inline != "" && c.File != "" {
		errs = append(errs, fmt.Errorf("%s and %s are mutually exclusive — set only one", EnvResources, EnvResourcesFile))
	}
	if c.ThrottleFloor > c.ThrottleCap {
		errs = append(errs, fmt.Errorf("%s (%s) must not exceed %s (%s)",
			EnvResourcesThrFloor, c.ThrottleFloor, EnvResourcesThrCap, c.ThrottleCap))
	}
	return errs
}
