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
// reason, class, source, route, method, code, result, bucket, decision.
// run_id/step_id/attempt/claim_id/worker_id/key_id are log and trace
// fields, never labels.

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
