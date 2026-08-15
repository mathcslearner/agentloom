// Package resource is the ADR-010 adapter that maps the fleet-wide
// resource-limit configuration (internal/limits) onto the generic Redis
// token buckets (internal/ratelimit). It is the one place that imports both
// sides, keeping each a leaf: the config package parses and resolves names,
// the ratelimit library runs the atomic buckets, and this package binds a
// resolved resource name + a pre-call token estimate to the two buckets and
// the dual acquire.
//
// The M9 limiter middleware (internal/engine) calls Acquire once per
// cost-bearing executor invocation, before the provider call. An
// unconfigured resource (Set.Lookup not-found) is unlimited by decision —
// Acquire returns an unlimited Decision and touches no Redis, so naming a
// resource in config is opt-in governance (ADR-010's unknown-resource
// policy). Redis or script errors surface as errors; the middleware
// fails open, exactly as 6.4 does for API limits.
package resource

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/mathcslearner/agentloom/internal/limits"
	"github.com/mathcslearner/agentloom/internal/ratelimit"
)

// DefaultKeyPrefix is the Redis key namespace for resource buckets when the
// operator sets none. Each bucket key is "<prefix>:<resource>:<dimension>".
const DefaultKeyPrefix = "ratelimit:resource"

// RetryAfterNever mirrors ratelimit.RetryAfterNever: a denial no wait can
// lift, because a denying bucket never refills. The middleware perm-fails
// these to the DLQ (ADR-010 wait-vs-never) rather than throttling forever.
const RetryAfterNever = ratelimit.RetryAfterNever

// Decision reports one resource acquire.
type Decision struct {
	// Limited is false when the resource has no configured limit: the call
	// was not metered and no Redis round trip happened (unlimited by
	// policy). Allowed is then true.
	Limited bool
	// Allowed is whether the call may proceed. True for an unlimited
	// resource and for a granted acquire; false for a denial.
	Allowed bool
	// Resource is the resolved config entry's name the buckets keyed off —
	// the exact name or the wildcard that matched (the bounded metric
	// label). Empty when the resource was unlimited.
	Resource string
	// Bucket names the denying dimension on a denial (requests/tokens/both),
	// for the throttle event and the `bucket` metric label; empty otherwise.
	Bucket string
	// RetryAfter is the limiter's own time until both dimensions can admit
	// the call (ratelimit.RetryAfterNever if a denying bucket never
	// refills). Zero when allowed.
	RetryAfter time.Duration
	// TokensMetered reports whether this acquire actually debited the token
	// bucket by the estimate — true only when the resource configures a token
	// dimension and the estimate was positive (a dual acquire, or a
	// tokens-only single acquire). It is false for unlimited resources,
	// requests-only resources, and tool claims (estTokens 0). The engine
	// reconciles the post-call estimate error (9.3) only when this is true and
	// the call was granted: nothing else touched the token ledger.
	TokensMetered bool
}

// ErrCostExceedsCapacity is returned when the token estimate exceeds the
// resource's token-bucket capacity: no refill can ever admit the call, so
// the middleware perm-fails it to the DLQ rather than throttling forever
// (ADR-010's wait-vs-never). Wraps ratelimit.ErrCostExceedsCapacity, so
// errors.Is against either holds.
var ErrCostExceedsCapacity = ratelimit.ErrCostExceedsCapacity

// Limiter acquires fleet-wide resource limits: it resolves a resource name
// against the configured Set and, when the resource is limited, acquires its
// request and token buckets atomically. Instances are cheap and safe for
// concurrent use.
type Limiter struct {
	limiter   *ratelimit.Limiter
	set       *limits.Set
	keyPrefix string
}

// New builds a resource Limiter over a Redis client, the validated
// resource-limit Set, and a key prefix (DefaultKeyPrefix when empty). A nil
// or empty Set means every resource is unlimited — the limiter still
// constructs, and every Acquire short-circuits to an unlimited Decision.
func New(client redis.Cmdable, set *limits.Set, keyPrefix string) (*Limiter, error) {
	if client == nil {
		return nil, errors.New("resource: New requires a Redis client")
	}
	if keyPrefix == "" {
		keyPrefix = DefaultKeyPrefix
	}
	return &Limiter{limiter: ratelimit.New(client), set: set, keyPrefix: keyPrefix}, nil
}

