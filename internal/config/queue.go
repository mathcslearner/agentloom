package config

import (
	"fmt"
	"strconv"
	"time"
)

// Environment variables read by QueueConfig.
const (
	EnvQueueConsumerBatch = "AGENTLOOM_QUEUE_CONSUMER_BATCH"
	EnvQueueConsumerBlock = "AGENTLOOM_QUEUE_CONSUMER_BLOCK"
)

// Defaults per ADR-005's tuning table (XREADGROUP COUNT / BLOCK). They
// mirror internal/queue's zero-value fallbacks and must stay in sync with
// them; the literals are duplicated here because config must not import
// queue (queue logs through internal/obs/log, which imports this package).
const (
	DefaultQueueConsumerBatch = 16
	DefaultQueueConsumerBlock = 5 * time.Second
)

// QueueConfig configures the Redis Streams consumer loop (ADR-005,
// internal/queue). The lease, reclaim, and promoter tunables join it with
// tickets 3.4–3.5.
type QueueConfig struct {
	// ConsumerBatch is the XREADGROUP COUNT: the maximum number of entries
	// fetched per read. Must be positive.
	ConsumerBatch int
	// ConsumerBlock is the XREADGROUP BLOCK timeout: the upper bound on how
	// long an idle consumer waits for work before re-checking for shutdown.
	// Must be positive.
	ConsumerBlock time.Duration
}

func defaultQueueConfig() QueueConfig {
	return QueueConfig{
		ConsumerBatch: DefaultQueueConsumerBatch,
		ConsumerBlock: DefaultQueueConsumerBlock,
	}
}

// applyEnv overrides fields from the environment, returning one error per
// invalid variable.
func (c *QueueConfig) applyEnv(fn LookupFunc) []error {
	var errs []error
	if raw, ok := lookup(fn, EnvQueueConsumerBatch); ok {
		if n, err := strconv.Atoi(raw); err != nil || n <= 0 {
			errs = append(errs, fmt.Errorf("%s: invalid value %q (want a positive integer)", EnvQueueConsumerBatch, raw))
		} else {
			c.ConsumerBatch = n
		}
	}
	if raw, ok := lookup(fn, EnvQueueConsumerBlock); ok {
		if d, err := time.ParseDuration(raw); err != nil || d <= 0 {
			errs = append(errs, fmt.Errorf("%s: invalid value %q (want a positive Go duration, e.g. 5s)", EnvQueueConsumerBlock, raw))
		} else {
			c.ConsumerBlock = d
		}
	}
	return errs
}
