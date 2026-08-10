package queue

import (
	"context"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/mathcslearner/agentloom/internal/obs/log"
)

// startHeartbeat launches the heartbeater goroutine for one in-flight
// entry: every HeartbeatInterval (±20% jitter per beat) it XCLAIMs the
// entry to this consumer with JUSTID, resetting the PEL idle time so the
// lease never expires while the handler runs. JUSTID is load-bearing per
// ADR-005: a plain XCLAIM would increment the delivery counter, and the
// counter must stay a pure redelivery signal — a long step heartbeating
// for an hour must not look like a poison message.
//
// The returned stop function signals the goroutine and waits for it to
// exit, so no beat can race the subsequent ack.
func (c *Consumer) startHeartbeat(ctx context.Context, entryID string) (stop func()) {
	stopCh := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			timer := time.NewTimer(heartbeatJitter(c.cfg.HeartbeatInterval))
			select {
			case <-stopCh:
				timer.Stop()
				return
			case <-timer.C:
			}
			if !c.heartbeat(ctx, entryID) {
				return
			}
		}
	}()
	return func() {
		close(stopCh)
		<-done
	}
}

// Heartbeat outcomes reported by heartbeatScript.
const (
	heartbeatGone      = 0 // entry no longer pending (acked or deleted)
	heartbeatOK        = 1 // still owned by this consumer; idle reset
	heartbeatDisplaced = 2 // pending under another consumer
)

// heartbeatScript is the ownership-guarded beat (ADR-005, post-M3 audit):
// XCLAIM min-idle 0 alone would unconditionally transfer ownership, so a
// stalled consumer that resumes after its lease was reclaimed would
// silently steal the entry back from the legitimate new holder and the
// two heartbeaters would flap ownership. The script claims only when the
// beater still owns the entry, atomically — check and claim cannot race.
//
// KEYS[1] = stream. ARGV[1] = group, ARGV[2] = consumer, ARGV[3] = entry
// ID. Returns {code, owner}: owner is the displacing consumer's name when
// code is heartbeatDisplaced, else "".
var heartbeatScript = redis.NewScript(`
local p = redis.call('XPENDING', KEYS[1], ARGV[1], ARGV[3], ARGV[3], 1)
if #p == 0 then return {0, ''} end
local owner = p[1][2]
if owner ~= ARGV[2] then return {2, owner} end
redis.call('XCLAIM', KEYS[1], ARGV[1], ARGV[2], '0', ARGV[3], 'JUSTID')
return {1, ''}
`)

// heartbeat issues one guarded XCLAIM JUSTID to self, reporting whether
// the heartbeater should keep beating. It runs on a detached context so a
// handler draining through shutdown keeps its lease. Failure is logged,
// never fatal: per ADR-005 (crash cell R1b), a worker whose heartbeat
// fails — Redis down, entry reclaimed, or PEL lost — logs and keeps
// executing, because correctness rests on the Postgres claim_id fence,
// not on the lease. A transport error keeps the beater alive (the next
// beat may succeed); a definitive loss of the lease (entry gone or owned
// by another consumer) stops it — there is no lease left to maintain.
func (c *Consumer) heartbeat(ctx context.Context, entryID string) bool {
	hbCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), detachedOpTimeout)
	defer cancel()
	raw, err := heartbeatScript.Run(hbCtx, c.queue.client,
		[]string{c.queue.stream}, c.queue.group, c.name, entryID).Result()
	if err != nil {
		log.From(ctx).WarnContext(ctx, "lease heartbeat failed; execution continues (claim_id fences)",
			slog.Any("error", err))
		return true
	}
	code, owner, err := parseHeartbeatReply(raw)
	if err != nil {
		log.From(ctx).WarnContext(ctx, "lease heartbeat reply unparseable; execution continues (claim_id fences)",
			slog.Any("error", err))
		return true
	}
	switch code {
	case heartbeatOK:
		return true
	case heartbeatDisplaced:
		log.From(ctx).WarnContext(ctx, "lease reclaimed by another consumer; execution continues (claim_id fences)",
			slog.String("new_owner", owner))
		return false
	default: // heartbeatGone
		log.From(ctx).WarnContext(ctx, "lease heartbeat found no PEL entry; execution continues (claim_id fences)")
		return false
	}
}

// parseHeartbeatReply decodes heartbeatScript's {code, owner} reply.
func parseHeartbeatReply(raw any) (code int64, owner string, err error) {
	reply, ok := raw.([]any)
	if !ok || len(reply) != 2 {
		return 0, "", fmt.Errorf("queue: unexpected heartbeat script reply %v", raw)
	}
	if code, ok = reply[0].(int64); !ok {
		return 0, "", fmt.Errorf("queue: unexpected heartbeat code reply %v", reply[0])
	}
	// Lua's '' comes back as a nil bulk string on some paths; tolerate both.
	if reply[1] != nil {
		if owner, ok = reply[1].(string); !ok {
			return 0, "", fmt.Errorf("queue: unexpected heartbeat owner reply %v", reply[1])
		}
	}
	return code, owner, nil
}

// heartbeatJitter perturbs the base interval by ±20% so a fleet of
// heartbeaters never aligns (ADR-005 tuning table).
func heartbeatJitter(interval time.Duration) time.Duration {
	return time.Duration(float64(interval) * (0.8 + 0.4*rand.Float64())) //nolint:gosec // jitter, not cryptography
}
