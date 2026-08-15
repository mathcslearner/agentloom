// Package redisstore is the Redis backing store for the response cache
// (ticket 9.5, ADR-011): the write-through storage the 9.5 engine middleware
// reads a hit from and writes a miss to. It is deliberately a thin
// byte-oriented key/value layer — it owns the Redis client, the key prefix
// and namespacing (via cache.RedisKey), the per-value size cap, and the TTL
// on write; it does NOT know the shape of a cached entry. The engine
// marshals its {output, usage} value and hands the store opaque bytes, which
// keeps this package free of any exec/engine dependency and keeps internal/cache
// a true leaf (this subpackage is the only importer of go-redis on that side,
// mirroring how internal/retrieval/pgfts is the only store importer).
//
// The store is the disposable-derived-data half of ADR-011: a Redis outage
// or a corrupt value is a cache miss, never a run failure — the failure
// stance lives in the engine middleware (fail-open), and this package simply
// surfaces Redis errors and the oversized-value signal for it to route.
package redisstore

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/mathcslearner/agentloom/internal/cache"
)

// DefaultKeyPrefix is the Redis key namespace for cache entries when the
// operator sets none. Each entry key is
// "<prefix>:v<KeySchemaVersion>:<kind>:<name>:<hash>" (cache.RedisKey) — the
// version and concrete-plugin segments give 9.6 its bust-by-prefix
// granularity.
const DefaultKeyPrefix = "cache"

// DefaultMaxValueBytes caps a stored entry, mirroring the tool HTTP response
// cap (ticket 8.7): a value over the cap is skipped, not stored (ADR-011 —
// no chunking, no Postgres spill), so a pathological giant output cannot
// evict thousands of useful small ones.
const DefaultMaxValueBytes = int64(1 << 20) // 1 MiB

// ErrValueTooLarge re-exports the store contract's oversized-value signal
// (defined in the cache leaf so the engine can match it without importing
// this package). Set returns it when the marshaled entry exceeds the cap;
// the engine middleware treats it as a store-skip bypass, never a run
// failure (ADR-011).
var ErrValueTooLarge = cache.ErrValueTooLarge

// Store reads and writes response-cache entries in Redis. Instances are
// cheap and safe for concurrent use (go-redis clients are).
type Store struct {
	client       redis.Cmdable
	keyPrefix    string
	maxValueByte int64
}

// New builds a Store over a Redis client. keyPrefix defaults to
// DefaultKeyPrefix when empty; maxValueBytes defaults to DefaultMaxValueBytes
// when non-positive.
func New(client redis.Cmdable, keyPrefix string, maxValueBytes int64) (*Store, error) {
	if client == nil {
		return nil, errors.New("redisstore: New requires a Redis client")
	}
	if keyPrefix == "" {
		keyPrefix = DefaultKeyPrefix
	}
	if maxValueBytes <= 0 {
		maxValueBytes = DefaultMaxValueBytes
	}
	return &Store{client: client, keyPrefix: keyPrefix, maxValueByte: maxValueBytes}, nil
}

// Get reads the entry stored under the content key for the given concrete
// plugin. It returns (value, true, nil) on a hit, (nil, false, nil) on a
// miss (absent key), and (nil, false, err) on a Redis error — the middleware
// treats a miss and an error identically (proceed to execute), but the error
// is surfaced so it can be counted as a fail-open.
func (s *Store) Get(ctx context.Context, plugin cache.PluginRef, key string) ([]byte, bool, error) {
	val, err := s.client.Get(ctx, cache.RedisKey(s.keyPrefix, plugin, key)).Bytes()
	if errors.Is(err, redis.Nil) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("redisstore: get: %w", err)
	}
	return val, true, nil
}

// Set writes an entry under the content key with the given TTL. A value over
// the configured size cap is not stored and returns ErrValueTooLarge (the
// caller records a bypass). ttl must be positive — the caller's policy
// (cache.Decide) always resolves one; a non-positive ttl is rejected rather
// than stored without expiry, so a cache entry can never outlive its bound.
func (s *Store) Set(ctx context.Context, plugin cache.PluginRef, key string, val []byte, ttl time.Duration) error {
	if int64(len(val)) > s.maxValueByte {
		return ErrValueTooLarge
	}
	if ttl <= 0 {
		return fmt.Errorf("redisstore: set requires a positive ttl, got %s", ttl)
	}
	if err := s.client.Set(ctx, cache.RedisKey(s.keyPrefix, plugin, key), val, ttl).Err(); err != nil {
		return fmt.Errorf("redisstore: set: %w", err)
	}
	return nil
}
