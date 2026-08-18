package metrics

// Ticket 7.2's core engine instruments. Every metric in the system is
// declared in this file — names, labels, and buckets in one place — so
// ADR-008's cardinality audit is a single-file read, enforced by the
// conformance test in instruments_test.go. The recording surfaces are
// plain methods: WorkerMetrics structurally satisfies the narrow metrics
// interfaces the producing packages declare (queue.ConsumerMetrics,
// engine.Metrics) and APIMetrics the API's (api.RequestMetrics,
// api.RateLimitMetrics), keeping the prometheus dependency confined to
// this package while cmd/worker and cmd/api wire the concrete structs.
//
// Label vocabularies are ADR-008's allowlist: step_type, outcome, status,
// reason, class, source, route, method, code, result, bucket, decision,
// resource, plugin, limit, action, trigger. run_id/step_id/attempt/
// claim_id/worker_id/key_id are log and trace fields, never labels.

import (
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// Histogram buckets, shared by kind of measurement rather than per metric
// so dashboards can overlay them.
var (
	// latencyBuckets covers queue/dispatch/scheduling paths: sub-ms up to
	// the reconciler-heal scale (a healed dispatch is many seconds late).
	latencyBuckets = []float64{.001, .005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 15, 60}
	// stepBuckets covers executor invocations: fast built-ins up to
	// long-running LLM/tool calls (M8+).
	stepBuckets = []float64{.005, .02, .05, .1, .25, .5, 1, 2.5, 5, 10, 30, 60, 120}
	// runBuckets covers whole-run completion latency: sub-second chains up
	// to multi-hour workflows.
	runBuckets = []float64{.1, .5, 1, 2.5, 5, 15, 30, 60, 300, 900, 3600, 7200}
	// estimateErrorBuckets covers the signed token-cost estimate error
	// (actual − estimate, ticket 9.3): roughly log-scaled and symmetric about
	// zero, so both over-estimates (negative — refunds) and under-estimates
	// (positive — extra debits) are visible and a systematic bias in the
	// estimator shows as a skewed distribution.
	estimateErrorBuckets = []float64{-65536, -16384, -4096, -1024, -256, -64, 0, 64, 256, 1024, 4096, 16384, 65536}
	// semanticDepthBuckets covers the semantic-retry depth (ticket 11.6): the
	// number of feedback-augmented attempts a validated llm step took to reach
	// a terminal verdict. Bounded by dag.MaxSemanticAttempts (10), so one
	// linear bucket per attempt makes the whole distribution legible.
	semanticDepthBuckets = prometheus.LinearBuckets(1, 1, 10)
	// judgeScoreBuckets covers the llm_judge quality score (ticket 11.6): a
	// [0,1] ratio in ten linear buckets, so the score distribution and the
	// pass/fail threshold band are both visible.
	judgeScoreBuckets = prometheus.LinearBuckets(0.1, 0.1, 10)
	// contextUtilizationBuckets covers the provider-window utilization (ticket
	// 12.6): (preflight + max_tokens) / context_window. Mostly [0,1] with a few
	// buckets above 1.0 for the (guarded, rejected) overflow tail, so both the
	// healthy distribution and how close it runs to the window are legible.
	contextUtilizationBuckets = []float64{0.1, 0.25, 0.5, 0.7, 0.8, 0.9, 0.95, 1.0, 1.1, 1.5}
)

// Claim results — the `result` label vocabulary (ADR-008), mirroring the
// engine's ACK-discipline actions.
const (
	ClaimResultWon       = "won"
	ClaimResultAckDrop   = "ack_drop"
	ClaimResultRedeliver = "redeliver"
	ClaimResultTakeover  = "takeover"
)

// WorkerMetrics is the worker deployable's instrument set: queue duties,
// outbox dispatch, reconcile heals, step execution, and run completion.
// Build one per process with NewWorkerMetrics and thread it to the
// consumer, engine, dispatcher, reconciler, and the cmd/worker sampler.
// All methods are safe for concurrent use (prometheus instruments are).
type WorkerMetrics struct {
	queueReadyDepth   prometheus.Gauge
	queueStreamLength prometheus.Gauge
	queuePELSize      prometheus.Gauge
	queueDelayedDepth prometheus.Gauge
	queueReclaimed    prometheus.Counter
	queuePoison       prometheus.Counter
	queuePromoted     prometheus.Counter
	queuePromoteLag   prometheus.Histogram

	outboxBacklog   prometheus.Gauge
	outboxOldestAge prometheus.Gauge

	dispatched  *prometheus.CounterVec
	dispatchLag prometheus.Histogram

	reconcileHealed *prometheus.CounterVec

	claims            *prometheus.CounterVec
	stepDuration      *prometheus.HistogramVec
	schedulingLatency prometheus.Histogram
	retries           *prometheus.CounterVec
	takeovers         prometheus.Counter
	fencingRejections prometheus.Counter
	deadLetters       *prometheus.CounterVec

	runDuration *prometheus.HistogramVec

	workerActive prometheus.Gauge

	stepLogCaptured      prometheus.Counter
	stepLogDropped       prometheus.Counter
	stepLogFlushFailures prometheus.Counter

	throttled          *prometheus.CounterVec
	throttleWait       *prometheus.HistogramVec
	rateLimitFailOpens prometheus.Counter
	estimateError      *prometheus.HistogramVec
	reconcileFailures  prometheus.Counter

	cacheHits      *prometheus.CounterVec
	cacheMisses    *prometheus.CounterVec
	cacheBypass    *prometheus.CounterVec
	cacheStores    *prometheus.CounterVec
	cacheFailOpens prometheus.Counter

	costSpent        *prometheus.CounterVec
	costSaved        *prometheus.CounterVec
	costInputTokens  *prometheus.CounterVec
	costOutputTokens *prometheus.CounterVec
	costBudgetExceed *prometheus.CounterVec
	costDowngrades   *prometheus.CounterVec

	validateVerdicts *prometheus.CounterVec
	validatorResults *prometheus.CounterVec
	semanticDepth    *prometheus.HistogramVec
	outputRepairs    *prometheus.CounterVec
	judgeScore       *prometheus.HistogramVec

	contextUtilization *prometheus.HistogramVec
	contextRejections  *prometheus.CounterVec

	approvalPending prometheus.Gauge
}

// NewWorkerMetrics registers the worker instrument set on reg (ADR-008:
// instance-scoped registries, so tests get isolation for free) and
// returns the recording surface.
func NewWorkerMetrics(reg *prometheus.Registry) *WorkerMetrics {
	m := &WorkerMetrics{
		queueReadyDepth: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: Namespace, Subsystem: "queue", Name: "ready_depth",
			Help: "Entries on the ready stream not yet delivered to the group (XINFO GROUPS lag; falls back to XLEN minus PEL when Redis cannot compute lag after trims).",
		}),
		queueStreamLength: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: Namespace, Subsystem: "queue", Name: "stream_length",
			Help: "Total entries on the ready stream (XLEN): undelivered + leased + acked-but-untrimmed.",
		}),
		queuePELSize: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: Namespace, Subsystem: "queue", Name: "pel_size",
			Help: "Delivered-but-unacked entries in the group PEL — the in-flight lease count.",
		}),
		queueDelayedDepth: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: Namespace, Subsystem: "queue", Name: "delayed_depth",
			Help: "Members waiting in the delayed-delivery sorted set (scheduled retries).",
		}),
		queueReclaimed: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: Namespace, Subsystem: "queue", Name: "reclaimed_total",
			Help: "Expired-lease entries reclaimed via XAUTOCLAIM.",
		}),
		queuePoison: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: Namespace, Subsystem: "queue", Name: "poison_total",
			Help: "Entries diverted to the poison handler after exceeding the delivery-count threshold.",
		}),
		queuePromoted: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: Namespace, Subsystem: "queue", Name: "promoted_total",
			Help: "Delayed members promoted onto the ready stream.",
		}),
		queuePromoteLag: prometheus.NewHistogram(prometheus.HistogramOpts{
			Namespace: Namespace, Subsystem: "queue", Name: "promote_lag_seconds",
			Help:    "Worst promotion lag (now minus fire-at) per promotion pass.",
			Buckets: latencyBuckets,
		}),
		outboxBacklog: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: Namespace, Subsystem: "outbox", Name: "backlog",
			Help: "Pending task_outbox rows awaiting dispatch to the stream.",
		}),
		outboxOldestAge: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: Namespace, Subsystem: "outbox", Name: "oldest_age_seconds",
			Help: "Age of the oldest pending task_outbox row; 0 when the outbox is empty.",
		}),
		dispatched: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: Namespace, Subsystem: "dispatch", Name: "dispatched_total",
			Help: "Outbox rows dispatched (XADDed) to the ready stream, by enqueue reason.",
		}, []string{"reason"}),
		dispatchLag: prometheus.NewHistogram(prometheus.HistogramOpts{
			Namespace: Namespace, Subsystem: "dispatch", Name: "lag_seconds",
			Help:    "Outbox drain lag: row age (dispatch time minus row created_at) at XADD.",
			Buckets: latencyBuckets,
		}),
		reconcileHealed: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: Namespace, Subsystem: "reconcile", Name: "healed_total",
			Help: "Crash gaps healed by reconciler sweeps, by outbox reason (reconcile_ready, reconcile_running, reconcile_retry).",
		}, []string{"reason"}),
		claims: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: Namespace, Subsystem: "step", Name: "claims_total",
			Help: "Claim decisions per delivery: won, ack_drop, redeliver, takeover.",
		}, []string{"result"}),
		stepDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: Namespace, Subsystem: "step", Name: "duration_seconds",
			Help:    "Executor invocation duration by step type and outcome.",
			Buckets: stepBuckets,
		}, []string{"step_type", "outcome"}),
		schedulingLatency: prometheus.NewHistogram(prometheus.HistogramOpts{
			Namespace: Namespace, Subsystem: "step", Name: "scheduling_latency_seconds",
			Help:    "Ready-to-running latency: time from a step turning ready to a worker's claim CAS winning.",
			Buckets: latencyBuckets,
		}),
		retries: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: Namespace, Subsystem: "step", Name: "retries_total",
			Help: "Retry routings (running to retrying) by failure class.",
		}, []string{"class"}),
		takeovers: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: Namespace, Subsystem: "step", Name: "takeovers_total",
			Help: "Lease-expiry takeovers: a silent holder's running step reclaimed (worker path and reconciler heals).",
		}),
		fencingRejections: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: Namespace, Subsystem: "step", Name: "fencing_rejections_total",
			Help: "Zombie writes rejected by claim_id fencing: abandoned completions and stale takeovers.",
		}),
		deadLetters: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: Namespace, Subsystem: "step", Name: "dead_letters_total",
			Help: "Steps dead-lettered, by source (retries_exhausted, permanent, poison).",
		}, []string{"source"}),
		runDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: Namespace, Subsystem: "run", Name: "duration_seconds",
			Help:    "Run completion latency (terminal transition minus run started_at) by terminal status.",
			Buckets: runBuckets,
		}, []string{"status"}),
		workerActive: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: Namespace, Subsystem: "worker", Name: "active",
			Help: "Consumer-group members recently active (idle below the activity threshold) — the fleet-wide worker count as this worker observes it.",
		}),
		stepLogCaptured: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: Namespace, Subsystem: "steplog", Name: "captured_total",
			Help: "Executor log lines accepted into the step-log capture queue (ticket 7.4).",
		}),
		stepLogDropped: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: Namespace, Subsystem: "steplog", Name: "dropped_total",
			Help: "Captured lines lost before storage: queue overflow (drop-oldest) or a failed flush abandoning its batch. Cap-evicted ring lines are not drops — they were stored, then rotated out.",
		}),
		stepLogFlushFailures: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: Namespace, Subsystem: "steplog", Name: "flush_failures_total",
			Help: "Step-log flush transactions that failed and dropped their batch.",
		}),
		throttled: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: Namespace, Subsystem: "ratelimit", Name: "throttled_total",
			Help: "Steps deferred by fleet-wide rate limiting (ticket 9.2, ADR-010), by resource and the denying bucket (requests, tokens, both).",
		}, []string{"resource", "bucket"}),
		throttleWait: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: Namespace, Subsystem: "ratelimit", Name: "throttle_wait_seconds",
			Help:    "Computed re-dispatch delay of a throttled step (the queue-wait rate limiting adds), by resource.",
			Buckets: latencyBuckets,
		}, []string{"resource"}),
		rateLimitFailOpens: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: Namespace, Subsystem: "ratelimit", Name: "fail_opens_total",
			Help: "Resource-limiter errors (e.g. Redis unreachable) after which the step proceeded without a limit (ADR-010 fail-open).",
		}),
		estimateError: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: Namespace, Subsystem: "ratelimit", Name: "estimate_error_tokens",
			Help:    "Signed token-cost estimate error (actual minus estimate) reconciled onto the token bucket (ticket 9.3), by resource. Negative = over-estimate (refund), positive = under-estimate (extra debit).",
			Buckets: estimateErrorBuckets,
		}, []string{"resource"}),
		reconcileFailures: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: Namespace, Subsystem: "ratelimit", Name: "reconcile_failures_total",
			Help: "Token-cost reconciliations that could not be applied (e.g. Redis unreachable); the estimate stays debited and the step proceeds (ADR-010 fail-open).",
		}),
		cacheHits: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: Namespace, Subsystem: "cache", Name: "hits_total",
			Help: "Response-cache hits (ticket 9.5, ADR-011): a step served from cache, skipping the limiter and provider, by plugin (<kind>:<name>).",
		}, []string{"plugin"}),
		cacheMisses: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: Namespace, Subsystem: "cache", Name: "misses_total",
			Help: "Response-cache misses: the read found no entry, by plugin.",
		}, []string{"plugin"}),
		cacheBypass: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: Namespace, Subsystem: "cache", Name: "bypass_total",
			Help: "Steps not consulted against the cache by policy (ineligible plugin, non-deterministic default, mode off, unbuildable key, or an oversized value skipped on write), by plugin.",
		}, []string{"plugin"}),
		cacheStores: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: Namespace, Subsystem: "cache", Name: "stores_total",
			Help: "Write-throughs: a miss's result stored in the cache, by plugin.",
		}, []string{"plugin"}),
		cacheFailOpens: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: Namespace, Subsystem: "cache", Name: "fail_opens_total",
			Help: "Cache store errors (read or write, e.g. Redis unreachable) after which the step proceeded uncached (ADR-011 fail-open).",
		}),
		costSpent: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: Namespace, Subsystem: "cost", Name: "spent_usd_total",
			Help: "Attributed spend in USD (ticket 10.5, ADR-012), by resolved resource (the model or tool:<name>). Recorded post-commit from the cost ledger, so it counts only charges that landed.",
		}, []string{"resource"}),
		costSaved: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: Namespace, Subsystem: "cost", Name: "saved_usd_total",
			Help: "Cache-hit counterfactual savings in USD (ADR-011 rule 2): the money a response-cache hit avoided spending, by resource.",
		}, []string{"resource"}),
		costInputTokens: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: Namespace, Subsystem: "cost", Name: "input_tokens_total",
			Help: "Input tokens billed by productive attempts (ticket 10.5), by resource. Cache hits consume none and are not counted here.",
		}, []string{"resource"}),
		costOutputTokens: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: Namespace, Subsystem: "cost", Name: "output_tokens_total",
			Help: "Output tokens billed by productive attempts (ticket 10.5), by resource.",
		}, []string{"resource"}),
		costBudgetExceed: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: Namespace, Subsystem: "cost", Name: "budget_exceeded_total",
			Help: "Claims the claim-time budget check terminated (ticket 10.5, ADR-012), by limit crossed (run/step_usd/step_tokens) and action (park/fail). Under fan-out each released sibling counts once; only the first parks the run.",
		}, []string{"limit", "action"}),
		costDowngrades: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: Namespace, Subsystem: "cost", Name: "downgrades_total",
			Help: "Claims routed to a cheaper model in a model_fallbacks chain (ticket 10.5, ADR-012), by trigger (budget_threshold/budget_projection).",
		}, []string{"trigger"}),
		validateVerdicts: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: Namespace, Subsystem: "validate", Name: "verdicts_total",
			Help: "Output-validation chain verdicts (ticket 11.6, ADR-013), by step type, the resolved resource that produced the output (model or \"none\"), and status (pass/fail). Recorded post-commit — one per validation run (a miss's validate stage or a cache-hit re-validation).",
		}, []string{"step_type", "resource", "status"}),
		validatorResults: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: Namespace, Subsystem: "validate", Name: "validator_results_total",
			Help: "Per-validator contributions to chain verdicts (ticket 11.6), by validator name and status (pass/fail/skipped/error). One per configured validator per verdict.",
		}, []string{"validator", "status"}),
		semanticDepth: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: Namespace, Subsystem: "validate", Name: "semantic_depth_attempts",
			Help:    "Semantic-retry loop depth (ticket 11.6, ADR-013): the number of semantic attempts a validated step took to terminate, by terminal outcome (succeeded/validation_failed). Recorded once per loop end, never on an intermediate re-attempt.",
			Buckets: semanticDepthBuckets,
		}, []string{"outcome"}),
		outputRepairs: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: Namespace, Subsystem: "validate", Name: "repairs_total",
			Help: "Structured-output shaping results (ticket 11.6), by provenance status (native/raw/repaired/unrepairable). One per productive llm attempt declaring an output_format; cache hits excluded.",
		}, []string{"status"}),
		judgeScore: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: Namespace, Subsystem: "validate", Name: "judge_score_ratio",
			Help:    "LLM-judge quality score distribution (ticket 11.6, ADR-013): the cost-bearing validator's [0,1] score, by validator name.",
			Buckets: judgeScoreBuckets,
		}, []string{"validator"}),
		contextUtilization: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: Namespace, Subsystem: "context", Name: "utilization_ratio",
			Help:    "Provider-window utilization (ticket 12.6, ADR-014): (preflight_tokens + max_tokens) / context_window for each guarded llm claim, by resolved resource. Recorded pre-call; a compaction-fit request stays below 1.0.",
			Buckets: contextUtilizationBuckets,
		}, []string{"resource"}),
		contextRejections: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: Namespace, Subsystem: "context", Name: "window_rejections_total",
			Help: "Claims the provider-window guardrail failed before any provider call (ticket 12.6): assembled context + max_tokens exceeded the model context window and compaction was absent or insufficient, by resolved resource.",
		}, []string{"resource"}),
		approvalPending: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: Namespace, Subsystem: "approval", Name: "pending",
			Help: "Human-approval steps currently parked awaiting a decision (ticket 15.2, ADR-017), fleet-wide as this worker's periodic sample observed it.",
		}),
	}
	reg.MustRegister(
		m.queueReadyDepth, m.queueStreamLength, m.queuePELSize, m.queueDelayedDepth,
		m.queueReclaimed, m.queuePoison, m.queuePromoted, m.queuePromoteLag,
		m.outboxBacklog, m.outboxOldestAge,
		m.dispatched, m.dispatchLag,
		m.reconcileHealed,
		m.claims, m.stepDuration, m.schedulingLatency, m.retries,
		m.takeovers, m.fencingRejections, m.deadLetters,
		m.runDuration,
		m.workerActive,
		m.stepLogCaptured, m.stepLogDropped, m.stepLogFlushFailures,
		m.throttled, m.throttleWait, m.rateLimitFailOpens,
		m.estimateError, m.reconcileFailures,
		m.cacheHits, m.cacheMisses, m.cacheBypass, m.cacheStores, m.cacheFailOpens,
		m.costSpent, m.costSaved, m.costInputTokens, m.costOutputTokens,
		m.costBudgetExceed, m.costDowngrades,
		m.validateVerdicts, m.validatorResults, m.semanticDepth,
		m.outputRepairs, m.judgeScore,
		m.contextUtilization, m.contextRejections,
		m.approvalPending,
	)
	return m
}

