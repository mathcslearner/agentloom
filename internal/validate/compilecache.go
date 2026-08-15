package validate

// Compiled-artifact cache (ticket 11.2, ADR-013). The deterministic
// built-in validators (json_schema, regex, cel) turn their config into a
// compiled artifact — a *jsonschema.Schema, a *regexp.Regexp, a
// *dag.CompiledExpr — that is expensive to build and pure in the config.
// The engine's resolveChain runs at *every claim* (per attempt, per retry,
// per takeover) and the same materialized policy bytes recur across every
// run of a definition, so recompiling per attempt would be wasteful and
// would violate the ticket's "compiled artifacts reused across attempts"
// criterion.
//
// compileCache is the reuse mechanism: a validator holds one, keyed by the
// SHA-256 of the raw config bytes (materialized policy JSON is byte-stable,
// so identical configs hit), and both the pre-flight ConfigCompiler gate
// and Validate go through get. An artifact is therefore compiled once per
// distinct config per worker process and reused everywhere after. The cache
// is bounded (a runaway variety of configs cannot grow it without limit)
// and safe for the concurrent Validate calls the SPI promises.

import (
	"crypto/sha256"
	"sync"
)

// maxCompileCacheEntries bounds a validator's compiled-artifact cache. A
// worker sees at most one distinct config per authored chain entry across
// its live definitions; the cap is far above any realistic fleet and exists
// only as a runaway backstop. On overflow the cache is cleared wholesale
// (simplest correct eviction — the next accesses recompile), never unbounded.
const maxCompileCacheEntries = 1024

// compileCache memoizes compiled artifacts of type T keyed by the hash of
// the config bytes that produced them. compile is the pure, possibly
// expensive builder; a build error is returned to the caller and never
// cached (a bad config is a permanent failure the pre-flight gate reports,
// not a memoized artifact). Safe for concurrent use.
type compileCache[T any] struct {
	compile func(config []byte) (T, error)
	mu      sync.Mutex
	entries map[[32]byte]T
	// compiles counts distinct compilations performed — the test seam that
	// proves reuse across attempts (no per-attempt recompilation).
	compiles int
}

// newCompileCache builds a cache over the given pure compiler.
func newCompileCache[T any](compile func(config []byte) (T, error)) *compileCache[T] {
	return &compileCache[T]{compile: compile, entries: make(map[[32]byte]T)}
}

// get returns the compiled artifact for config, building and caching it on
// first sight and returning the cached one thereafter. A compile error is
// returned uncached.
func (c *compileCache[T]) get(config []byte) (T, error) {
	key := sha256.Sum256(config)

	c.mu.Lock()
	if v, ok := c.entries[key]; ok {
		c.mu.Unlock()
		return v, nil
	}
	c.mu.Unlock()

	// Compile outside the lock — building a schema/regex/program is the
	// expensive part and must not serialize unrelated configs. A concurrent
	// duplicate compile of the same key is possible and harmless (the last
	// writer wins; both artifacts are equivalent pure functions of config).
	v, err := c.compile(config)
	if err != nil {
		var zero T
		return zero, err
	}

	c.mu.Lock()
	if len(c.entries) >= maxCompileCacheEntries {
		c.entries = make(map[[32]byte]T)
	}
	c.entries[key] = v
	c.compiles++
	c.mu.Unlock()
	return v, nil
}

// compileCount reports how many distinct compilations the cache has
// performed — used by tests to assert reuse.
func (c *compileCache[T]) compileCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.compiles
}
