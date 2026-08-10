package queue

import (
	"context"
	"fmt"
	"sort"
	"strings"
)

// StreamStats is a point-in-time snapshot of one stream + group pair, built
// from XLEN and the XPENDING summary. It feeds the M7 queue metrics and
// M13's system-stats endpoint; nothing here is an input to protocol logic.
type StreamStats struct {
	// Length is the number of entries in the stream (XLEN) — the ready
	// depth, including entries already delivered but not yet acked.
	Length int64
	// Pending is the total number of delivered-but-unacked entries in the
	// group's PEL — the in-flight (leased) count.
	Pending int64
	// Consumers breaks Pending down per consumer, sorted by name for
	// deterministic output.
	Consumers []ConsumerPending
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
		Length:    length,
		Pending:   pending.Count,
		Consumers: make([]ConsumerPending, 0, len(pending.Consumers)),
	}
	for name, count := range pending.Consumers {
		stats.Consumers = append(stats.Consumers, ConsumerPending{Name: name, Pending: count})
	}
	sort.Slice(stats.Consumers, func(i, j int) bool {
		return stats.Consumers[i].Name < stats.Consumers[j].Name
	})
	return stats, nil
}

// isNoGroup reports whether err is Redis's NOGROUP reply. Same necessity
// as isBusyGroup: go-redis surfaces error replies as plain errors.
func isNoGroup(err error) bool {
	return err != nil && strings.HasPrefix(err.Error(), "NOGROUP")
}