// Reclaimed, PoisonDiverted, and Promoted satisfy queue.ConsumerMetrics.

// Reclaimed records n entries reclaimed by one XAUTOCLAIM pass.
func (m *WorkerMetrics) Reclaimed(n int) { m.queueReclaimed.Add(float64(n)) }

// PoisonDiverted records one entry handed to the poison handler.
func (m *WorkerMetrics) PoisonDiverted() { m.queuePoison.Inc() }

// Promoted records one promotion pass: n members moved to the ready
// stream, maxLag the worst now−fireAt among them.
func (m *WorkerMetrics) Promoted(n int, maxLag time.Duration) {
	m.queuePromoted.Add(float64(n))
	m.queuePromoteLag.Observe(maxLag.Seconds())
}

// The methods below satisfy engine.Metrics.

// ClaimDecision records one delivery's claim decision.
func (m *WorkerMetrics) ClaimDecision(result string) { m.claims.WithLabelValues(result).Inc() }

// SchedulingLatency records one ready→running observation.
func (m *WorkerMetrics) SchedulingLatency(d time.Duration) {
	m.schedulingLatency.Observe(d.Seconds())
}

// StepDuration records one executor invocation.
func (m *WorkerMetrics) StepDuration(stepType, outcome string, d time.Duration) {
	m.stepDuration.WithLabelValues(stepType, outcome).Observe(d.Seconds())
}

