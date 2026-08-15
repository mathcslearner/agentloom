// Package engine is the worker's execution pipeline (ADR-002): the code
// that turns a queue delivery into durable state transitions against
// Postgres. Ticket 4.2 provides the claim path — a queue.Handler that
// attempts the guarded CAS ready → running and applies ADR-005's ACK
// discipline to every outcome. Ticket 4.3 provides the tail: the executor
// runs and its result settles in one completion transaction (output, the
// claim-fenced terminal CAS, edge resolution, readiness fan-out, outbox
// rows, run rollup), with the ACK after commit. The outbox dispatcher and
// reconciler (4.4) and fencing enforcement (4.5) build on these.
//
// The engine is transport-agnostic on purpose: it knows deliveries and
// the store, never Redis. Which entries redeliver, heartbeat, or expire
// is internal/queue's business; the engine only decides, per delivery,
// whether consuming it succeeded (ACK), is provably unnecessary (ACK and
// drop), or must be retried (no ACK).
package engine

import (
	"context"
	"errors"
	"log/slog"
	"math/rand/v2"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	oteltrace "go.opentelemetry.io/otel/trace"

	"github.com/mathcslearner/agentloom/internal/exec"
	"github.com/mathcslearner/agentloom/internal/exec/effects"
	"github.com/mathcslearner/agentloom/internal/queue"
	"github.com/mathcslearner/agentloom/internal/ratelimit/resource"
	"github.com/mathcslearner/agentloom/internal/store"
)

// Default requeue-math knobs for the throttle backoff (ADR-010): the delay
// before a rate-limited step's re-dispatch is clamp(retry_after, floor,
// cap) plus additive jitter. cmd/worker overrides these from config.
const (
	defaultThrottleFloor      = 500 * time.Millisecond
	defaultThrottleCap        = 5 * time.Minute
	defaultThrottleJitterFrac = 0.20
)

// ResourceLimiter is the engine's seam onto the fleet-wide resource rate
// limiter (ticket 9.2, ADR-010) — satisfied by *resource.Limiter. The
// middleware acquires a cost-bearing step's resource buckets before its
// provider call; a denial defers the step (throttle), an impossible request
// perm-fails, a Redis error fails open. Nil disables limiting entirely.
type ResourceLimiter interface {
	Acquire(ctx context.Context, resourceName string, estTokens int64) (resource.Decision, error)
	// Reconcile corrects a resource's token bucket by actual − estimate after
	// a granted, token-metered call has returned (ticket 9.3, ADR-010): a
	// biased estimator cannot drift the fleet past the provider's real
	// tokens/min budget over time. Called only when Acquire granted and
	// metered tokens; a no-op (and no Redis) for unlimited or requests-only
	// resources. Fail-open, like Acquire.
	Reconcile(ctx context.Context, resourceName string, estTokens, actualTokens int64) error
}

// RetryScheduler is the engine's seam onto delayed delivery (ticket 5.2)
// — satisfied by *queue.Delayed. After a retry-routing completion commits,
// the engine schedules the re-dispatch envelope through it; a failure (or
// a crash before the call) is deliberately survivable, healed by the
// reconciler's overdue-retrying scan, which is also why tests inject a
// failing implementation to provoke that gap.
type RetryScheduler interface {
	Schedule(ctx context.Context, env queue.Envelope, fireAt time.Time) error
}

// StepLogSink is the engine's seam onto per-step log capture (ticket
// 7.4) — satisfied by *steplog.Sink. LoggerFor returns base teed into the
// durable step_logs store for one attempt; implementations must keep the
// returned logger's capture path non-blocking (execution must never wait
// on log persistence).
type StepLogSink interface {
	LoggerFor(base *slog.Logger, runID uuid.UUID, stepID string, attempt int, traceID string) *slog.Logger
}

