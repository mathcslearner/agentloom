package config

import (
	"fmt"
	"strconv"
	"time"
)

// Environment variables read by QueueConfig.
const (
	EnvQueueConsumerBatch        = "AGENTLOOM_QUEUE_CONSUMER_BATCH"
	EnvQueueConsumerBlock        = "AGENTLOOM_QUEUE_CONSUMER_BLOCK"
	EnvQueueLeaseTTL             = "AGENTLOOM_QUEUE_LEASE_TTL"
	EnvQueueHeartbeatInterval    = "AGENTLOOM_QUEUE_HEARTBEAT_INTERVAL"
	EnvQueueReclaimInterval      = "AGENTLOOM_QUEUE_RECLAIM_INTERVAL"
	EnvQueuePoisonThreshold      = "AGENTLOOM_QUEUE_POISON_THRESHOLD"
	EnvQueueJanitorInterval      = "AGENTLOOM_QUEUE_JANITOR_INTERVAL"
	EnvQueueJanitorIdleThreshold = "AGENTLOOM_QUEUE_JANITOR_IDLE_THRESHOLD"
	EnvQueuePromoterTick         = "AGENTLOOM_QUEUE_PROMOTER_TICK"
)

// Defaults per ADR-005's tuning table. They mirror internal/queue's
// zero-value fallbacks and must stay in sync with them; the literals are
// duplicated here because config must not import queue (queue logs through
// internal/obs/log, which imports this package).
const (
	DefaultQueueConsumerBatch        = 16
	DefaultQueueConsumerBlock        = 5 * time.Second
	DefaultQueueLeaseTTL             = 30 * time.Second
	DefaultQueuePoisonThreshold      = 5
	DefaultQueueJanitorInterval      = 10 * time.Minute
	DefaultQueueJanitorIdleThreshold = time.Hour
	DefaultQueuePromoterTick         = time.Second
)

// QueueConfig configures the Redis Streams consumer loop, lease
// machinery, and delayed-delivery promoter (ADR-005, internal/queue).
type QueueConfig struct {
	// ConsumerBatch is the XREADGROUP COUNT: the maximum number of entries
	// fetched per read, and also the per-tick XAUTOCLAIM batch bound. Must
	// be positive.
	ConsumerBatch int
	// ConsumerBlock is the XREADGROUP BLOCK timeout: the upper bound on how
	// long an idle consumer waits for work before re-checking for shutdown.
	// Must be positive.
	ConsumerBlock time.Duration
	// LeaseTTL is the XAUTOCLAIM min-idle threshold: a PEL entry idle
	// longer than this is an expired lease, eligible for reclaim by any
	// consumer. Must be positive.
	LeaseTTL time.Duration
	// HeartbeatInterval is the base period between XCLAIM JUSTID heartbeats
	// for an in-flight entry (±20% jitter is applied per beat). Zero means
	// derived: LeaseTTL/3, so two consecutive missed beats still precede
	// expiry — the ratio survives a LeaseTTL-only override. Must be
	// positive when set explicitly.
	HeartbeatInterval time.Duration
	// ReclaimInterval is the period between XAUTOCLAIM passes. Zero means
	// derived: LeaseTTL/2, bounding worst-case reclaim latency to
	// TTL + TTL/2 per ADR-005. Must be positive when set explicitly.
	ReclaimInterval time.Duration
	// PoisonThreshold is the delivery count above which a reclaimed entry
	// is diverted to the poison callback instead of the handler. Must be
	// positive.
	PoisonThreshold int
	// JanitorInterval is the period between orphan-consumer cleanup passes.
	// Must be positive.
	JanitorInterval time.Duration
	// JanitorIdleThreshold is how long a consumer with an empty PEL must be
	// idle before the janitor deletes it. Generous by design (hours, not
	// lease TTLs) — cleanup is cosmetic-plus-memory. Must be positive.
	JanitorIdleThreshold time.Duration
	// PromoterTick is the period between delayed-delivery promotion passes:
	// each tick moves due entries from the sched:delayed sorted set onto
	// the ready stream. Delayed delivery serves backoffs and timeouts
	// measured in seconds-to-days, so sub-second precision buys nothing.
	// Must be positive.
	PromoterTick time.Duration
}

func defaultQueueConfig() QueueConfig {
	return QueueConfig{
		ConsumerBatch:        DefaultQueueConsumerBatch,
		ConsumerBlock:        DefaultQueueConsumerBlock,
		LeaseTTL:             DefaultQueueLeaseTTL,
		PoisonThreshold:      DefaultQueuePoisonThreshold,
		JanitorInterval:      DefaultQueueJanitorInterval,
		JanitorIdleThreshold: DefaultQueueJanitorIdleThreshold,
		PromoterTick:         DefaultQueuePromoterTick,
	}
}

// applyEnv overrides fields from the environment, returning one error per
// invalid variable.
func (c *QueueConfig) applyEnv(fn LookupFunc) []error {
	var errs []error
	errs = applyPositiveInt(errs, fn, EnvQueueConsumerBatch, &c.ConsumerBatch)
	errs = applyPositiveDuration(errs, fn, EnvQueueConsumerBlock, &c.ConsumerBlock)
	errs = applyPositiveDuration(errs, fn, EnvQueueLeaseTTL, &c.LeaseTTL)
	errs = applyPositiveDuration(errs, fn, EnvQueueHeartbeatInterval, &c.HeartbeatInterval)
	errs = applyPositiveDuration(errs, fn, EnvQueueReclaimInterval, &c.ReclaimInterval)
	errs = applyPositiveInt(errs, fn, EnvQueuePoisonThreshold, &c.PoisonThreshold)
	errs = applyPositiveDuration(errs, fn, EnvQueueJanitorInterval, &c.JanitorInterval)
	errs = applyPositiveDuration(errs, fn, EnvQueueJanitorIdleThreshold, &c.JanitorIdleThreshold)
	errs = applyPositiveDuration(errs, fn, EnvQueuePromoterTick, &c.PromoterTick)
	return errs
}

// applyPositiveInt overrides *dst from the environment when the variable is
// set, appending an error for a non-positive or unparseable value.
func applyPositiveInt(errs []error, fn LookupFunc, key string, dst *int) []error {
	raw, ok := lookup(fn, key)
	if !ok {
		return errs
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return append(errs, fmt.Errorf("%s: invalid value %q (want a positive integer)", key, raw))
	}
	*dst = n
	return errs
}

// applyPositiveDuration is applyPositiveInt for Go durations.
func applyPositiveDuration(errs []error, fn LookupFunc, key string, dst *time.Duration) []error {
	raw, ok := lookup(fn, key)
	if !ok {
		return errs
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		return append(errs, fmt.Errorf("%s: invalid value %q (want a positive Go duration, e.g. 5s)", key, raw))
	}
	*dst = d
	return errs
}