// RetryScheduled records one retry routing.
func (m *WorkerMetrics) RetryScheduled(class string) { m.retries.WithLabelValues(class).Inc() }

// Takeover records one lease-expiry takeover.
func (m *WorkerMetrics) Takeover() { m.takeovers.Inc() }

// FencingRejection records one fenced (rejected) zombie write.
func (m *WorkerMetrics) FencingRejection() { m.fencingRejections.Inc() }

// DeadLetter records one dead-lettered step.
func (m *WorkerMetrics) DeadLetter(source string) { m.deadLetters.WithLabelValues(source).Inc() }

// RunCompleted records one run reaching a terminal status.
func (m *WorkerMetrics) RunCompleted(status string, d time.Duration) {
	m.runDuration.WithLabelValues(status).Observe(d.Seconds())
}

// Dispatched records one outbox row XADDed to the stream.
func (m *WorkerMetrics) Dispatched(reason string, lag time.Duration) {
	m.dispatched.WithLabelValues(reason).Inc()
	m.dispatchLag.Observe(lag.Seconds())
}

// ReconcileHealed records n heals of one reason from a reconciler sweep.
func (m *WorkerMetrics) ReconcileHealed(reason string, n int) {
	m.reconcileHealed.WithLabelValues(reason).Add(float64(n))
}

