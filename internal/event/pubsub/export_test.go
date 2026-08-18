package pubsub

import "context"

// SetPublishFn overrides the publisher's per-channel publish function, so a unit
// test can simulate a hung or failing PUBLISH without a live Redis. Not for
// production use.
func (p *Publisher) SetPublishFn(fn func(ctx context.Context, channel string, msg []byte) error) {
	p.publishFn = fn
}

// QueueLen reports the number of batches queued but not yet drained — a test
// hook for the overflow assertion.
func (p *Publisher) QueueLen() int { return len(p.ch) }
