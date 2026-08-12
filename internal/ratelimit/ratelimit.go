// Package ratelimit implements the generic Redis token bucket of ADR-007:
// one atomic Lua script per acquire — capacity, refill rate, variable cost
// — returning allowed / remaining / retry-after. Atomicity in Lua is the
// point: check-then-decrement as two round trips over-grants under
// concurrency.
//
// The library is deliberately tenant-agnostic. It knows nothing about HTTP
// or LLMs: 6.4 keys buckets per API key and route class, M9 per provider
// resource — the two differ only in key naming and cost semantics, both of
// which are the caller's parameters here.
//
// Clock: the script reads the Redis server clock (TIME) rather than taking
// a caller-injected now. Acquirers are many API replicas and workers with
// independently skewed clocks, and a bucket is shared state — a skewed
// caller passing its own now could mint or destroy tokens for everyone.
// One Redis = one clock. Tests inject fake time through an unexported seam
// the production path cannot reach (see acquireAt).
//
// State: one Redis hash per bucket key (`tokens`, `ts`), where an absent
// key means a full bucket. Every acquire re-arms the key's TTL to
// time-to-full, so an idle bucket expires exactly when its state becomes
// indistinguishable from no state — per-key buckets self-clean instead of
// accumulating forever. A bucket that never refills (RefillPerSec 0) never
// becomes full again, so its state is kept without expiry.
//
// The library does not log: an acquire is a per-request hot-path primitive
// (one Redis round trip — well under the 1ms local target, see
// BenchmarkAcquire), and deny/429 logging belongs to the callers who know
// the tenant semantics.
package ratelimit

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
)

// ErrCostExceedsCapacity rejects an acquire whose cost is larger than the
// bucket's capacity: it could never succeed no matter how long the caller
// waits, which callers must distinguish from a throttle (M9 perm-fails
// these instead of scheduling a delayed requeue). Test with errors.Is.
var ErrCostExceedsCapacity = errors.New("ratelimit: cost exceeds bucket capacity")

// RetryAfterNever is the Result.RetryAfter sentinel for a denial that no
// amount of waiting can lift: the bucket's RefillPerSec is zero, so the
// missing tokens will never come back.
const RetryAfterNever time.Duration = -1

// Bucket describes one token bucket. The zero value is invalid; every
// field is required (RefillPerSec may be zero — a bucket that never
// refills, i.e. a fixed quota).
//
// Config lives with the caller, not in Redis: capacity and rate ride along
// on every acquire, so changing a limit takes effect on the next request.
// A capacity shrink clamps stored tokens down on the next acquire; a grow
// lets the bucket refill up to the new ceiling.
type Bucket struct {
	// Key is the full Redis key holding this bucket's state. Naming is the
	// caller's: 6.4 uses `ratelimit:api:<key_id>:<route_class>`, M9
	// `ratelimit:llm:<resource>`.
	Key string
	// Capacity is the maximum token balance — the burst size.
	Capacity int64
	// RefillPerSec is the continuous refill rate in tokens per second.
	// Zero means the bucket never refills.
	RefillPerSec float64
}

func (b Bucket) validate(cost int64) error {
	if b.Key == "" {
		return errors.New("ratelimit: bucket key must be non-empty")
	}
	if b.Capacity <= 0 {
		return fmt.Errorf("ratelimit: bucket capacity must be positive, got %d", b.Capacity)
	}
	if b.RefillPerSec < 0 || math.IsNaN(b.RefillPerSec) || math.IsInf(b.RefillPerSec, 0) {
		return fmt.Errorf("ratelimit: refill rate must be finite and non-negative, got %v", b.RefillPerSec)
	}
	if cost <= 0 {
		return fmt.Errorf("ratelimit: cost must be positive, got %d", cost)
	}
	if cost > b.Capacity {
		return fmt.Errorf("%w: cost %d > capacity %d", ErrCostExceedsCapacity, cost, b.Capacity)
	}
	return nil
}

// Result reports one acquire.
type Result struct {
	// Allowed is whether the tokens were granted.
	Allowed bool
	// Remaining is the whole tokens left in the bucket after this acquire
	// (unchanged by a denial) — 6.4's X-RateLimit-Remaining header.
	Remaining int64
	// RetryAfter is zero when allowed; when denied, how long until the
	// bucket has refilled enough for this cost (RetryAfterNever if the
	// bucket never refills). It is per-cost: a cheaper acquire may succeed
	// sooner.
	RetryAfter time.Duration
}

// Limiter acquires from token buckets on one Redis. Instances are cheap
// and safe for concurrent use; buckets are addressed per call, so one
// Limiter serves any number of them.
type Limiter struct {
	client redis.Cmdable
}

// New wraps an existing client, mirroring queue.New — the caller owns the
// client's lifecycle.
func New(client redis.Cmdable) *Limiter {
	return &Limiter{client: client}
}