// Throttled records one rate-limit backpressure event (ticket 9.2).
func (m *WorkerMetrics) Throttled(resource, bucket string) {
	m.throttled.WithLabelValues(resource, bucket).Inc()
}

// ThrottleWait records the computed re-dispatch delay of one throttle.
func (m *WorkerMetrics) ThrottleWait(resource string, d time.Duration) {
	m.throttleWait.WithLabelValues(resource).Observe(d.Seconds())
}

// RateLimitFailOpen records one fail-open (limiter error → proceed).
func (m *WorkerMetrics) RateLimitFailOpen() { m.rateLimitFailOpens.Inc() }

// EstimateError records one post-call token-cost reconciliation (ticket 9.3):
// the signed error actual − estimate corrected on the token bucket, by
// resource.
func (m *WorkerMetrics) EstimateError(resource string, delta int64) {
	m.estimateError.WithLabelValues(resource).Observe(float64(delta))
}

// ReconcileFailure records one reconciliation that could not be applied
// (fail-open — the estimate stays debited and the step proceeds).
func (m *WorkerMetrics) ReconcileFailure() { m.reconcileFailures.Inc() }

// CacheHit records one response-cache hit (ticket 9.5).
func (m *WorkerMetrics) CacheHit(plugin string) { m.cacheHits.WithLabelValues(plugin).Inc() }

