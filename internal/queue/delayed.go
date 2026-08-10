package queue

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/mathcslearner/agentloom/internal/obs/log"
)

// Delayed-delivery defaults per ADR-005. The promoter tick mirrors
// config.DefaultQueuePromoterTick (the usual keep-in-sync duplication:
// config cannot import queue).
const (
	// DefaultDelayedKey is the ADR-005 sorted-set key for delayed
	// envelopes. Like the stream name, it is parameterized per instance —
	// tests pass unique keys, and M19 stream sharding will need per-shard
	// delayed keys, because a promoter XADDs to its own queue's stream.
	DefaultDelayedKey = "sched:delayed"
	// DefaultPromoterTick is the period between promotion passes. Delayed
	// delivery serves backoffs and timeouts measured in seconds-to-days;
	// sub-second precision buys nothing.
	DefaultPromoterTick = time.Second
)

// malformedKeySuffix names the quarantine list relative to the delayed
// key: members the promoter cannot decode are moved here rather than
// silently dropped (or worse, left wedging the batch window — a malformed
// member has a due score, so it would be re-selected first every tick,
// stalling every promoter in the fleet forever).
const malformedKeySuffix = ":malformed"

// Delayed is a handle on one delayed-delivery sorted set feeding one
// destination Queue: member = encoded envelope, score = fire-at time in
// epoch milliseconds (ADR-005). Entries here are scheduling state, not
// truth — each tenant must hold durable Postgres state from which a lost
// entry is re-derivable.
type Delayed struct {
	client redis.Cmdable
	key    string
	queue  *Queue
}

// NewDelayed binds a delayed-delivery set to this queue. An empty key
// falls back to the ADR-005 default; tests pass unique keys for
// isolation.
func (q *Queue) NewDelayed(key string) *Delayed {
	if key == "" {
		key = DefaultDelayedKey
	}
	return &Delayed{client: q.client, key: key, queue: q}
}

// Key returns the sorted-set key this handle schedules into.
func (d *Delayed) Key() string { return d.key }

// MalformedKey returns the quarantine list key: undecodable members land
// here with contents preserved, per ADR-005's no-silent-drop rule.
func (d *Delayed) MalformedKey() string { return d.key + malformedKeySuffix }

// Schedule adds the envelope to the delayed set, due at fireAt. fireAt is
// caller-supplied per the injectable-time invariant — the queue library
// holds no clock. Scheduling an identical envelope again *moves its fire
// time* rather than queueing a second copy (plain ZADD semantics, pinned
// by ADR-005): at most one pending future dispatch exists per identical
// envelope, so a tenant needing two independent future dispatches of the
// same step must make the envelopes distinct.
func (d *Delayed) Schedule(ctx context.Context, env Envelope, fireAt time.Time) error {
	member, err := encodeDelayedMember(env)
	if err != nil {
		return err
	}
	if fireAt.IsZero() {
		return errors.New("queue: Schedule: zero fireAt — pass the injected fire time")
	}
	err = d.client.ZAdd(ctx, d.key, redis.Z{
		Score:  float64(fireAt.UnixMilli()),
		Member: member,
	}).Err()
	if err != nil {
		return fmt.Errorf("queue: ZADD to %s: %w", d.key, err)
	}
	return nil
}

// Len returns the number of entries waiting in the delayed set — the M7
// depth metric and 3.6's delayed-empty quiescence assertion.
func (d *Delayed) Len(ctx context.Context) (int64, error) {
	n, err := d.client.ZCard(ctx, d.key).Result()
	if err != nil {
		return 0, fmt.Errorf("queue: ZCARD %s: %w", d.key, err)
	}
	return n, nil
}

// encodeDelayedMember renders the envelope as a ZSET member: the JSON
// object of its stream field–value pairs. json.Marshal sorts map keys, so
// identical envelopes produce byte-identical members — which is what
// gives ZADD its move-the-fire-time dedup semantics. The promoter's Lua
// script decodes the member back into XADD args, so a promoted stream
// entry is identical to what Enqueue would have written.
func encodeDelayedMember(env Envelope) (string, error) {
	values, err := env.Encode()
	if err != nil {
		return "", err
	}
	b, err := json.Marshal(values)
	if err != nil {
		// Unreachable: Encode produces only string values.
		return "", fmt.Errorf("queue: encoding delayed member: %w", err)
	}
	return string(b), nil
}

