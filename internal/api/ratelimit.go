package api

// This file is per-client API rate limiting (ticket 6.4, ADR-007): a
// middleware mounted after requireScope on every /v1 route, acquiring from
// two token buckets in Redis via internal/ratelimit — the caller's
// per-key_id bucket for the route's class (submits stricter than reads,
// admin strictest), then the API-wide global safety bucket. Either denial
// is a 429 with Retry-After; the X-RateLimit-* headers always describe the
// caller's own class bucket.
//
// Decisions (recorded in ADR-007's 6.4 as-built section):
//
//   - Fail-open: an Acquire error (Redis down) allows the request through
//     with an error log. Rate limits are protective, not correctness —
//     Postgres stays the API's only hard dependency.
//   - Per-key before global: a client that has exhausted its own bucket is
//     rejected without touching the global bucket, so one abusive client's
//     429 storm cannot drain the fleet's shared budget. The accepted cost:
//     when the *global* bucket denies, the per-key token already spent is
//     not refunded.
//   - X-RateLimit-Reset is derived client-side from Remaining and the
//     configured refill rate (≤ 1 token of imprecision), keeping 6.3's
//     library reply untouched.

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"time"

	"github.com/mathcslearner/agentloom/internal/obs/log"
	"github.com/mathcslearner/agentloom/internal/ratelimit"
)

// routeClass groups routes for rate limiting (ADR-007): every /v1 route
// belongs to exactly one class, pinned by the route→class coverage test in
// ratelimit_routes_test.go. Class names appear in bucket keys, so renaming
// one silently resets in-flight budgets — treat them as contract.
type routeClass string

const (
	classSubmit routeClass = "submit"
	classRead   routeClass = "read"
	classAdmin  routeClass = "admin"
)

// globalBucketID is the suffix of the API-wide safety bucket's key. It can
// never collide with a per-key bucket: those keys always carry a
// `:<class>` segment after the key_id.
const globalBucketID = "global"

// Acquirer is the middleware's seam over *ratelimit.Limiter, letting unit
// tests script grant/deny/error sequences without Redis.
type Acquirer interface {
	Acquire(ctx context.Context, b ratelimit.Bucket, cost int64) (ratelimit.Result, error)
}

// ClassLimit is one bucket's parameters: Capacity the burst, RefillPerSec
// the sustained rate. Both must be positive (validated in New).
type ClassLimit struct {
	Capacity     int64
	RefillPerSec float64
}

// RateLimitMetrics is the observability seam stubbed for M7 (roadmap:
// "API request histograms + 429 counters (hooks from 6.4)"). M7 wires a
// Prometheus implementation; until then the no-op stub stands in.
// Implementations must be safe for concurrent use and must not block —
// they sit on the request hot path.
type RateLimitMetrics interface {
	// Decision records one bucket decision; global distinguishes the
	// API-wide safety bucket from a per-key class bucket.
	Decision(class string, global, allowed bool)
	// FailOpen records an acquire that errored and was allowed through.
	FailOpen(class string)
}

// nopRateLimitMetrics is the default RateLimitMetrics until M7.
type nopRateLimitMetrics struct{}

func (nopRateLimitMetrics) Decision(string, bool, bool) {}
func (nopRateLimitMetrics) FailOpen(string)             {}

// RateLimitOptions configures the middleware. The zero value (nil
// Acquirer) disables rate limiting entirely — the dev/unit-test mode;
// cmd/api always passes a live limiter unless configured off.
type RateLimitOptions struct {
	// Acquirer performs bucket acquires; production passes
	// *ratelimit.Limiter. Nil disables the middleware.
	Acquirer Acquirer
	// KeyPrefix prefixes every bucket key: `<prefix>:<key_id>:<class>`
	// per key, `<prefix>:global` for the safety bucket.
	KeyPrefix string
	// Submit, Read, Admin are the per-key_id class buckets; Global is the
	// API-wide safety bucket.
	Submit, Read, Admin ClassLimit
	Global              ClassLimit
	// Metrics receives decision/fail-open observations; nil means the
	// no-op stub (M7 replaces it).
	Metrics RateLimitMetrics
}

// validate rejects a misconfigured enabled options set in New, so a bad
// limit fails boot instead of failing open on every request.
func (o RateLimitOptions) validate() error {
	if o.Acquirer == nil {
		return nil
	}
	if o.KeyPrefix == "" {
		return fmt.Errorf("api: rate limit key prefix must be non-empty")
	}
	for _, c := range []struct {
		name  string
		limit ClassLimit
	}{
		{"submit", o.Submit}, {"read", o.Read}, {"admin", o.Admin}, {"global", o.Global},
	} {
		if c.limit.Capacity <= 0 {
			return fmt.Errorf("api: rate limit %s capacity must be positive, got %d", c.name, c.limit.Capacity)
		}
		if !(c.limit.RefillPerSec > 0) || math.IsInf(c.limit.RefillPerSec, 0) {
			return fmt.Errorf("api: rate limit %s refill rate must be positive and finite, got %v", c.name, c.limit.RefillPerSec)
		}
	}
	return nil
}

