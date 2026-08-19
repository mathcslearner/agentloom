package queue

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"
)

// StreamStats is a point-in-time snapshot of one stream + group pair, built
// from XLEN and the XPENDING summary. It feeds the M7 queue metrics and
// M13's system-stats endpoint; nothing here is an input to protocol logic.
type StreamStats struct {
	// Length is the number of entries in the stream (XLEN): undelivered
	// entries, delivered-but-unacked entries, and acked entries the trim
	// duty (TrimAcked) has not yet removed. Between trim passes it
	// overstates the ready depth by up to one interval of acked
	// throughput.
	Length int64
	// Pending is the total number of delivered-but-unacked entries in the
	// group's PEL — the in-flight (leased) count.
	Pending int64
	// Lag is the number of stream entries never delivered to the group —
	// the true ready depth (XINFO GROUPS lag; ticket 7.2). It is -1 when
	// Redis cannot compute it (possible after trims/XDEL make the exact
	// count unknowable); callers wanting a depth then approximate with
	// max(0, Length-Pending), which overstates by acked-but-untrimmed
	// entries only.
	Lag int64
	// Consumers breaks Pending down per consumer, sorted by name for
	// deterministic output.
	Consumers []ConsumerPending
}

// ReadyDepth is the best available undelivered-entry count: the exact
// group lag when Redis reports one, otherwise the Length−Pending
// approximation (never negative).
func (s StreamStats) ReadyDepth() int64 {
	if s.Lag >= 0 {
		return s.Lag
	}
	return max(s.Length-s.Pending, 0)
}

// ConsumerPending is one consumer's share of the PEL.
type ConsumerPending struct {
	Name    string
	Pending int64
}

// Stats returns the stream depth and PEL counts. It fails with ErrNoGroup
// when the consumer group has not been created yet (call EnsureGroup
// first) — introspection never fabricates zeros for a group it cannot see.
func (q *Queue) Stats(ctx context.Context) (StreamStats, error) {
	length, err := q.client.XLen(ctx, q.stream).Result()
	if err != nil {
		return StreamStats{}, fmt.Errorf("queue: XLEN %s: %w", q.stream, err)
	}
	pending, err := q.client.XPending(ctx, q.stream, q.group).Result()
	if err != nil {
		if isNoGroup(err) {
			return StreamStats{}, fmt.Errorf("queue: group %s on %s: %w", q.group, q.stream, ErrNoGroup)
		}
		return StreamStats{}, fmt.Errorf("queue: XPENDING %s %s: %w", q.stream, q.group, err)
	}
	stats := StreamStats{
		Length: length,
		// Unknown until the group shows up in XINFO GROUPS below — the
		// same sentinel go-redis uses for a null lag.
		Lag:       -1,
		Pending:   pending.Count,
		Consumers: make([]ConsumerPending, 0, len(pending.Consumers)),
	}
	for name, count := range pending.Consumers {
		stats.Consumers = append(stats.Consumers, ConsumerPending{Name: name, Pending: count})
	}
	sort.Slice(stats.Consumers, func(i, j int) bool {
		return stats.Consumers[i].Name < stats.Consumers[j].Name
	})
	groups, err := q.client.XInfoGroups(ctx, q.stream).Result()
	if err != nil {
		return StreamStats{}, fmt.Errorf("queue: XINFO GROUPS %s: %w", q.stream, err)
	}
	for _, g := range groups {
		if g.Name == q.group {
			// go-redis reports Redis's null lag as -1, matching the field's
			// unknown sentinel.
			stats.Lag = g.Lag
			break
		}
	}
	return stats, nil
}

// ActiveConsumers counts the group's consumers whose idle time is at most
// maxIdle — the fleet-size observable behind the active-workers gauge
// (ticket 7.2). A live consumer re-issues its blocking read every Block,
// so maxIdle a few multiples of Block separates live members from
// crashed ones the janitor has not yet collected. Fails with ErrNoGroup
// like Stats when the group does not exist yet.
func (q *Queue) ActiveConsumers(ctx context.Context, maxIdle time.Duration) (int, error) {
	consumers, err := q.client.XInfoConsumers(ctx, q.stream, q.group).Result()
	if err != nil {
		if isNoGroup(err) {
			return 0, fmt.Errorf("queue: group %s on %s: %w", q.group, q.stream, ErrNoGroup)
		}
		return 0, fmt.Errorf("queue: XINFO CONSUMERS %s %s: %w", q.stream, q.group, err)
	}
	active := 0
	for _, c := range consumers {
		if c.Idle <= maxIdle {
			active++
		}
	}
	return active, nil
}

// ConsumerInfo is one group consumer as XINFO CONSUMERS reports it — the
// worker-fleet detail behind /v1/system/stats (ticket 18.6). Name is the
// queue consumer name (queue.NewConsumerName, the worker_id on every log line);
// Idle is how long since it last read; Pending is its share of the PEL.
type ConsumerInfo struct {
	Name    string
	Idle    time.Duration
	Pending int64
}

// ListConsumers returns the group's consumers (XINFO CONSUMERS) with idle time
// and PEL share, sorted by name for deterministic output — the fleet roster the
// system-stats panel renders. ActiveConsumers gives only a count; this gives the
// per-worker breakdown. Fails with ErrNoGroup like Stats when the group does not
// exist yet.
func (q *Queue) ListConsumers(ctx context.Context) ([]ConsumerInfo, error) {
	consumers, err := q.client.XInfoConsumers(ctx, q.stream, q.group).Result()
	if err != nil {
		if isNoGroup(err) {
			return nil, fmt.Errorf("queue: group %s on %s: %w", q.group, q.stream, ErrNoGroup)
		}
		return nil, fmt.Errorf("queue: XINFO CONSUMERS %s %s: %w", q.stream, q.group, err)
	}
	out := make([]ConsumerInfo, 0, len(consumers))
	for _, c := range consumers {
		out = append(out, ConsumerInfo{Name: c.Name, Idle: c.Idle, Pending: c.Pending})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// isNoGroup reports whether err is Redis's NOGROUP reply. Same necessity
// as isBusyGroup: go-redis surfaces error replies as plain errors.
func isNoGroup(err error) bool {
	return err != nil && strings.HasPrefix(err.Error(), "NOGROUP")
}