// Acquire debits the resource's buckets for one provider call: the requests
// bucket by 1 and the tokens bucket by estTokens (the pre-call estimate). It
// resolves the name against the configured Set first (exact → wildcard →
// unlimited); an unlimited resource returns immediately with no Redis call.
//
// When the resource configures both dimensions and estTokens > 0, the two
// buckets are acquired atomically (all-or-nothing). When only one dimension
// is effective — a requests-only resource, a tokens-nil config, or
// estTokens == 0 (a tool claim, which meters no tokens) — a single-bucket
// acquire runs; a resource with only its token dimension effective and
// estTokens == 0 meters nothing and returns unlimited.
func (l *Limiter) Acquire(ctx context.Context, resourceName string, estTokens int64) (Decision, error) {
	res, ok := l.set.Lookup(resourceName)
	if !ok {
		// Unknown resource → unlimited (ADR-010). No Redis, no metering.
		return Decision{Limited: false, Allowed: true}, nil
	}

	reqBucket, hasReq := l.bucket(res.Name, "requests", res.Requests)
	tokBucket, hasTok := l.bucket(res.Name, "tokens", res.Tokens)
	// The token dimension only bites when there is a positive estimate to
	// charge; a tool claim (estTokens 0) never touches the token bucket.
	meterTokens := hasTok && estTokens > 0

	switch {
	case hasReq && meterTokens:
		return l.acquireDual(ctx, res.Name, reqBucket, tokBucket, estTokens)
	case hasReq:
		return l.acquireSingle(ctx, res.Name, reqBucket, 1, ratelimit.DeniedRequests)
	case meterTokens:
		return l.acquireSingle(ctx, res.Name, tokBucket, estTokens, ratelimit.DeniedTokens)
	default:
		// Nothing effective to meter (a tokens-only resource with a zero
		// estimate): treat as unlimited for this call.
		return Decision{Limited: false, Allowed: true}, nil
	}
}

// Reconcile corrects the resource's token bucket by the post-call estimate
// error after a metered call has returned (ADR-010, ticket 9.3): the limiter
// debited estTokens before the call, and the provider reported actualTokens,
// so the bucket is adjusted by delta = actualTokens − estTokens — an extra
// debit when the estimate was low (drives the bucket toward debt, throttling
// later calls until refill repays it) or a refund when it was high. Only the
// token bucket is touched; the requests dimension's cost of 1 is exact.
//
// It resolves the name against the configured Set first (exact → wildcard →
// unlimited). An unlimited resource, a resource with no token dimension, or a
// zero delta reconciles nothing and touches no Redis — nothing was debited to
// correct. The middleware calls this only for a granted, token-metered
// acquire, so the estimate really is on the ledger to correct.
func (l *Limiter) Reconcile(ctx context.Context, resourceName string, estTokens, actualTokens int64) error {
	delta := actualTokens - estTokens
	if delta == 0 {
		return nil
	}
	res, ok := l.set.Lookup(resourceName)
	if !ok {
		return nil // unlimited — nothing was metered
	}
	tokBucket, hasTok := l.bucket(res.Name, "tokens", res.Tokens)
	if !hasTok {
		return nil // no token dimension — nothing to reconcile
	}
	if _, err := l.limiter.Adjust(ctx, tokBucket, delta); err != nil {
		return err
	}
	return nil
}

// bucket builds the ratelimit.Bucket for one dimension of a resource, or
// reports absent when that dimension is unconfigured (nil Rate).
func (l *Limiter) bucket(resourceName, dimension string, rate *limits.Rate) (ratelimit.Bucket, bool) {
	if rate == nil {
		return ratelimit.Bucket{}, false
	}
	return ratelimit.Bucket{
		Key:          fmt.Sprintf("%s:%s:%s", l.keyPrefix, resourceName, dimension),
		Capacity:     rate.Capacity(),
		RefillPerSec: rate.RefillPerSec(),
	}, true
}

func (l *Limiter) acquireDual(ctx context.Context, name string, req, tok ratelimit.Bucket, estTokens int64) (Decision, error) {
	res, err := l.limiter.AcquireDual(ctx, req, 1, tok, estTokens)
	if err != nil {
		return Decision{}, err
	}
	return Decision{
		Limited: true, Allowed: res.Allowed, Resource: name,
		Bucket: res.Denied.String(), RetryAfter: res.RetryAfter,
		TokensMetered: true,
	}, nil
}

func (l *Limiter) acquireSingle(ctx context.Context, name string, b ratelimit.Bucket, cost int64, dim ratelimit.DeniedBucket) (Decision, error) {
	res, err := l.limiter.Acquire(ctx, b, cost)
	if err != nil {
		return Decision{}, err
	}
	d := Decision{
		Limited: true, Allowed: res.Allowed, Resource: name, RetryAfter: res.RetryAfter,
		// A single acquire on the token dimension (a tokens-only resource with
		// a positive estimate) debited tokens; a requests-only acquire did not.
		TokensMetered: dim == ratelimit.DeniedTokens,
	}
	if !res.Allowed {
		d.Bucket = dim.String()
	}
	return d, nil
}