// CacheMiss records one response-cache miss.
func (m *WorkerMetrics) CacheMiss(plugin string) { m.cacheMisses.WithLabelValues(plugin).Inc() }

// CacheBypass records one step not consulted against the cache by policy.
func (m *WorkerMetrics) CacheBypass(plugin string) { m.cacheBypass.WithLabelValues(plugin).Inc() }

// CacheStore records one write-through.
func (m *WorkerMetrics) CacheStore(plugin string) { m.cacheStores.WithLabelValues(plugin).Inc() }

// CacheFailOpen records one cache store error after which the step proceeded
// uncached (ADR-011 fail-open).
func (m *WorkerMetrics) CacheFailOpen() { m.cacheFailOpens.Inc() }

// nanoUSDToFloat converts integer nano-USD (the ledger's exact unit) to the
// base USD unit Prometheus counters carry (ADR-008: base units). The ledger
// stays the integer source of truth; a rate counter tolerates float.
func nanoUSDToFloat(nanoUSD int64) float64 { return float64(nanoUSD) / 1e9 }

// CostSpent records one productive attempt's attributed spend (ticket 10.5),
// by resource.
func (m *WorkerMetrics) CostSpent(resource string, nanoUSD int64) {
	m.costSpent.WithLabelValues(resource).Add(nanoUSDToFloat(nanoUSD))
}

