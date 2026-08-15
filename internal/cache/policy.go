package cache

import (
	"time"

	"github.com/mathcslearner/agentloom/internal/dag"
	"github.com/mathcslearner/agentloom/internal/plugin"
)

// Decision is the outcome of the cache policy for one step attempt: whether
// to read a hit, whether to write a miss, and under what TTL and scope. The
// zero value is a full bypass (neither read nor write) — the safe default
// the middleware falls back to whenever the policy declines.
type Decision struct {
	// Read: consult the cache before executing; a hit skips the provider
	// and the rate limiter entirely (9.5).
	Read bool
	// Write: store the result after a successful execution on a miss.
	Write bool
	// TTL is the entry's time-to-live when Write is true.
	TTL time.Duration
	// Scope bounds sharing of the entry (feeds KeyInput.Scope).
	Scope dag.CacheScope
}

// bypass is the zero Decision — never read, never write.
func bypass() Decision { return Decision{} }

// Decide is the encoded ADR-011 policy matrix. It takes the plugin's
// capability flags, an executor-supplied determinism signal, the step's
// authored cache policy (nil = none), and the fleet's default TTL, and
// returns whether this attempt reads and/or writes the cache.
//
// The rules, in order:
//
//  1. Eligibility is a hard gate: a plugin that is not Cacheable, or is
//     SideEffectful, is never cached — no step-level policy can open it
//     (ADR-009: side-effectful outputs must never be served from cache). A
//     `cache` block on such a step is a silent bypass, not an error.
//  2. Within eligible plugins the default is determinism-driven: a
//     deterministic step (an llm call at temperature 0, a pure tool) caches
//     read-write by default; a non-deterministic one (temperature > 0 or the
//     provider default, a corpus-mutating retrieve) bypasses unless opted in.
//  3. A step's `cache.mode` overrides the default within eligibility: off
//     bypasses, read_write reads and writes, read_only reads but never writes.
//  4. TTL is the step's `cache.ttl` when set (validated ≤ MaxCacheTTL at
//     submit; re-parsed defensively here), else the fleet default. Scope is
//     the step's `cache.scope` when set, else global.
//
// determinism is the caller's judgment because it is plugin-specific: the
// llm executor passes temperature != nil && *temperature == 0; a pure tool
// passes true; a retrieve passes false.
func Decide(caps plugin.Capabilities, deterministic bool, policy *dag.CachePolicy, defaultTTL time.Duration) Decision {
	if !caps.Cacheable || caps.SideEffectful {
		return bypass()
	}

	mode := defaultMode(deterministic)
	if policy != nil && policy.Mode != "" {
		mode = policy.Mode
	}

	var read, write bool
	switch mode {
	case dag.CacheOff:
		return bypass()
	case dag.CacheReadWrite:
		read, write = true, true
	case dag.CacheReadOnly:
		read, write = true, false
	default:
		// An unknown mode should have been rejected at submit; treat it
		// conservatively as a bypass rather than guessing.
		return bypass()
	}

	return Decision{
		Read:  read,
		Write: write,
		TTL:   resolveTTL(policy, defaultTTL),
		Scope: resolveScope(policy),
	}
}

// defaultMode is the eligibility-passed default: read_write for
// deterministic steps, off for the rest.
func defaultMode(deterministic bool) dag.CacheMode {
	if deterministic {
		return dag.CacheReadWrite
	}
	return dag.CacheOff
}

// resolveTTL picks the step's TTL over the fleet default. The authored TTL
// is validated parseable and in-bounds at submit; a defensive re-parse
// failure falls back to the default rather than caching forever.
func resolveTTL(policy *dag.CachePolicy, defaultTTL time.Duration) time.Duration {
	if policy == nil || policy.TTL == "" {
		return defaultTTL
	}
	d, err := time.ParseDuration(policy.TTL)
	if err != nil || d <= 0 {
		return defaultTTL
	}
	return d
}

// resolveScope picks the step's scope over the global default.
func resolveScope(policy *dag.CachePolicy) dag.CacheScope {
	if policy == nil || policy.Scope == "" {
		return dag.CacheGlobal
	}
	return policy.Scope
}
