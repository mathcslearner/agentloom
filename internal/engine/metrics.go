package engine

import "time"

// Metrics is the engine's observability seam (ticket 7.2, ADR-008): the
// execution pipeline, dispatcher, and reconciler record onto it, and
// cmd/worker wires internal/obs/metrics.WorkerMetrics here, which
// satisfies this interface structurally — the engine stays free of any
// metrics dependency, mirroring the queue's ConsumerMetrics seam.
// Implementations must be safe for concurrent use and must not block.
type Metrics interface {
	// ClaimDecision records one delivery's claim decision: won, ack_drop,
	// redeliver, or takeover (the claimAction vocabulary).
	ClaimDecision(result string)
	// SchedulingLatency records one ready→running observation: the time
	// from a step turning ready to the claim CAS winning. Only recorded
	// for claims whose origin status was ready — a retrying claim's
	// interval includes the deliberate backoff.
	SchedulingLatency(d time.Duration)
	// StepDuration records one executor invocation by step type and
	// outcome (succeeded, transient, permanent, timeout, cancelled).
	StepDuration(stepType, outcome string, d time.Duration)
	// RetryScheduled records one retry routing by failure class.
	RetryScheduled(class string)
	// Takeover records one lease-expiry takeover (worker path or
	// reconciler heal).
	Takeover()
	// FencingRejection records one zombie write rejected by claim_id
	// fencing: an abandoned completion or a stale takeover.
	FencingRejection()
	// DeadLetter records one dead-lettered step by source.
	DeadLetter(source string)
	// RunCompleted records one run reaching a terminal status, with the
	// completion latency (terminal transition minus run started_at — both
	// injected-clock stamps, so the interval is clock-consistent).
	RunCompleted(status string, d time.Duration)
	// Dispatched records one outbox row XADDed to the stream, with its
	// drain lag (dispatch time minus row created_at).
	Dispatched(reason string, lag time.Duration)
	// ReconcileHealed records n heals of one outbox reason from a
	// reconciler sweep.
	ReconcileHealed(reason string, n int)
	// Throttled records one fleet-wide rate-limit backpressure event
	// (ticket 9.2, ADR-010) by resource and the denying bucket
	// (requests/tokens/both).
	Throttled(resource, bucket string)
	// ThrottleWait records the computed re-dispatch delay of one throttle,
	// by resource — the queue-wait a rate-limited step incurs before its
	// next attempt.
	ThrottleWait(resource string, d time.Duration)
	// RateLimitFailOpen records one fail-open: the resource limiter errored
	// (e.g. Redis unreachable) and the step proceeded without a limit
	// (ADR-010 — a limit is never a correctness dependency).
	RateLimitFailOpen()
	// EstimateError records one post-call token-cost reconciliation (ticket
	// 9.3) by resource: the signed error actual − estimate the middleware
	// corrected on the token bucket. A negative observation means the estimate
	// was high (a refund), positive means it was low (an extra debit) — the
	// distribution reveals a biased estimator.
	EstimateError(resource string, delta int64)
	// ReconcileFailure records one reconciliation that could not be applied
	// (e.g. Redis unreachable): the estimate stays on the ledger and the step
	// proceeds — reconciliation is fail-open like the acquire.
	ReconcileFailure()
	// CacheHit records one response-cache hit (ticket 9.5, ADR-011): a step
	// served from cache, skipping the limiter and provider, by plugin
	// (<kind>:<name>, the concrete provider/tool/retriever).
	CacheHit(plugin string)
	// CacheMiss records one response-cache miss: the read found no entry, so
	// the step executes, by plugin.
	CacheMiss(plugin string)
	// CacheBypass records one step not consulted against the cache by policy
	// (ineligible plugin, non-deterministic default, mode off, unbuildable
	// key, or an oversized value skipped on write), by plugin.
	CacheBypass(plugin string)
	// CacheStore records one write-through: a miss's result stored, by plugin.
	CacheStore(plugin string)
	// CacheFailOpen records one cache store error (read or write) after which
	// the step proceeded uncached (ADR-011 fail-open) — unlabeled, like the
	// rate-limiter fail-open.
	CacheFailOpen()
}

// nopMetrics is the default Metrics: every test layer runs with recording
// off unless it opts in.
type nopMetrics struct{}

func (nopMetrics) ClaimDecision(string)                       {}
func (nopMetrics) SchedulingLatency(time.Duration)            {}
func (nopMetrics) StepDuration(string, string, time.Duration) {}
func (nopMetrics) RetryScheduled(string)                      {}
func (nopMetrics) Takeover()                                  {}
func (nopMetrics) FencingRejection()                          {}
func (nopMetrics) DeadLetter(string)                          {}
func (nopMetrics) RunCompleted(string, time.Duration)         {}
func (nopMetrics) Dispatched(string, time.Duration)           {}
func (nopMetrics) ReconcileHealed(string, int)                {}
func (nopMetrics) Throttled(string, string)                   {}
func (nopMetrics) ThrottleWait(string, time.Duration)         {}
func (nopMetrics) RateLimitFailOpen()                         {}
func (nopMetrics) EstimateError(string, int64)                {}
func (nopMetrics) ReconcileFailure()                          {}
func (nopMetrics) CacheHit(string)                            {}
func (nopMetrics) CacheMiss(string)                           {}
func (nopMetrics) CacheBypass(string)                         {}
func (nopMetrics) CacheStore(string)                          {}
func (nopMetrics) CacheFailOpen()                             {}
