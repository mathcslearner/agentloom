package queue

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"strconv"
	"strings"

	"github.com/mathcslearner/agentloom/internal/obs/log"
)

// TrimAcked removes stream entries the consumer group has permanently
// forgotten: XACK clears the PEL but leaves the entry in the stream, so
// without trimming the stream retains every envelope ever enqueued
// (ADR-005, stream retention). The trim threshold is the group's smallest
// pending entry ID — or, with an empty PEL, the successor of its
// last-delivered ID — so pending and undelivered entries are never
// touched: everything below the threshold has been delivered and acked.
//
// The threshold is monotonically safe against concurrent consumers:
// XREADGROUP `>` only delivers IDs above last-delivered-id, and
// XCLAIM/XAUTOCLAIM only operate on entries already in the PEL, so no
// entry below a snapshotted threshold can ever become pending again — a
// stale snapshot merely trims less. Returns the number of entries
// removed. Fails with ErrNoGroup when the group (or stream) does not
// exist yet; call EnsureGroup first.
func (q *Queue) TrimAcked(ctx context.Context) (int64, error) {
	groups, err := q.client.XInfoGroups(ctx, q.stream).Result()
	if err != nil {
		if isNoSuchKey(err) {
			return 0, fmt.Errorf("queue: group %s on %s: %w", q.group, q.stream, ErrNoGroup)
		}
		return 0, fmt.Errorf("queue: XINFO GROUPS %s: %w", q.stream, err)
	}
	lastDelivered := ""
	for _, g := range groups {
		if g.Name == q.group {
			lastDelivered = g.LastDeliveredID
			break
		}
	}
	if lastDelivered == "" {
		return 0, fmt.Errorf("queue: group %s on %s: %w", q.group, q.stream, ErrNoGroup)
	}
	pending, err := q.client.XPending(ctx, q.stream, q.group).Result()
	if err != nil {
		return 0, fmt.Errorf("queue: XPENDING %s %s: %w", q.stream, q.group, err)
	}
	var threshold string
	if pending.Count > 0 {
		threshold = pending.Lower
	} else {
		if threshold, err = successorStreamID(lastDelivered); err != nil {
			return 0, err
		}
	}
	trimmed, err := q.client.XTrimMinID(ctx, q.stream, threshold).Result()
	if err != nil {
		return 0, fmt.Errorf("queue: XTRIM %s MINID %s: %w", q.stream, threshold, err)
	}
	return trimmed, nil
}

// successorStreamID returns the smallest stream ID strictly greater than
// id — the XTRIM MINID threshold that removes id itself while keeping
// everything after it.
func successorStreamID(id string) (string, error) {
	msRaw, seqRaw, ok := strings.Cut(id, "-")
	if !ok {
		return "", fmt.Errorf("queue: stream ID %q has no sequence part", id)
	}
	ms, err := strconv.ParseUint(msRaw, 10, 64)
	if err != nil {
		return "", fmt.Errorf("queue: stream ID %q has a non-integer timestamp: %w", id, err)
	}
	seq, err := strconv.ParseUint(seqRaw, 10, 64)
	if err != nil {
		return "", fmt.Errorf("queue: stream ID %q has a non-integer sequence: %w", id, err)
	}
	if seq == math.MaxUint64 {
		return fmt.Sprintf("%d-0", ms+1), nil
	}
	return fmt.Sprintf("%d-%d", ms, seq+1), nil
}

// isNoSuchKey reports whether err is Redis's reply for XINFO on a missing
// stream. Same necessity as isBusyGroup: go-redis surfaces Redis error
// replies as plain errors.
func isNoSuchKey(err error) bool {
	return err != nil && strings.HasPrefix(err.Error(), "ERR no such key")
}

// trimTick runs one retention pass from the consumer's duty loop. Errors
// log and retry next tick, like the other duties; a crash anywhere in the
// pass is harmless — the next pass, on any consumer, recomputes the
// threshold from live state.
func (c *Consumer) trimTick(ctx context.Context) {
	trimmed, err := c.queue.TrimAcked(ctx)
	if err != nil {
		if ctx.Err() == nil {
			log.From(ctx).ErrorContext(ctx, "stream trim failed; retrying next tick",
				slog.Any("error", err))
		}
		return
	}
	if trimmed > 0 {
		log.From(ctx).InfoContext(ctx, "trimmed acked stream entries",
			slog.Int64("count", trimmed))
	}
}