// promoteScript atomically moves due members from the delayed set to the
// ready stream. Atomicity is the ADR-005 guarantee: there is no crash
// point between "removed from the set" and "added to the stream", so
// within one Redis, promotion neither loses nor duplicates entries.
//
// A member that does not decode to a flat string→string object is moved
// to the quarantine list instead of the stream: XADD with garbage args
// would raise a Lua error mid-script, and since the bad member holds a
// due score it would be re-selected first on every tick — wedging every
// promoter in the fleet. Quarantine preserves the contents for an
// operator, never a silent drop.
//
// KEYS[1] = delayed zset, KEYS[2] = ready stream, KEYS[3] = quarantine
// list. ARGV[1] = now in epoch ms (caller's injected clock — the script
// never reads Redis server time), ARGV[2] = batch limit. Returns
// {promoted-scores, quarantined-count}.
var promoteScript = redis.NewScript(`
local due = redis.call('ZRANGEBYSCORE', KEYS[1], '-inf', ARGV[1], 'WITHSCORES', 'LIMIT', 0, ARGV[2])
local promoted = {}
local quarantined = 0
for i = 1, #due, 2 do
  local member = due[i]
  local args = nil
  local ok, fields = pcall(cjson.decode, member)
  if ok and type(fields) == 'table' then
    args = {}
    for k, v in pairs(fields) do
      if type(k) == 'string' and type(v) == 'string' then
        args[#args+1] = k
        args[#args+1] = v
      else
        args = nil
        break
      end
    end
    if args ~= nil and #args == 0 then
      args = nil
    end
  end
  if args ~= nil then
    redis.call('XADD', KEYS[2], '*', unpack(args))
    promoted[#promoted+1] = due[i+1]
  else
    redis.call('RPUSH', KEYS[3], member)
    quarantined = quarantined + 1
  end
  redis.call('ZREM', KEYS[1], member)
end
return {promoted, quarantined}
`)

// PromoteResult reports one promotion pass.
type PromoteResult struct {
	// Promoted is the number of members moved to the ready stream.
	Promoted int
	// Quarantined is the number of undecodable members moved to the
	// quarantine list instead.
	Quarantined int
	// MaxLag is the largest now−fireAt among promoted members: the
	// promotion-latency observable that feeds the M7 metric. Zero when
	// nothing promoted.
	MaxLag time.Duration
}

// PromoteDue atomically moves up to limit members due at now (score ≤
// now) from the delayed set onto the ready stream, oldest first. now
// comes from the caller's injectable clock — fake-clock tests drive
// promotion directly through this method. Promotion is at-least-once at
// the system level (AOF replay, owner re-scheduling); duplicates are
// absorbed downstream at the claim CAS like every other duplicate. A
// non-positive limit falls back to DefaultConsumerBatch.
func (d *Delayed) PromoteDue(ctx context.Context, now time.Time, limit int) (PromoteResult, error) {
	if now.IsZero() {
		return PromoteResult{}, errors.New("queue: PromoteDue: zero now — pass the injected current time")
	}
	if limit <= 0 {
		limit = DefaultConsumerBatch
	}
	raw, err := promoteScript.Run(ctx, d.client,
		[]string{d.key, d.queue.stream, d.MalformedKey()},
		now.UnixMilli(), limit).Result()
	if err != nil {
		return PromoteResult{}, fmt.Errorf("queue: promoting due entries from %s: %w", d.key, err)
	}
	return parsePromoteReply(raw, now)
}

// parsePromoteReply decodes the script's {promoted-scores, quarantined}
// reply. Scores come back as Redis score strings; each is a fire-at in
// epoch ms, and now−fireAt is that member's promotion lag.
func parsePromoteReply(raw any, now time.Time) (PromoteResult, error) {
	reply, ok := raw.([]any)
	if !ok || len(reply) != 2 {
		return PromoteResult{}, fmt.Errorf("queue: unexpected promote script reply %v", raw)
	}
	scores, ok := reply[0].([]any)
	if !ok {
		return PromoteResult{}, fmt.Errorf("queue: unexpected promoted-scores reply %v", reply[0])
	}
	quarantined, ok := reply[1].(int64)
	if !ok {
		return PromoteResult{}, fmt.Errorf("queue: unexpected quarantined-count reply %v", reply[1])
	}
	res := PromoteResult{Promoted: len(scores), Quarantined: int(quarantined)}
	for _, s := range scores {
		str, ok := s.(string)
		if !ok {
			return PromoteResult{}, fmt.Errorf("queue: unexpected score reply %v", s)
		}
		score, err := strconv.ParseFloat(str, 64)
		if err != nil {
			return PromoteResult{}, fmt.Errorf("queue: unparseable score %q: %w", str, err)
		}
		if lag := time.Duration(now.UnixMilli()-int64(score)) * time.Millisecond; lag > res.MaxLag {
			res.MaxLag = lag
		}
	}
	return res, nil
}

// promoteTick runs one promotion pass from the consumer's duty loop,
// passing the wall clock as now — the same real-time convention the loop
// already uses for duty scheduling; fake-clock tests target PromoteDue
// directly. Errors log and retry next tick, like the reclaimer.
func (c *Consumer) promoteTick(ctx context.Context) {
	res, err := c.delayed.PromoteDue(ctx, time.Now(), c.cfg.Batch)
	if err != nil {
		if ctx.Err() == nil {
			log.From(ctx).ErrorContext(ctx, "delayed promotion failed; retrying next tick",
				slog.Any("error", err))
		}
		return
	}
	if res.Quarantined > 0 {
		log.From(ctx).ErrorContext(ctx, "quarantined undecodable delayed members",
			slog.Int("count", res.Quarantined),
			slog.String("key", c.delayed.MalformedKey()))
	}
	if res.Promoted > 0 {
		log.From(ctx).InfoContext(ctx, "promoted delayed entries",
			slog.Int("count", res.Promoted),
			slog.Duration("max_lag", res.MaxLag))
	}
}