// Engine executes claimed steps for one worker process. It is safe for
// concurrent Handle calls (every field is read-only after New), though
// v0's worker runs a single serialized consumer loop.
type Engine struct {
	// Control is the run-lifecycle op surface (Cancel/Park/Unpark/Requeue),
	// embedded so engine call sites read e.Cancel(...) while the API server
	// can hold a standalone Control (NewControl) without the rest of the
	// engine. Built by New from the engine's own store, clock, and nudge.
	*Control
	store    *store.Store
	registry *exec.Registry
	// workerID identifies this worker in logs (canonical worker_id field).
	// By convention it is the queue consumer name, so log lines join
	// against XINFO CONSUMERS output during an incident.
	workerID string
	// now is the injected clock stamped onto claim and completion
	// transitions.
	now func() time.Time
	// nudge, when set, is called after a completion transaction commits
	// with newly-ready steps in the outbox — the "nudge the dispatcher"
	// seam. The outbox drain loop (ticket 4.4) wires its wake channel
	// here; nil means no one to nudge (drain cadence alone dispatches).
	nudge func()
	// scheduler, when set, receives the delayed retry envelope after a
	// retry-routing completion commits (ticket 5.2). Nil means no delayed
	// scheduling — retries then re-dispatch only via the reconciler's
	// overdue-retrying heal (degraded latency, same convergence).
	scheduler RetryScheduler
	// jitterRand supplies the [0,1) draw for full-jitter backoff.
	jitterRand func() float64
	// effects is the side-effect journal (ticket 5.5), bound per claimed
	// step and handed to executors through StepContext.Effects. Built in
	// New over the same store and clock the engine uses.
	effects *effects.Journal
	// effectsStrict makes journal misuse panic (dev/test loudness) instead
	// of dead-lettering the step; see config.WorkerConfig.EffectsStrict.
	effectsStrict bool
	// cancelPollInterval is how often the in-flight cancellation watch
	// polls the run's status while an executor runs (ticket 5.6): the
	// latency bound on "in-flight executors get context cancellation".
	// Zero disables the watch — correctness is unaffected (the completion
	// transaction re-checks under the run lock); only the executor then
	// runs to its own end before the cancel settles.
	cancelPollInterval time.Duration
	// stepLogs, when set, tees each attempt's StepContext.Logger into the
	// durable step_logs store (ticket 7.4). Nil means no capture — the
	// default for every test layer that doesn't opt in.
	stepLogs StepLogSink
	// metrics receives execution-pipeline observations (ticket 7.2).
	// Never nil after New — the default is the no-op recorder.
	metrics Metrics
	// tracerProvider supplies the tracer for the pipeline's child spans
	// (ticket 7.3): claim CAS, executor invocation, completion transaction
	// — all children of the consumer's attempt span via the delivery
	// context. Nil until New resolves it to the global provider (the
	// no-op provider unless obs/trace.Setup enabled export).
	tracerProvider oteltrace.TracerProvider
	// tracer is tracerProvider's resolved tracer. Never nil after New.
	tracer oteltrace.Tracer
	// limiter, when set, is the fleet-wide resource rate limiter the M9
	// middleware acquires before a cost-bearing executor's provider call
	// (ticket 9.2, ADR-010). Nil disables limiting — every step executes
	// without an acquire, the default for every test layer that doesn't opt
	// in.
	limiter ResourceLimiter
	// throttleFloor / throttleCap / throttleJitterFrac are ADR-010's requeue
	// math knobs (the delay before a throttled step's re-dispatch). Set to
	// the defaults in New; cmd/worker overrides via WithThrottleBackoff.
	throttleFloor      time.Duration
	throttleCap        time.Duration
	throttleJitterFrac float64
	// cache, when set, is the response-cache store the M9 middleware reads a
	// hit from (ahead of the limiter) and writes a miss through to (ticket
	// 9.5, ADR-011). Nil disables caching — every step executes uncached,
	// the default for every test layer that doesn't opt in.
	cache ResponseCache
	// cacheDefaultTTL is the fleet default entry TTL applied when a cached
	// step opts in without its own `cache.ttl` (ticket 9.5). Only consulted
	// when cache is non-nil.
	cacheDefaultTTL time.Duration
}

// Option customizes an Engine.
type Option func(*Engine)

// WithClock overrides the engine's clock (project invariant: time is
// injectable; tests pass a fixed clock).
func WithClock(now func() time.Time) Option {
	return func(e *Engine) { e.now = now }
}

// WithDispatchNudge sets the function called after a completion
// transaction commits having outboxed newly-ready steps. Ticket 4.4's
// outbox drain loop registers its wake signal here so fan-out latency is
// one nudge, not one poll interval. The function must be safe for
// concurrent calls and must not block.
func WithDispatchNudge(nudge func()) Option {
	return func(e *Engine) { e.nudge = nudge }
}

// WithRetryScheduler sets the delayed-delivery producer retry-routing
// completions schedule their re-dispatch through — wire the queue's
// Delayed handle here (cmd/worker does). Nil is legal: retries still
// commit durably and the reconciler re-dispatches them, just on its sweep
// cadence instead of on time.
func WithRetryScheduler(s RetryScheduler) Option {
	return func(e *Engine) { e.scheduler = s }
}

// WithJitterRand overrides the [0,1) source full-jitter backoff draws
// from (project invariant: nondeterminism is injectable; timing tests pin
// it or use jitter "none").
func WithJitterRand(r func() float64) Option {
	return func(e *Engine) { e.jitterRand = r }
}

// WithStrictEffects sets the side-effect journal's misuse behavior (ticket
// 5.5): strict panics — the loud dev/test mode, riding the consumer's
// panic path into poison dead-lettering — while non-strict dead-letters
// the step cleanly with a permanent-classified error. cmd/worker wires
// config.WorkerConfig.EffectsStrict here.
func WithStrictEffects(strict bool) Option {
	return func(e *Engine) { e.effectsStrict = strict }
}