// acquireScript is the atomic token-bucket acquire. The bucket is a hash
// {tokens, ts}: tokens is the balance as of ts, serialized with %.17g so
// the float64 round-trips exactly (tostring's %.14g would silently corrupt
// it), and ts is epoch microseconds — TIME's native precision, exact in a
// Lua double until ~2255.
//
// An absent key is a full bucket, which composes with the TTL rule: after
// every acquire the key expires at time-to-full (plus a safety margin so a
// late expiry never over-grants; an expiry can only fire once the bucket
// would have been full anyway). A persisted balance is always strictly
// below capacity — cost ≥ 1 on a grant, balance < cost on a denial — so
// the TTL is always positive. Rate-zero buckets PERSIST instead: they
// never refill, so expiring the state would silently re-arm the quota.
//
// Negative elapsed time is clamped to zero: Redis TIME can go backwards
// across a failover, and minting tokens from a clock step would over-grant.
//
// KEYS[1] = bucket hash. ARGV = capacity, refill tokens/sec, cost, and the
// test-only now override in epoch microseconds (empty in production, which
// makes the script read TIME).
// Returns {allowed 0|1, tokens %.17g string, retry_after µs (-1 = never)}.
var acquireScript = redis.NewScript(`
local capacity = tonumber(ARGV[1])
local rate = tonumber(ARGV[2])
local cost = tonumber(ARGV[3])
local now
if ARGV[4] == '' then
  local t = redis.call('TIME')
  now = t[1] * 1000000 + t[2]
else
  now = tonumber(ARGV[4])
end

local tokens = capacity
local state = redis.call('HMGET', KEYS[1], 'tokens', 'ts')
if state[1] and state[2] then
  tokens = tonumber(state[1])
  local elapsed = now - tonumber(state[2])
  if elapsed < 0 then
    elapsed = 0
  end
  tokens = tokens + elapsed * rate / 1000000
  if tokens > capacity then
    tokens = capacity
  end
end

local allowed = 0
local retry_after = -1
if cost <= tokens then
  allowed = 1
  tokens = tokens - cost
  retry_after = 0
elseif rate > 0 then
  retry_after = math.ceil((cost - tokens) / rate * 1000000)
end

redis.call('HSET', KEYS[1], 'tokens', string.format('%.17g', tokens), 'ts', string.format('%.0f', now))
if rate > 0 then
  redis.call('PEXPIRE', KEYS[1], math.ceil((capacity - tokens) / rate * 1000) + 1000)
else
  redis.call('PERSIST', KEYS[1])
end
return {allowed, string.format('%.17g', tokens), retry_after}
`)

// Acquire atomically takes cost tokens from the bucket, refilling it for
// the time elapsed since its last acquire first. A denial changes nothing
// but the refill bookkeeping. Time comes from the Redis server clock, so
// every acquirer of a shared bucket sees one consistent ledger regardless
// of its own clock.
func (l *Limiter) Acquire(ctx context.Context, b Bucket, cost int64) (Result, error) {
	res, _, err := l.acquire(ctx, b, cost, "")
	return res, err
}

// acquireAt is Acquire against an injected clock instead of Redis TIME —
// the test seam behind the injectable-time invariant, exposed only through
// export_test.go so production code cannot skew a shared bucket. It also
// returns the exact fractional balance, which the refill-math property
// test compares against its model.
func (l *Limiter) acquireAt(ctx context.Context, b Bucket, cost int64, now time.Time) (Result, float64, error) {
	if now.IsZero() {
		return Result{}, 0, errors.New("ratelimit: acquireAt: zero now — pass the injected current time")
	}
	return l.acquire(ctx, b, cost, strconv.FormatInt(now.UnixMicro(), 10))
}

func (l *Limiter) acquire(ctx context.Context, b Bucket, cost int64, nowArg string) (Result, float64, error) {
	if err := b.validate(cost); err != nil {
		return Result{}, 0, err
	}
	raw, err := acquireScript.Run(ctx, l.client, []string{b.Key},
		strconv.FormatInt(b.Capacity, 10),
		strconv.FormatFloat(b.RefillPerSec, 'g', -1, 64),
		strconv.FormatInt(cost, 10),
		nowArg,
	).Result()
	if err != nil {
		return Result{}, 0, fmt.Errorf("ratelimit: acquiring from %s: %w", b.Key, err)
	}
	return parseAcquireReply(raw)
}

// parseAcquireReply decodes the script's {allowed, tokens, retry_after}
// reply. The balance comes back as a %.17g string because the script's
// exact float64 state is part of the contract the property test pins;
// Remaining is its floor.
func parseAcquireReply(raw any) (Result, float64, error) {
	reply, ok := raw.([]any)
	if !ok || len(reply) != 3 {
		return Result{}, 0, fmt.Errorf("ratelimit: unexpected acquire script reply %v", raw)
	}
	allowed, ok := reply[0].(int64)
	if !ok {
		return Result{}, 0, fmt.Errorf("ratelimit: unexpected allowed reply %v", reply[0])
	}
	tokensStr, ok := reply[1].(string)
	if !ok {
		return Result{}, 0, fmt.Errorf("ratelimit: unexpected tokens reply %v", reply[1])
	}
	tokens, err := strconv.ParseFloat(tokensStr, 64)
	if err != nil {
		return Result{}, 0, fmt.Errorf("ratelimit: unparseable tokens %q: %w", tokensStr, err)
	}
	retryAfterUS, ok := reply[2].(int64)
	if !ok {
		return Result{}, 0, fmt.Errorf("ratelimit: unexpected retry_after reply %v", reply[2])
	}
	res := Result{
		Allowed:   allowed == 1,
		Remaining: int64(math.Floor(tokens)),
	}
	switch {
	case res.Allowed:
		res.RetryAfter = 0
	case retryAfterUS < 0:
		res.RetryAfter = RetryAfterNever
	default:
		res.RetryAfter = time.Duration(retryAfterUS) * time.Microsecond
	}
	return res, tokens, nil
}