// CostSaved records one cache hit's counterfactual savings (ticket 10.5), by
// resource.
func (m *WorkerMetrics) CostSaved(resource string, nanoUSD int64) {
	m.costSaved.WithLabelValues(resource).Add(nanoUSDToFloat(nanoUSD))
}

// CostTokens records one productive attempt's billed input/output tokens by
// resource (ticket 10.5).
func (m *WorkerMetrics) CostTokens(resource string, input, output int64) {
	m.costInputTokens.WithLabelValues(resource).Add(float64(input))
	m.costOutputTokens.WithLabelValues(resource).Add(float64(output))
}

// BudgetExceeded records one claim the budget check terminated (ticket 10.5),
// by the limit crossed and the action taken.
func (m *WorkerMetrics) BudgetExceeded(limit, action string) {
	m.costBudgetExceed.WithLabelValues(limit, action).Inc()
}

// ModelDowngraded records one claim routed to a cheaper model (ticket 10.5),
// by trigger.
func (m *WorkerMetrics) ModelDowngraded(trigger string) {
	m.costDowngrades.WithLabelValues(trigger).Inc()
}

// The methods below are ticket 11.6's output-quality signals (ADR-013).

// ValidationVerdict records one chain verdict by step type, resource, and
// status (pass/fail).
func (m *WorkerMetrics) ValidationVerdict(stepType, resource, status string) {
	m.validateVerdicts.WithLabelValues(stepType, resource, status).Inc()
}