// WithCancelPollInterval sets how often the in-flight cancellation watch
// polls the run's status during executor invocations (ticket 5.6) —
// cmd/worker wires config.WorkerConfig.CancelPollInterval here. Zero
// disables the watch (executors then run to their own end; the completion
// transaction still settles the cancel).
func WithCancelPollInterval(d time.Duration) Option {
	return func(e *Engine) { e.cancelPollInterval = d }
}

// WithMetrics sets the engine's metrics recorder (ticket 7.2) —
// cmd/worker wires obs/metrics.WorkerMetrics here. Nil restores the
// no-op default.
func WithMetrics(m Metrics) Option {
	return func(e *Engine) {
		if m == nil {
			m = nopMetrics{}
		}
		e.metrics = m
	}
}

// WithStepLogs sets the per-step log capture sink (ticket 7.4) —
// cmd/worker wires *steplog.Sink here. Nil disables capture.
func WithStepLogs(s StepLogSink) Option {
	return func(e *Engine) { e.stepLogs = s }
}

// WithTracerProvider sets the provider for the engine's pipeline spans
// (ticket 7.3). Nil restores the default: the global provider, which is
// the no-op provider unless obs/trace.Setup enabled export — so tests run
// span-free unless they inject a recorder.
func WithTracerProvider(tp oteltrace.TracerProvider) Option {
	return func(e *Engine) { e.tracerProvider = tp }
}

// WithResourceLimiter sets the fleet-wide resource rate limiter the M9
// middleware consults before a cost-bearing executor's provider call
// (ticket 9.2, ADR-010) — cmd/worker wires *resource.Limiter here when the
// resource-limit config names any resource. Nil (the default) disables
// limiting: every step executes with no acquire.
func WithResourceLimiter(l ResourceLimiter) Option {
	return func(e *Engine) { e.limiter = l }
}

// WithThrottleBackoff overrides ADR-010's requeue-math knobs — the floor,
// cap, and jitter fraction of a throttled step's re-dispatch delay.
// cmd/worker wires config.WorkerConfig here. Non-positive floor/cap keep the
// defaults; a negative jitter fraction is clamped to zero (no jitter).
func WithThrottleBackoff(floor, ceiling time.Duration, jitterFrac float64) Option {
	return func(e *Engine) {
		if floor > 0 {
			e.throttleFloor = floor
		}
		if ceiling > 0 {
			e.throttleCap = ceiling
		}
		if jitterFrac < 0 {
			jitterFrac = 0
		}
		e.throttleJitterFrac = jitterFrac
	}
}

// WithResponseCache sets the response-cache store the M9 middleware reads a
// hit from ahead of the limiter and writes a miss through to (ticket 9.5,
// ADR-011) — cmd/worker wires *cache/redisstore.Store here when caching is
// enabled. defaultTTL is the fleet default entry TTL for opted-in steps that
// set no `cache.ttl`; it must be positive when the cache is set. Nil cache
// (the default) disables caching: every step executes uncached.
func WithResponseCache(c ResponseCache, defaultTTL time.Duration) Option {
	return func(e *Engine) {
		e.cache = c
		e.cacheDefaultTTL = defaultTTL
	}
}

// New builds an Engine over the given store and executor registry.
// workerID may be empty (logs then carry an empty worker_id — the queue
// consumer name is the conventional value).
func New(s *store.Store, r *exec.Registry, workerID string, opts ...Option) (*Engine, error) {
	if s == nil {
		return nil, errors.New("engine: New requires a store")
	}
	if r == nil {
		return nil, errors.New("engine: New requires an executor registry")
	}
	e := &Engine{
		store: s, registry: r, workerID: workerID, now: time.Now,
		jitterRand: rand.Float64, metrics: nopMetrics{},
		throttleFloor: defaultThrottleFloor, throttleCap: defaultThrottleCap,
		throttleJitterFrac: defaultThrottleJitterFrac,
	}
	for _, opt := range opts {
		opt(e)
	}
	if e.tracerProvider == nil {
		e.tracerProvider = otel.GetTracerProvider()
	}
	e.tracer = e.tracerProvider.Tracer("agentloom/engine")
	// Built after the options so the journal shares whatever clock the
	// engine ended up with (tests inject fixed clocks through WithClock).
	j, err := effects.New(s, effects.WithClock(func() time.Time { return e.now() }), effects.WithStrict(e.effectsStrict))
	if err != nil {
		return nil, err
	}
	e.effects = j
	// The lifecycle control surface shares the engine's store, clock, and
	// dispatcher nudge (both read-only after New, so copying is safe).
	e.Control = &Control{store: e.store, now: e.now, nudge: e.nudge}
	return e, nil
}
