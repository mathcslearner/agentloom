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