// ValidatorResult records one validator's contribution to a chain verdict by
// validator name and status (pass/fail/skipped/error).
func (m *WorkerMetrics) ValidatorResult(validator, status string) {
	m.validatorResults.WithLabelValues(validator, status).Inc()
}

// SemanticRetryDepth records one terminated semantic-retry loop by outcome and
// the number of semantic attempts it took.
func (m *WorkerMetrics) SemanticRetryDepth(outcome string, attempts int) {
	m.semanticDepth.WithLabelValues(outcome).Observe(float64(attempts))
}

// OutputRepair records one structured-output shaping result by provenance
// status.
func (m *WorkerMetrics) OutputRepair(status string) {
	m.outputRepairs.WithLabelValues(status).Inc()
}

// JudgeScore records one llm_judge quality score by validator name.
func (m *WorkerMetrics) JudgeScore(validator string, score float64) {
	m.judgeScore.WithLabelValues(validator).Observe(score)
}

// ContextUtilization records one guarded claim's provider-window utilization
// ratio by resolved resource (ticket 12.6).
func (m *WorkerMetrics) ContextUtilization(resource string, ratio float64) {
	m.contextUtilization.WithLabelValues(resource).Observe(ratio)
}

// ContextWindowRejection records one claim the provider-window guardrail
// terminated before any provider call, by resolved resource (ticket 12.6).
func (m *WorkerMetrics) ContextWindowRejection(resource string) {
	m.contextRejections.WithLabelValues(resource).Inc()
}

// The setters below are the cmd/worker sampler's surface — point-in-time
// gauges sampled from Redis and Postgres on an interval.

// SetQueueDepths records one queue depth sample.
func (m *WorkerMetrics) SetQueueDepths(ready, length, pel, delayed int64) {
	m.queueReadyDepth.Set(float64(ready))
	m.queueStreamLength.Set(float64(length))
	m.queuePELSize.Set(float64(pel))
	m.queueDelayedDepth.Set(float64(delayed))
}

// SetOutbox records one outbox backlog sample.
func (m *WorkerMetrics) SetOutbox(backlog int64, oldestAge time.Duration) {
	m.outboxBacklog.Set(float64(backlog))
	m.outboxOldestAge.Set(oldestAge.Seconds())
}

// SetActiveWorkers records one active-consumer sample.
func (m *WorkerMetrics) SetActiveWorkers(n int) { m.workerActive.Set(float64(n)) }

