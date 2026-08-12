package config

import "time"

// Environment variables read by WorkerConfig.
const (
	EnvWorkerHealthInterval        = "AGENTLOOM_WORKER_HEALTH_INTERVAL"
	EnvWorkerDispatchInterval      = "AGENTLOOM_WORKER_DISPATCH_INTERVAL"
	EnvWorkerDispatchBatch         = "AGENTLOOM_WORKER_DISPATCH_BATCH"
	EnvWorkerReconcileInterval     = "AGENTLOOM_WORKER_RECONCILE_INTERVAL"
	EnvWorkerReconcileReadyStale   = "AGENTLOOM_WORKER_RECONCILE_READY_STALE"
	EnvWorkerReconcileRunningStale = "AGENTLOOM_WORKER_RECONCILE_RUNNING_STALE"
	EnvWorkerReconcileRetryStale   = "AGENTLOOM_WORKER_RECONCILE_RETRY_STALE"
	EnvWorkerReconcileLimit        = "AGENTLOOM_WORKER_RECONCILE_LIMIT"
	EnvWorkerEffectsStrict         = "AGENTLOOM_EFFECTS_STRICT"
)

// DefaultWorkerHealthInterval spaces the worker's periodic health log —
// a liveness line with queue depth and PEL size. A minute keeps the log
// quiet while making a wedged worker visible within one scrape-ish
// interval; M7 replaces the numbers with real metrics.
const DefaultWorkerHealthInterval = time.Minute

// Dispatcher and reconciler defaults (ticket 4.4). The relationships that
// matter: ReconcileReadyStale ≫ DispatchInterval (a healthy ready step
// must have been drained long before it looks lost) and
// ReconcileRunningStale ≫ the lease TTL (updated_at moves on transitions,
// not heartbeats — ADR-005).
const (
	// DefaultWorkerDispatchInterval is the outbox polling cadence — the
	// latency backstop; same-process completions arrive via nudge instead.
	DefaultWorkerDispatchInterval = time.Second
	// DefaultWorkerDispatchBatch bounds rows locked per drain transaction.
	DefaultWorkerDispatchBatch = 64
	// DefaultWorkerReconcileInterval spaces sweeps (jittered ±20% by the
	// reconciler); the advisory lock already serializes the fleet.
	DefaultWorkerReconcileInterval = 30 * time.Second
	// DefaultWorkerReconcileReadyStale is 60× the dispatch interval.
	DefaultWorkerReconcileReadyStale = time.Minute
	// DefaultWorkerReconcileRunningStale is 10× the default lease TTL.
	DefaultWorkerReconcileRunningStale = 5 * time.Minute
	// DefaultWorkerReconcileRetryStale is 60× the default promoter tick —
	// measured from a retrying step's next_attempt_at, so no lease-TTL
	// margin applies (ticket 5.2).
	DefaultWorkerReconcileRetryStale = time.Minute
	// DefaultWorkerReconcileLimit caps rows per scan per sweep.
	DefaultWorkerReconcileLimit = 256
)

// WorkerConfig configures the worker deployable (cmd/worker) beyond what
// the shared component configs already carry: Postgres via
// PostgresConfig, Redis via RedisConfig, queue tuning via QueueConfig.
type WorkerConfig struct {
	// HealthInterval is the period between health log lines (worker
	// liveness + queue stats). Must be positive.
	HealthInterval time.Duration
	// DispatchInterval is the outbox drain loop's polling cadence — the
	// dispatch-latency backstop when no completion nudge arrives. Must be
	// positive.
	DispatchInterval time.Duration
	// DispatchBatch is the maximum outbox rows locked and dispatched per
	// drain transaction. Must be positive.
	DispatchBatch int
	// ReconcileInterval is the base period between reconciler sweeps.
	// Must be positive.
	ReconcileInterval time.Duration
	// ReconcileReadyStale is how long a step may sit in ready before the
	// reconciler presumes its dispatch lost and re-outboxes it. Must
	// comfortably exceed DispatchInterval. Must be positive.
	ReconcileReadyStale time.Duration
	// ReconcileRunningStale is how long a step may sit in running before
	// the reconciler presumes its holder dead and takes the step over.
	// Must be ≫ the lease TTL — and, in effect, it is a hard cap on a
	// step's wall-clock execution time: a live executor still running
	// past it gets taken over and its side effects run twice (state stays
	// correct; the fenced completion is rejected). Since M5.3 the per-step
	// `timeout` is the intended execution bound — size this above the
	// largest configured step timeout, or a timeout will never fire before
	// the takeover; it remains the backstop for executors that ignore
	// cancellation. Must be positive.
	ReconcileRunningStale time.Duration
	// ReconcileRetryStale is how long past its next_attempt_at a retrying
	// step may sit before the reconciler presumes its delayed re-dispatch
	// lost and re-outboxes it (ticket 5.2, ADR-006's failure-commit/
	// delayed-schedule crash gap). Measured from the due time; must
	// comfortably exceed the promoter tick. Must be positive.
	ReconcileRetryStale time.Duration
	// ReconcileLimit caps each reconciler scan's rows per sweep. Must be
	// positive.
	ReconcileLimit int
	// EffectsStrict makes side-effect journal misuse panic instead of
	// dead-lettering the step (ticket 5.5) — the loud dev/test behavior.
	// Default true while every deployment is dev; production deployments
	// set AGENTLOOM_EFFECTS_STRICT=false for the clean dead-letter path.
	EffectsStrict bool
}

func defaultWorkerConfig() WorkerConfig {
	return WorkerConfig{
		HealthInterval:        DefaultWorkerHealthInterval,
		DispatchInterval:      DefaultWorkerDispatchInterval,
		DispatchBatch:         DefaultWorkerDispatchBatch,
		ReconcileInterval:     DefaultWorkerReconcileInterval,
		ReconcileReadyStale:   DefaultWorkerReconcileReadyStale,
		ReconcileRunningStale: DefaultWorkerReconcileRunningStale,
		ReconcileRetryStale:   DefaultWorkerReconcileRetryStale,
		ReconcileLimit:        DefaultWorkerReconcileLimit,
		EffectsStrict:         true,
	}
}

// applyEnv overrides fields from the environment, returning one error per
// invalid variable.
func (c *WorkerConfig) applyEnv(fn LookupFunc) []error {
	var errs []error
	errs = applyPositiveDuration(errs, fn, EnvWorkerHealthInterval, &c.HealthInterval)
	errs = applyPositiveDuration(errs, fn, EnvWorkerDispatchInterval, &c.DispatchInterval)
	errs = applyPositiveInt(errs, fn, EnvWorkerDispatchBatch, &c.DispatchBatch)
	errs = applyPositiveDuration(errs, fn, EnvWorkerReconcileInterval, &c.ReconcileInterval)
	errs = applyPositiveDuration(errs, fn, EnvWorkerReconcileReadyStale, &c.ReconcileReadyStale)
	errs = applyPositiveDuration(errs, fn, EnvWorkerReconcileRunningStale, &c.ReconcileRunningStale)
	errs = applyPositiveDuration(errs, fn, EnvWorkerReconcileRetryStale, &c.ReconcileRetryStale)
	errs = applyPositiveInt(errs, fn, EnvWorkerReconcileLimit, &c.ReconcileLimit)
	errs = applyBool(errs, fn, EnvWorkerEffectsStrict, &c.EffectsStrict)
	return errs
}
