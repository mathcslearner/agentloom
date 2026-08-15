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

// validateFields checks the bucket's own configuration — everything
// independent of a per-acquire cost. Adjust (9.3) validates only this, since
// a reconciliation delta is signed and has no cost-vs-capacity relationship.
func (b Bucket) validateFields() error {
	if b.Key == "" {
		return errors.New("ratelimit: bucket key must be non-empty")
	}
	if b.Capacity <= 0 {
		return fmt.Errorf("ratelimit: bucket capacity must be positive, got %d", b.Capacity)
	}
	if b.RefillPerSec < 0 || math.IsNaN(b.RefillPerSec) || math.IsInf(b.RefillPerSec, 0) {
		return fmt.Errorf("ratelimit: refill rate must be finite and non-negative, got %v", b.RefillPerSec)
	}
	return nil
}

func (b Bucket) validate(cost int64) error {
	if err := b.validateFields(); err != nil {
		return err
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

// DeniedBucket reports which dimension(s) of a dual acquire denied it
// (ADR-010): the two-key script grants only when both buckets have their
// tokens, so a denial names requests, tokens, or both. DeniedNone marks an
// allowed acquire. It is the source of the middleware's `bucket` metric
// label — the ratelimit library returns the enum, the caller names it.
type DeniedBucket int

const (
	// DeniedNone marks an allowed acquire (both buckets debited).
	DeniedNone DeniedBucket = 0
	// DeniedRequests means the requests bucket lacked a token; tokens had room.
	DeniedRequests DeniedBucket = 1
	// DeniedTokens means the tokens bucket lacked the estimate; requests had room.
	DeniedTokens DeniedBucket = 2
	// DeniedBoth means neither bucket had room.
	DeniedBoth DeniedBucket = 3
)

// String renders the denied dimension for the middleware's `bucket` label.
func (d DeniedBucket) String() string {
	switch d {
	case DeniedRequests:
		return "requests"
	case DeniedTokens:
		return "tokens"
	case DeniedBoth:
		return "both"
	default:
		return "none"
	}
}

// DualResult reports one two-key (dual-bucket) acquire.
type DualResult struct {
	// Allowed is whether both buckets granted — the acquire is all-or-nothing.
	Allowed bool
	// Denied names the dimension(s) that lacked tokens on a denial;
	// DeniedNone when allowed.
	Denied DeniedBucket
	// RetryAfter is zero when allowed; when denied, the time until *both*
	// dimensions can admit the call — the max of the denying buckets'
	// per-cost refill deadlines (RetryAfterNever if any denying bucket never
	// refills, so no wait can lift it).
	RetryAfter time.Duration
	// RemainingRequests / RemainingTokens are the whole tokens left in each
	// bucket after this acquire (unchanged from the refilled balance on a
	// denial — neither bucket debits).
	RemainingRequests int64
	RemainingTokens   int64
}

// acquireDualScript is the atomic two-key acquire of ADR-010: it refills
// both buckets against one Redis clock, grants only if *both* hold their
// cost, and debits both or neither — the all-or-nothing property that keeps
// the request and token ledgers from drifting apart under steady-state
// throttling (6.4's sequential no-refund acquire would leak a request token
// on every token denial). Each bucket is the same {tokens, ts} hash the
// single-key script maintains, with identical semantics: absent key = full,
// %.17g exact balance, TTL re-armed to time-to-full (PERSIST for a
// never-refilling bucket), backwards-clock elapsed clamped to zero. Refill
// bookkeeping is written for both buckets even on a denial — that only
// advances the clock, minting nothing.
//
// KEYS[1] = requests bucket, KEYS[2] = tokens bucket. ARGV = cap1, rate1,
// cost1, cap2, rate2, cost2, and the test-only now override in epoch
// microseconds (empty in production). Returns {allowed 0|1, tokens1 %.17g,
// tokens2 %.17g, retry_after µs (-1 = never), denied 0|1|2|3}.
var acquireDualScript = redis.NewScript(`
local now
if ARGV[7] == '' then
  local t = redis.call('TIME')
  now = t[1] * 1000000 + t[2]
else
  now = tonumber(ARGV[7])
end

local function refill(key, capacity, rate)
  local tokens = capacity
  local state = redis.call('HMGET', key, 'tokens', 'ts')
  if state[1] and state[2] then
    tokens = tonumber(state[1])
    local elapsed = now - tonumber(state[2])
    if elapsed < 0 then elapsed = 0 end
    tokens = tokens + elapsed * rate / 1000000
    if tokens > capacity then tokens = capacity end
  end
  return tokens
end

local cap1 = tonumber(ARGV[1]); local rate1 = tonumber(ARGV[2]); local cost1 = tonumber(ARGV[3])
local cap2 = tonumber(ARGV[4]); local rate2 = tonumber(ARGV[5]); local cost2 = tonumber(ARGV[6])

local t1 = refill(KEYS[1], cap1, rate1)
local t2 = refill(KEYS[2], cap2, rate2)

local ok1 = cost1 <= t1
local ok2 = cost2 <= t2
local allowed = 0
local denied = 0
local retry_after = 0
if ok1 and ok2 then
  allowed = 1
  t1 = t1 - cost1
  t2 = t2 - cost2
else
  local function ra(ok, cost, tokens, rate)
    if ok then return 0 end
    if rate > 0 then return math.ceil((cost - tokens) / rate * 1000000) end
    return -1
  end
  local r1 = ra(ok1, cost1, t1, rate1)
  local r2 = ra(ok2, cost2, t2, rate2)
  if (not ok1) and (not ok2) then
    denied = 3
  elseif not ok1 then
    denied = 1
  else
    denied = 2
  end
  if r1 == -1 or r2 == -1 then
    retry_after = -1
  elseif r1 >= r2 then
    retry_after = r1
  else
    retry_after = r2
  end
end

local function persist(key, capacity, rate, tokens)
  redis.call('HSET', key, 'tokens', string.format('%.17g', tokens), 'ts', string.format('%.0f', now))
  if rate > 0 then
    redis.call('PEXPIRE', key, math.ceil((capacity - tokens) / rate * 1000) + 1000)
  else
    redis.call('PERSIST', key)
  end
end
persist(KEYS[1], cap1, rate1, t1)
persist(KEYS[2], cap2, rate2, t2)

return {allowed, string.format('%.17g', t1), string.format('%.17g', t2), retry_after, denied}
`)

// AcquireDual atomically takes reqCost from the requests bucket and tokCost
// from the tokens bucket in one step, refilling both for the elapsed time
// first: both grant and both debit, or neither debits and the call is
// denied. This is ADR-010's fleet-wide resource acquire — the requests and
// token dimensions of one provider limit, enforced together so a denial on
// one never leaks a token from the other. Time comes from the Redis server
// clock, so every worker sharing the bucket sees one ledger.
func (l *Limiter) AcquireDual(ctx context.Context, requests Bucket, reqCost int64, tokens Bucket, tokCost int64) (DualResult, error) {
	res, _, _, err := l.acquireDual(ctx, requests, reqCost, tokens, tokCost, "")
	return res, err
}

// acquireDualAt is AcquireDual against an injected clock — the test seam
// behind the injectable-time invariant, exposed only through export_test.go.
// It also returns the exact fractional balances the accounting property test
// compares against its model.
func (l *Limiter) acquireDualAt(ctx context.Context, requests Bucket, reqCost int64, tokens Bucket, tokCost int64, now time.Time) (DualResult, float64, float64, error) {
	if now.IsZero() {
		return DualResult{}, 0, 0, errors.New("ratelimit: acquireDualAt: zero now — pass the injected current time")
	}
	return l.acquireDual(ctx, requests, reqCost, tokens, tokCost, strconv.FormatInt(now.UnixMicro(), 10))
}

func (l *Limiter) acquireDual(ctx context.Context, requests Bucket, reqCost int64, tokens Bucket, tokCost int64, nowArg string) (DualResult, float64, float64, error) {
	if err := requests.validate(reqCost); err != nil {
		return DualResult{}, 0, 0, fmt.Errorf("ratelimit: requests bucket: %w", err)
	}
	if err := tokens.validate(tokCost); err != nil {
		return DualResult{}, 0, 0, fmt.Errorf("ratelimit: tokens bucket: %w", err)
	}
	raw, err := acquireDualScript.Run(ctx, l.client, []string{requests.Key, tokens.Key},
		strconv.FormatInt(requests.Capacity, 10),
		strconv.FormatFloat(requests.RefillPerSec, 'g', -1, 64),
		strconv.FormatInt(reqCost, 10),
		strconv.FormatInt(tokens.Capacity, 10),
		strconv.FormatFloat(tokens.RefillPerSec, 'g', -1, 64),
		strconv.FormatInt(tokCost, 10),
		nowArg,
	).Result()
	if err != nil {
		return DualResult{}, 0, 0, fmt.Errorf("ratelimit: dual acquire from %s + %s: %w", requests.Key, tokens.Key, err)
	}
	return parseDualReply(raw)
}

// parseDualReply decodes the two-key script's
// {allowed, tokens1, tokens2, retry_after, denied} reply.
func parseDualReply(raw any) (DualResult, float64, float64, error) {
	reply, ok := raw.([]any)
	if !ok || len(reply) != 5 {
		return DualResult{}, 0, 0, fmt.Errorf("ratelimit: unexpected dual acquire reply %v", raw)
	}
	allowed, ok := reply[0].(int64)
	if !ok {
		return DualResult{}, 0, 0, fmt.Errorf("ratelimit: unexpected allowed reply %v", reply[0])
	}
	t1, err := parseBalance(reply[1])
	if err != nil {
		return DualResult{}, 0, 0, err
	}
	t2, err := parseBalance(reply[2])
	if err != nil {
		return DualResult{}, 0, 0, err
	}
	retryAfterUS, ok := reply[3].(int64)
	if !ok {
		return DualResult{}, 0, 0, fmt.Errorf("ratelimit: unexpected retry_after reply %v", reply[3])
	}
	denied, ok := reply[4].(int64)
	if !ok {
		return DualResult{}, 0, 0, fmt.Errorf("ratelimit: unexpected denied reply %v", reply[4])
	}
	res := DualResult{
		Allowed:           allowed == 1,
		Denied:            DeniedBucket(denied),
		RemainingRequests: int64(math.Floor(t1)),
		RemainingTokens:   int64(math.Floor(t2)),
	}
	switch {
	case res.Allowed:
		res.RetryAfter = 0
	case retryAfterUS < 0:
		res.RetryAfter = RetryAfterNever
	default:
		res.RetryAfter = time.Duration(retryAfterUS) * time.Microsecond
	}
	return res, t1, t2, nil
}

// adjustScript is the post-call token-cost reconciliation of ADR-010 (ticket
// 9.3). The M9 limiter debits a pre-call token *estimate*; once the provider
// responds, the true usage is known, and this script corrects the bucket by
// the signed error so a biased estimator cannot drift the fleet past the
// provider's real tokens/min budget.
//
// It refills the bucket to `now` exactly as the acquire scripts do (same
// {tokens, ts} hash, absent-key-=-full, backwards-clock clamp, %.17g exact
// state), then applies `delta = actual − estimate`:
//
//   - A **positive** delta (under-estimate — actual exceeded the estimate) is
//     an extra debit, deliberately **unclamped**: it can drive the balance
//     negative. A negative balance is the entire enforcement mechanism — the
//     acquire scripts already deny while `cost > tokens` and grow retry_after
//     as `(cost − tokens)/rate`, so subsequent acquires throttle until refill
//     pays the debt back. No acquire-script change is needed.
//   - A **negative** delta (over-estimate — the estimate exceeded actual) is a
//     refund and **clamps at capacity**: a bucket is never fuller than full,
//     mirroring the refill clamp.
//
// The TTL rule composes with a negative balance for free: time-to-full is
// `(capacity − tokens)/rate`, which for tokens < 0 is longer than a minute of
// refill, so the debt survives in Redis until it is actually paid off — an
// early expiry (absent key = full) would silently erase the debt.
//
// KEYS[1] = bucket hash. ARGV = capacity, refill tokens/sec, delta, and the
// test-only now override in epoch microseconds (empty in production).
// Returns the post-adjust balance as a %.17g string.
var adjustScript = redis.NewScript(`
local capacity = tonumber(ARGV[1])
local rate = tonumber(ARGV[2])
local delta = tonumber(ARGV[3])
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

tokens = tokens - delta
if tokens > capacity then
  tokens = capacity
end

redis.call('HSET', KEYS[1], 'tokens', string.format('%.17g', tokens), 'ts', string.format('%.0f', now))
if rate > 0 then
  redis.call('PEXPIRE', KEYS[1], math.ceil((capacity - tokens) / rate * 1000) + 1000)
else
  redis.call('PERSIST', KEYS[1])
end
return string.format('%.17g', tokens)
`)

// Adjust reconciles a token bucket by a signed delta = actual − estimate
// after the metered call whose estimate was debited has returned (ADR-010,
// ticket 9.3). A positive delta debits the shortfall (may go negative, so
// later acquires throttle until it is refilled away); a negative delta
// refunds the over-estimate (clamped at capacity). It returns the whole
// tokens remaining after the adjust (negative when the bucket is in debt).
// Time comes from the Redis server clock, so every worker sharing the bucket
// reconciles against one ledger.
func (l *Limiter) Adjust(ctx context.Context, b Bucket, delta int64) (int64, error) {
	rem, _, err := l.adjust(ctx, b, delta, "")
	return rem, err
}

// adjustAt is Adjust against an injected clock — the test seam behind the
// injectable-time invariant, exposed only through export_test.go. It also
// returns the exact fractional balance the accounting property test compares
// against its model.
func (l *Limiter) adjustAt(ctx context.Context, b Bucket, delta int64, now time.Time) (int64, float64, error) {
	if now.IsZero() {
		return 0, 0, errors.New("ratelimit: adjustAt: zero now — pass the injected current time")
	}
	return l.adjust(ctx, b, delta, strconv.FormatInt(now.UnixMicro(), 10))
}

func (l *Limiter) adjust(ctx context.Context, b Bucket, delta int64, nowArg string) (int64, float64, error) {
	if err := b.validateFields(); err != nil {
		return 0, 0, err
	}
	raw, err := adjustScript.Run(ctx, l.client, []string{b.Key},
		strconv.FormatInt(b.Capacity, 10),
		strconv.FormatFloat(b.RefillPerSec, 'g', -1, 64),
		strconv.FormatInt(delta, 10),
		nowArg,
	).Result()
	if err != nil {
		return 0, 0, fmt.Errorf("ratelimit: adjusting %s: %w", b.Key, err)
	}
	tokens, err := parseBalance(raw)
	if err != nil {
		return 0, 0, err
	}
	return int64(math.Floor(tokens)), tokens, nil
}

// parseBalance decodes one %.17g bucket-balance string from a script reply.
func parseBalance(v any) (float64, error) {
	s, ok := v.(string)
	if !ok {
		return 0, fmt.Errorf("ratelimit: unexpected tokens reply %v", v)
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, fmt.Errorf("ratelimit: unparseable tokens %q: %w", s, err)
	}
	return f, nil
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
