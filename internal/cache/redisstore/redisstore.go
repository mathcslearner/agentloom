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
//
// Ticket 9.6 adds the ops surface on top of the same store: Get/Set keep
// per-plugin hit/miss/store counters in Redis (a hash beside the entries,
// best-effort — a counter update that fails never touches the step) so the
// admin stats endpoint has a durable, cross-process source that reconciles
// against the engine's Prometheus counters; Bust SCAN-batches a non-blocking
// UNLINK by the RedisKey namespace (all / one kind / one concrete plugin);
// Stats reads the counters back. The engine middleware is unchanged — the
// stats ride inside the store the worker already wires.
package redisstore

import (
	"context"
	"errors"
	"fmt"
	"sort"
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

// scanCount is the SCAN COUNT hint for Bust and Stats: a batch size that
// keeps each server-side scan step short (non-blocking, ADR-011) while
// bounding the number of round-trips over a large keyspace.
const scanCount = int64(512)

// statsTTL bounds the per-plugin counter hashes. It is re-armed on every
// update, so an actively-used plugin's counters never expire; a plugin no
// longer invoked lets its stale counters self-evict after this window
// (mirroring the ratelimit buckets' self-cleaning TTL, ticket 6.3). Long
// enough that a lull between runs never drops live counters.
const statsTTL = 30 * 24 * time.Hour

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
		s.bumpStat(ctx, plugin, cache.StatsFieldMisses)
		return nil, false, nil
	}
	if err != nil {
		// A Redis error increments nothing (the op never reached the server);
		// the engine records this as a fail-open, so store and engine counters
		// stay aligned.
		return nil, false, fmt.Errorf("redisstore: get: %w", err)
	}
	s.bumpStat(ctx, plugin, cache.StatsFieldHits)
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
	s.bumpStat(ctx, plugin, cache.StatsFieldStores)
	return nil
}

// bumpStat increments one counter in a plugin's stats hash and re-arms the
// hash TTL, in a single pipelined round-trip. It is best-effort by design:
// a failure is swallowed, never propagated, because the stats are pure
// observability (ticket 9.6) and must never affect a cache read, a write, or
// the step behind them. The counters mirror the engine's Prometheus cache
// counters on the normal path — one HINCRBY here per Redis op the engine
// would count — which is what lets the stats endpoint reconcile against
// them (the one documented divergence: a corrupt stored value is a store
// hit here but an engine fail-open there, ADR-011).
func (s *Store) bumpStat(ctx context.Context, plugin cache.PluginRef, field string) {
	statsKey := cache.StatsRedisKey(s.keyPrefix, plugin)
	pipe := s.client.Pipeline()
	pipe.HIncrBy(ctx, statsKey, field, 1)
	pipe.PExpire(ctx, statsKey, statsTTL)
	_, _ = pipe.Exec(ctx) //nolint:errcheck // best-effort: stats never gate a step
}

// Bust deletes every cache entry matching the namespace selector, SCAN-batched
// so it never blocks Redis, and returns the number of keys removed (ticket
// 9.6, ADR-011). Deletion uses UNLINK, so the memory reclaim is asynchronous
// on the server too. The stats counters live outside every bust pattern, so
// a bust reclaims entries without erasing history. Semantics are point-in-time:
// an entry written concurrently by a live worker after this scan passed its
// slot survives — a bust is a best-effort reclaim, not a barrier.
func (s *Store) Bust(ctx context.Context, match cache.BustMatch) (int64, error) {
	pattern, err := cache.BustPattern(s.keyPrefix, match)
	if err != nil {
		return 0, err
	}
	var (
		deleted int64
		cursor  uint64
	)
	for {
		keys, next, err := s.client.Scan(ctx, cursor, pattern, scanCount).Result()
		if err != nil {
			return deleted, fmt.Errorf("redisstore: bust scan: %w", err)
		}
		if len(keys) > 0 {
			// UNLINK is idempotent: a key another scan pass already removed
			// counts 0, so a SCAN duplicate never inflates the total.
			n, err := s.client.Unlink(ctx, keys...).Result()
			if err != nil {
				return deleted, fmt.Errorf("redisstore: bust unlink: %w", err)
			}
			deleted += n
		}
		cursor = next
		if cursor == 0 {
			return deleted, nil
		}
	}
}

// Stats reads back every plugin's cumulative cache counters (ticket 9.6),
// SCAN-batched over the stats namespace. The result is sorted by (kind,
// name) for a stable listing. A malformed counter value is surfaced as an
// error (worth reporting, unlike a corrupt entry); a stray key that does not
// parse as a stats key is skipped.
func (s *Store) Stats(ctx context.Context) ([]cache.PluginStats, error) {
	pattern := cache.StatsPattern(s.keyPrefix)
	// Dedupe across SCAN passes: the same key can appear in more than one
	// batch, and a plugin must be reported exactly once.
	seen := make(map[string]struct{})
	var cursor uint64
	for {
		keys, next, err := s.client.Scan(ctx, cursor, pattern, scanCount).Result()
		if err != nil {
			return nil, fmt.Errorf("redisstore: stats scan: %w", err)
		}
		for _, k := range keys {
			seen[k] = struct{}{}
		}
		cursor = next
		if cursor == 0 {
			break
		}
	}

	out := make([]cache.PluginStats, 0, len(seen))
	for k := range seen {
		ref, ok := cache.ParseStatsKey(s.keyPrefix, k)
		if !ok {
			continue
		}
		fields, err := s.client.HGetAll(ctx, k).Result()
		if err != nil {
			return nil, fmt.Errorf("redisstore: stats hgetall %s: %w", k, err)
		}
		hits, err := cache.ParseCounter(fields[cache.StatsFieldHits])
		if err != nil {
			return nil, err
		}
		misses, err := cache.ParseCounter(fields[cache.StatsFieldMisses])
		if err != nil {
			return nil, err
		}
		stores, err := cache.ParseCounter(fields[cache.StatsFieldStores])
		if err != nil {
			return nil, err
		}
		out = append(out, cache.NewPluginStats(ref, hits, misses, stores))
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Kind != out[j].Kind {
			return out[i].Kind < out[j].Kind
		}
		return out[i].Name < out[j].Name
	})
	return out, nil
}
