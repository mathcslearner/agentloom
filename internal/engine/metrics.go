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
	// CostSpent records one productive attempt's attributed spend (ticket
	// 10.5, ADR-012) in nano-USD, by resolved resource (the model or
	// "tool:<name>" — bounded to the pricing catalog). Recorded post-commit,
	// so it counts only charges that landed in the ledger.
	CostSpent(resource string, nanoUSD int64)
	// CostSaved records one cache hit's counterfactual savings (ADR-011 rule
	// 2) in nano-USD, by resource — the money a hit avoided spending, the
	// saved-by-cache signal.
	CostSaved(resource string, nanoUSD int64)
	// CostTokens records one productive attempt's billed input/output tokens
	// by resource (cache hits consume none, so they are not recorded here).
	CostTokens(resource string, input, output int64)
	// BudgetExceeded records one claim the budget check terminated (ticket
	// 10.5) by the limit crossed (run/step_usd/step_tokens) and the action
	// (park/fail). Under fan-out each released sibling counts once, mirroring
	// the budget_exceeded event count; only the first actually parks the run.
	BudgetExceeded(limit, action string)
	// ModelDowngraded records one claim routed to a cheaper model (ticket
	// 10.5) by trigger (budget_threshold/budget_projection).
	ModelDowngraded(trigger string)
	// ValidationVerdict records one output-validation chain verdict (ticket
	// 11.6, ADR-013) by step type, the resolved resource that produced the
	// output (the model or "none" for a non-cost-bearing step), and the
	// overall status (pass/fail). Recorded post-commit, once per validation
	// that ran — a miss's validate stage or a cache hit's re-validation — so
	// the failure rate reads by step type and model.
	ValidationVerdict(stepType, resource, status string)
	// ValidatorResult records one validator's contribution to a chain
	// verdict (ticket 11.6) by validator name and per-validator status
	// (pass/fail/skipped/error). One per configured validator per verdict, so
	// the per-validator failure rate and on_error:skip degradations are
	// visible.
	ValidatorResult(validator, status string)
	// SemanticRetryDepth records one terminated semantic-retry loop (ticket
	// 11.6, ADR-013) by terminal outcome (succeeded/validation_failed) and
	// the number of semantic attempts it took (1 = passed/failed first try).
	// Recorded once per loop end — a pass verdict or an exhaustion
	// dead-letter — never on an intermediate feedback-augmented re-attempt.
	SemanticRetryDepth(outcome string, attempts int)
	// OutputRepair records one structured-output shaping result (ticket
	// 11.6, ADR-013) by provenance status (native/raw/repaired/unrepairable).
	// One per productive llm attempt that declared an output_format; cache
	// hits are excluded (the provenance was counted at the miss), so the
	// repair rate reads over real provider calls.
	OutputRepair(status string)
	// JudgeScore records one cost-bearing validator's quality score (ticket
	// 11.6, ADR-013) by validator name, as a [0,1] ratio — the llm_judge's
	// score distribution in bounded buckets.
	JudgeScore(validator string, score float64)
	// ContextUtilization records one guarded llm claim's provider-window
	// utilization (ticket 12.6, ADR-014) by resolved resource, as the ratio
	// (preflight_tokens + max_tokens) / context_window. Recorded pre-call for
	// every llm step whose model has a known window, so the distribution shows
	// how close assembled requests run to the window (and that compaction keeps
	// them below 1.0).
	ContextUtilization(resource string, ratio float64)
	// ContextWindowRejection records one claim the provider-window guardrail
	// terminated (ticket 12.6): the assembled request plus max_tokens exceeded
	// the model context window and compaction was absent or insufficient, so the
	// step failed permanently before any provider call, by resolved resource.
	ContextWindowRejection(resource string)
	// ApprovalDecided records one human-approval decision (ticket 15.3/15.4,
	// ADR-017/008) by decision (approve/reject) and source (human/timeout). The
	// worker records the timeout-sourced decisions here; the API server records
	// human-sourced ones on its own instrument set (the same
	// engine_approval_decisions_total series, distinct registries — the
	// build_info precedent).
	ApprovalDecided(decision, source string)
	// ApprovalTimeout records one approval timeout policy application (ticket
	// 15.4) by action (rejected/approved/run_parked/run_already_parked) —
	// engine_approval_timeouts_total. Recorded post-commit from the worker's
	// timeout handler.
	ApprovalTimeout(action string)
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
func (nopMetrics) CostSpent(string, int64)                    {}
func (nopMetrics) CostSaved(string, int64)                    {}
func (nopMetrics) CostTokens(string, int64, int64)            {}
func (nopMetrics) BudgetExceeded(string, string)              {}
func (nopMetrics) ModelDowngraded(string)                     {}
func (nopMetrics) ValidationVerdict(string, string, string)   {}
func (nopMetrics) ValidatorResult(string, string)             {}
func (nopMetrics) SemanticRetryDepth(string, int)             {}
func (nopMetrics) OutputRepair(string)                        {}
func (nopMetrics) JudgeScore(string, float64)                 {}
func (nopMetrics) ContextUtilization(string, float64)         {}
func (nopMetrics) ContextWindowRejection(string)              {}
func (nopMetrics) ApprovalDecided(string, string)             {}
func (nopMetrics) ApprovalTimeout(string)                     {}
