package config

import (
	"fmt"
	"time"
)

// Environment variables read by CacheConfig.
const (
	EnvCacheEnabled       = "AGENTLOOM_CACHE_ENABLED"
	EnvCacheKeyPrefix     = "AGENTLOOM_CACHE_KEY_PREFIX"
	EnvCacheDefaultTTL    = "AGENTLOOM_CACHE_DEFAULT_TTL"
	EnvCacheMaxValueBytes = "AGENTLOOM_CACHE_MAX_VALUE_BYTES"
)

// Cache defaults (ticket 9.5, ADR-011).
const (
	// DefaultCacheKeyPrefix is the Redis key namespace for cache entries;
	// a test-isolation knob, and the bust-by-prefix root (9.6).
	DefaultCacheKeyPrefix = "cache"
	// DefaultCacheDefaultTTL is the fleet default entry TTL applied when an
	// opted-in step sets no `cache.ttl`. 24h is a dev-scale guess (ADR-011);
	// a production deploy revisits it from hit/evict data.
	DefaultCacheDefaultTTL = 24 * time.Hour
	// DefaultCacheMaxValueBytes caps a stored entry, mirroring the tool HTTP
	// response cap (8.7): an oversized value is skipped, not stored.
	DefaultCacheMaxValueBytes = int64(1 << 20) // 1 MiB
	// MaxCacheDefaultTTL bounds the configured fleet default TTL. It mirrors
	// dag.MaxCacheTTL (30 days) — a cached AI output kept past a month is a
	// staleness hazard — but is kept a local constant so config stays a leaf
	// (it never imports dag).
	MaxCacheDefaultTTL = 30 * 24 * time.Hour
)

// CacheConfig configures the response cache (ticket 9.5, internal/cache +
// internal/cache/redisstore). The worker builds a redisstore over the same
// Redis client the queue uses (the shared coordination Redis) and wires it
// into the engine's cache middleware. Since ticket 9.6 the API also builds a
// redisstore over this config — but solely for the admin ops surface
// (bust/stats), never for dispatch or for reading a cached result, and it
// fails soft when Redis is down, so ADR-002's rule that Postgres is the
// API's only hard dependency holds. Enabled by default: the worker already
// requires Redis, and a default-on cache is what makes identical
// deterministic steps hit with zero configuration.
type CacheConfig struct {
	// Enabled turns the response cache on. Default true; set
	// AGENTLOOM_CACHE_ENABLED=false to run every step uncached.
	Enabled bool
	// KeyPrefix is the Redis key namespace for cache entries
	// (AGENTLOOM_CACHE_KEY_PREFIX). A test-isolation knob; defaults to
	// DefaultCacheKeyPrefix.
	KeyPrefix string
	// DefaultTTL is the entry TTL applied when an opted-in step sets no
	// `cache.ttl`. Must be positive and at most MaxCacheDefaultTTL.
	DefaultTTL time.Duration
	// MaxValueBytes caps a stored entry; an oversized value is skipped, not
	// stored (ADR-011). Must be positive.
	MaxValueBytes int64
}

func defaultCacheConfig() CacheConfig {
	return CacheConfig{
		Enabled:       true,
		KeyPrefix:     DefaultCacheKeyPrefix,
		DefaultTTL:    DefaultCacheDefaultTTL,
		MaxValueBytes: DefaultCacheMaxValueBytes,
	}
}

// applyEnv overrides fields from the environment, reporting one error per
// invalid variable and rejecting a default TTL over the ceiling.
func (c *CacheConfig) applyEnv(fn LookupFunc) []error {
	var errs []error
	errs = applyBool(errs, fn, EnvCacheEnabled, &c.Enabled)
	applyString(fn, EnvCacheKeyPrefix, &c.KeyPrefix)
	errs = applyPositiveDuration(errs, fn, EnvCacheDefaultTTL, &c.DefaultTTL)
	errs = applyPositiveInt64(errs, fn, EnvCacheMaxValueBytes, &c.MaxValueBytes)
	if c.DefaultTTL > MaxCacheDefaultTTL {
		errs = append(errs, fmt.Errorf("%s (%s) must not exceed %s", EnvCacheDefaultTTL, c.DefaultTTL, MaxCacheDefaultTTL))
	}
	return errs
}
