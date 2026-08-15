// Package cache is the definition-and-key half of ADR-011: the response
// cache's key builder and its eligibility/default policy. The runtime half
// — the Redis read-through/write-through middleware, the hit accounting, the
// admin bust surface — is the engine's (M9.5–9.6); this package computes the
// deterministic key an entry is stored under and decides whether a given
// step may read or write the cache at all.
//
// It is deliberately a leaf, like internal/limits and internal/llm: it
// imports internal/dag (the authored CachePolicy) and internal/plugin (the
// capability flags) and nothing else — never internal/exec, the engine, or a
// Redis client. The 9.5 middleware projects a resolved request onto this
// package's inputs and owns the storage.
//
// The two guarantees this package exists to make:
//
//   - A key is a pure, stable function of everything that can change the
//     output: the definition format version, the executor plugin and the
//     concrete external plugin (provider/tool/retriever) with their behavioral
//     versions (ADR-009: bump a version whenever cached outputs should
//     invalidate), the resolved model and sampling params, and the rendered
//     request. Semantically equal requests that differ only in JSON key order
//     or number spelling produce the *same* key; any semantic change produces
//     a *different* one.
//   - Caching is opt-in-safe: a plugin that is not Cacheable, or is
//     SideEffectful, is never read from or written to the cache — a hard gate
//     no step-level policy can open (ADR-011). Within eligible plugins, the
//     default caches deterministic steps and bypasses the rest; a step's
//     `cache` block overrides that decision.
package cache

import (
	"errors"

	"github.com/mathcslearner/agentloom/internal/plugin"
)

// ErrValueTooLarge is the store contract's oversized-value signal (ADR-011's
// size-cap fallback): a value over the configured cap is skipped, not stored
// — no chunking, no Postgres spill. It lives here in the leaf so both the
// Redis store (which returns it) and the engine middleware (which routes it
// to a store-skip bypass metric) can reference it without importing each
// other.
var ErrValueTooLarge = errors.New("cache: value exceeds size cap")

// KeySchemaVersion is this key builder's own format version. It is mixed
// into every key and into the Redis key namespace, so bumping it is the
// fleet-wide invalidation lever: a change to *how* keys are built (a new
// component, a different canonicalization) bumps this integer and strands
// every previously written entry behind an unreachable prefix, rather than
// risking a silent collision with an old entry built the old way.
const KeySchemaVersion = 1

// PluginRef is a plugin's cache-relevant identity: its kind, its name, and
// its behavioral semver version (ADR-009). Two refs that differ in any
// field key different cache entries.
type PluginRef struct {
	// Kind is the plugin kind (executor, model_provider, tool, retriever).
	Kind plugin.Kind
	// Name is the plugin name within its kind.
	Name string
	// Version is the plugin's semver version string.
	Version string
}

// empty reports whether the ref is unusable (missing name or version).
func (r PluginRef) empty() bool { return r.Name == "" || r.Version == "" }