// classLimit returns the per-key bucket parameters for a route class. The
// class set is closed and every mounted class is covered, so the zero
// return is unreachable; if it ever fires, the acquire fails validation in
// the library and the request fails open — degraded, never wedged.
func (o RateLimitOptions) classLimit(c routeClass) ClassLimit {
	switch c {
	case classSubmit:
		return o.Submit
	case classRead:
		return o.Read
	case classAdmin:
		return o.Admin
	}
	return ClassLimit{}
}

// rateLimit enforces the route class's per-key bucket and the global
// safety bucket. It must be mounted after requireScope: the bucket key is
// the authenticated key_id (the root credential rides under "root").
func (h *Handler) rateLimit(class routeClass) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if h.rl.Acquirer == nil {
				next.ServeHTTP(w, r)
				return
			}
			ctx := r.Context()
			id, ok := identityFrom(ctx)
			if !ok {
				// Middleware-order bug: rateLimit mounted without (or
				// before) requireScope. Fail open — a wiring mistake must
				// not take the route down — and log loudly.
				log.From(ctx).ErrorContext(ctx, "api: rate limit reached without identity — mount rateLimit after requireScope",
					slog.String("path", r.URL.Path))
				next.ServeHTTP(w, r)
				return
			}

			limit := h.rl.classLimit(class)
			res, err := h.rl.Acquirer.Acquire(ctx, ratelimit.Bucket{
				Key:          h.rl.KeyPrefix + ":" + id.keyID + ":" + string(class),
				Capacity:     limit.Capacity,
				RefillPerSec: limit.RefillPerSec,
			}, 1)
			if err != nil {
				h.failOpen(ctx, class, false, err)
				next.ServeHTTP(w, r)
				return
			}
			// The caller's own quota state goes on every response, allowed
			// or denied, so clients can pace themselves before hitting 429.
			setRateLimitHeaders(w, limit, res.Remaining)
			h.rl.Metrics.Decision(string(class), false, res.Allowed)
			if !res.Allowed {
				h.deny(ctx, w, class, false, res.RetryAfter)
				return
			}

			gres, err := h.rl.Acquirer.Acquire(ctx, ratelimit.Bucket{
				Key:          h.rl.KeyPrefix + ":" + globalBucketID,
				Capacity:     h.rl.Global.Capacity,
				RefillPerSec: h.rl.Global.RefillPerSec,
			}, 1)
			if err != nil {
				h.failOpen(ctx, class, true, err)
				next.ServeHTTP(w, r)
				return
			}
			h.rl.Metrics.Decision(string(class), true, gres.Allowed)
			if !gres.Allowed {
				h.deny(ctx, w, class, true, gres.RetryAfter)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// deny answers 429 for one bucket denial. Retry-After comes from the
// bucket that denied; the X-RateLimit-* headers (already set) keep
// describing the caller's class bucket either way. The library does not
// log denials by design — this is the caller-side deny logging 6.3
// deferred here (the request logger already carries key_id).
func (h *Handler) deny(ctx context.Context, w http.ResponseWriter, class routeClass, global bool, retryAfter time.Duration) {
	log.From(ctx).InfoContext(ctx, "api: request rate limited",
		slog.String("class", string(class)),
		slog.Bool("global", global),
		slog.Duration("retry_after", retryAfter))
	w.Header().Set("Retry-After", strconv.FormatInt(retryAfterSeconds(retryAfter), 10))
	msg := fmt.Sprintf("rate limit exceeded for the %q route class", class)
	if global {
		msg = "API-wide rate limit exceeded"
	}
	writeError(w, http.StatusTooManyRequests, ErrorDetail{Code: ErrCodeRateLimited, Message: msg})
}

// failOpen logs an acquire failure and lets the request through (ADR-007:
// rate limits are protective — a Redis outage must not take the API down).
func (h *Handler) failOpen(ctx context.Context, class routeClass, global bool, err error) {
	h.rl.Metrics.FailOpen(string(class))
	log.From(ctx).ErrorContext(ctx, "api: rate limit acquire failed — failing open",
		slog.String("class", string(class)),
		slog.Bool("global", global),
		slog.Any("error", err))
}

// setRateLimitHeaders writes the caller's class-bucket state.
func setRateLimitHeaders(w http.ResponseWriter, limit ClassLimit, remaining int64) {
	w.Header().Set("X-RateLimit-Limit", strconv.FormatInt(limit.Capacity, 10))
	w.Header().Set("X-RateLimit-Remaining", strconv.FormatInt(remaining, 10))
	w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(resetSeconds(limit, remaining), 10))
}

// resetSeconds derives X-RateLimit-Reset — whole seconds until the bucket
// is full again — from the integer Remaining and the configured refill
// rate. Remaining floors the exact balance, so this over-reports by at
// most one token's refill time; good enough for a pacing hint, and it
// keeps the limiter library's reply untouched (the 6.3 deferred decision).
func resetSeconds(limit ClassLimit, remaining int64) int64 {
	missing := limit.Capacity - remaining
	if missing <= 0 {
		return 0
	}
	return int64(math.Ceil(float64(missing) / limit.RefillPerSec))
}

// retryAfterSeconds converts the limiter's precise duration into the
// Retry-After header's whole seconds, rounding up so a client honoring the
// header never retries early. Config validation guarantees refill > 0, so
// RetryAfterNever cannot occur; it is clamped defensively anyway.
func retryAfterSeconds(d time.Duration) int64 {
	if d <= 0 {
		return 1
	}
	return int64(math.Ceil(d.Seconds()))
}
