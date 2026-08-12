package queue

import "time"

// ConsumerMetrics is the consumer duties' observability seam (ticket 7.2,
// ADR-008): the queue package stays free of any metrics dependency, and
// cmd/worker wires internal/obs/metrics.WorkerMetrics here, which
// satisfies this interface structurally. Implementations must be safe for
// concurrent use and must not block — they sit on the duty hot paths.
type ConsumerMetrics interface {
	// Reclaimed records n expired-lease entries taken by one XAUTOCLAIM
	// pass.
	Reclaimed(n int)
	// PoisonDiverted records one entry handed to the poison handler
	// (and consumed by it).
	PoisonDiverted()
	// Promoted records one promotion pass: n delayed members moved to the
	// ready stream, maxLag the worst now−fireAt among them.
	Promoted(n int, maxLag time.Duration)
}

// nopConsumerMetrics is the default ConsumerMetrics: every test layer and
// harness runs with recording off unless it opts in.
type nopConsumerMetrics struct{}

func (nopConsumerMetrics) Reclaimed(int)               {}
func (nopConsumerMetrics) PoisonDiverted()             {}
func (nopConsumerMetrics) Promoted(int, time.Duration) {}