// SetApprovalPending updates the parked-approval gauge (ticket 15.2) from
// the worker's periodic sample of the store's pending-approval count.
func (m *WorkerMetrics) SetApprovalPending(n int64) { m.approvalPending.Set(float64(n)) }

// The methods below satisfy steplog.Metrics (ticket 7.4).

// StepLogCaptured records n lines accepted into the capture queue.
func (m *WorkerMetrics) StepLogCaptured(n int) { m.stepLogCaptured.Add(float64(n)) }

// StepLogDropped records n captured lines lost before storage.
func (m *WorkerMetrics) StepLogDropped(n int) { m.stepLogDropped.Add(float64(n)) }

// StepLogFlushFailure records one failed flush transaction.
func (m *WorkerMetrics) StepLogFlushFailure() { m.stepLogFlushFailures.Inc() }

// APIMetrics is the API deployable's instrument set: request histograms
// and the rate-limit decision counters (the 6.4 RateLimitMetrics seam,
// wired here per ADR-008). All methods are safe for concurrent use.
type APIMetrics struct {
	requests         *prometheus.CounterVec
	requestDuration  *prometheus.HistogramVec
	requestsInFlight prometheus.Gauge
	rlDecisions      *prometheus.CounterVec
	rlFailOpen       *prometheus.CounterVec
}

// NewAPIMetrics registers the API instrument set on reg and returns the
// recording surface.
func NewAPIMetrics(reg *prometheus.Registry) *APIMetrics {
	m := &APIMetrics{
		requests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: Namespace, Subsystem: "api", Name: "requests_total",
			Help: "HTTP requests served, by route pattern, method, and status code.",
		}, []string{"route", "method", "code"}),
		requestDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: Namespace, Subsystem: "api", Name: "request_duration_seconds",
			Help: "HTTP request duration by route pattern and method (status deliberately excluded — see ADR-008's series budget).",
			// Requests are Postgres round-trips; the default web buckets fit.
			Buckets: prometheus.DefBuckets,
		}, []string{"route", "method"}),
		requestsInFlight: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: Namespace, Subsystem: "api", Name: "requests_in_flight",
			Help: "HTTP requests currently being served (ticket 7.5). Unlabeled — route is only known after routing.",
		}),
		rlDecisions: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: Namespace, Subsystem: "api", Name: "ratelimit_decisions_total",
			Help: "Rate-limit bucket decisions, by route class, bucket kind (per_key or global), and decision (allowed or denied). 429 responses also appear in engine_api_requests_total{code=\"429\"}.",
		}, []string{"class", "bucket", "decision"}),
		rlFailOpen: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: Namespace, Subsystem: "api", Name: "ratelimit_failopen_total",
			Help: "Rate-limit acquires that errored (Redis unavailable) and were allowed through, by route class.",
		}, []string{"class"}),
	}
	reg.MustRegister(m.requests, m.requestDuration, m.requestsInFlight, m.rlDecisions, m.rlFailOpen)
	return m
}

// Request satisfies api.RequestMetrics: one served request. route is the
// chi route pattern, never the raw path (ADR-008); the API layer maps
// unrouted requests to route "unmatched" with a clamped method.
func (m *APIMetrics) Request(route, method string, status int, d time.Duration) {
	code := strconv.Itoa(status)
	m.requests.WithLabelValues(route, method, code).Inc()
	m.requestDuration.WithLabelValues(route, method).Observe(d.Seconds())
}

// RequestStarted satisfies api.RequestMetrics: one request began serving.
func (m *APIMetrics) RequestStarted() { m.requestsInFlight.Inc() }

// RequestFinished satisfies api.RequestMetrics: one request finished.
func (m *APIMetrics) RequestFinished() { m.requestsInFlight.Dec() }

// Decision satisfies api.RateLimitMetrics: one bucket decision.
func (m *APIMetrics) Decision(class string, global, allowed bool) {
	bucket := "per_key"
	if global {
		bucket = "global"
	}
	decision := "allowed"
	if !allowed {
		decision = "denied"
	}
	m.rlDecisions.WithLabelValues(class, bucket, decision).Inc()
}

// FailOpen satisfies api.RateLimitMetrics: one errored acquire allowed
// through.
func (m *APIMetrics) FailOpen(class string) { m.rlFailOpen.WithLabelValues(class).Inc() }
