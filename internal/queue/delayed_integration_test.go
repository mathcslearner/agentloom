//go:build integration

package queue_test

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/mathcslearner/agentloom/internal/queue"
)

// t0 is an arbitrary fixed instant: the promoter's now is caller-supplied
// and the Lua script never reads Redis server time, so these tests drive
// promotion with fully synthetic clocks.
var t0 = time.UnixMilli(1_754_000_000_000).UTC()

// newTestDelayed binds a delayed set to the test queue under a unique key
// derived from the (already unique) stream name, cleaning up both the set
// and its quarantine list.
func newTestDelayed(tb testing.TB, q *queue.Queue) *queue.Delayed {
	tb.Helper()
	d := q.NewDelayed(q.Stream() + ":delayed")
	client := rawClient(tb)
	tb.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
		defer cancel()
		if err := client.Del(ctx, d.Key(), d.MalformedKey()).Err(); err != nil {
			tb.Errorf("deleting delayed keys: %v", err)
		}
	})
	return d
}

// streamEnvelopes decodes every entry currently on the stream, in order.
func streamEnvelopes(ctx context.Context, tb testing.TB, client *redis.Client, q *queue.Queue) []queue.Envelope {
	tb.Helper()
	msgs, err := client.XRange(ctx, q.Stream(), "-", "+").Result()
	if err != nil {
		tb.Fatalf("XRANGE %s: %v", q.Stream(), err)
	}
	envs := make([]queue.Envelope, 0, len(msgs))
	for _, msg := range msgs {
		env, err := queue.DecodeEnvelope(msg.Values)
		if err != nil {
			tb.Fatalf("promoted entry %s does not decode: %v", msg.ID, err)
		}
		envs = append(envs, env)
	}
	return envs
}

// TestPromoteDueFakeClock is acceptance criterion 1 (and 3): with a fully
// synthetic now, due entries promote within one pass with an exact
// promotion lag, and not-yet-due entries never promote.
func TestPromoteDueFakeClock(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
	defer cancel()

	q := newTestQueue(t)
	d := newTestDelayed(t, q)
	client := rawClient(t)

	due := envelopeForStep("due")
	future := envelopeForStep("future")
	if err := d.Schedule(ctx, due, t0.Add(-time.Second)); err != nil {
		t.Fatalf("Schedule due: %v", err)
	}
	if err := d.Schedule(ctx, future, t0.Add(10*time.Second)); err != nil {
		t.Fatalf("Schedule future: %v", err)
	}

	res, err := d.PromoteDue(ctx, t0, 16)
	if err != nil {
		t.Fatalf("PromoteDue at t0: %v", err)
	}
	if res.Promoted != 1 || res.Quarantined != 0 {
		t.Errorf("PromoteDue at t0 = %+v, want Promoted 1, Quarantined 0", res)
	}
	if res.MaxLag != time.Second {
		t.Errorf("MaxLag = %v, want exactly 1s (now − fireAt)", res.MaxLag)
	}
	envs := streamEnvelopes(ctx, t, client, q)
	if len(envs) != 1 || envs[0] != due {
		t.Errorf("stream after promotion = %+v, want exactly the due envelope %+v", envs, due)
	}
	if n, err := d.Len(ctx); err != nil || n != 1 {
		t.Errorf("Len = %d (err %v), want 1 (the future entry stays)", n, err)
	}

	// A second pass at the same now must find nothing: due entries promote
	// once, not-yet-due entries never promote.
	res, err = d.PromoteDue(ctx, t0, 16)
	if err != nil {
		t.Fatalf("second PromoteDue at t0: %v", err)
	}
	if res.Promoted != 0 || res.MaxLag != 0 {
		t.Errorf("second pass at t0 = %+v, want nothing promoted", res)
	}

	// Advance the fake clock past the future entry's fire time.
	res, err = d.PromoteDue(ctx, t0.Add(10*time.Second), 16)
	if err != nil {
		t.Fatalf("PromoteDue at t0+10s: %v", err)
	}
	if res.Promoted != 1 || res.MaxLag != 0 {
		t.Errorf("pass at t0+10s = %+v, want Promoted 1 with zero lag (promoted exactly on time)", res)
	}
	if envs := streamEnvelopes(ctx, t, client, q); len(envs) != 2 || envs[1] != future {
		t.Errorf("stream after second promotion = %+v, want [due, future]", envs)
	}
	if n, err := d.Len(ctx); err != nil || n != 0 {
		t.Errorf("Len = %d (err %v), want 0", n, err)
	}
}

// TestScheduleMovesFireTime pins the ADR-005 ZSET member contract: ZADD
// of an identical envelope moves its fire time rather than queueing a
// second copy — at most one pending future dispatch per identical
// envelope.
func TestScheduleMovesFireTime(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
	defer cancel()

	q := newTestQueue(t)
	d := newTestDelayed(t, q)
	client := rawClient(t)

	env := minimalEnvelope()
	if err := d.Schedule(ctx, env, t0.Add(time.Second)); err != nil {
		t.Fatalf("first Schedule: %v", err)
	}
	if err := d.Schedule(ctx, env, t0.Add(time.Minute)); err != nil {
		t.Fatalf("second Schedule: %v", err)
	}
	if n, err := d.Len(ctx); err != nil || n != 1 {
		t.Fatalf("Len = %d (err %v), want 1 (identical envelope must not queue twice)", n, err)
	}

	// At the original fire time nothing is due — the fire time moved.
	res, err := d.PromoteDue(ctx, t0.Add(time.Second), 16)
	if err != nil {
		t.Fatalf("PromoteDue at original fire time: %v", err)
	}
	if res.Promoted != 0 {
		t.Errorf("promoted %d at the original fire time, want 0 (fire time moved to t0+1m)", res.Promoted)
	}
	res, err = d.PromoteDue(ctx, t0.Add(time.Minute), 16)
	if err != nil {
		t.Fatalf("PromoteDue at moved fire time: %v", err)
	}
	if res.Promoted != 1 {
		t.Errorf("promoted %d at the moved fire time, want 1", res.Promoted)
	}
	if envs := streamEnvelopes(ctx, t, client, q); len(envs) != 1 || envs[0] != env {
		t.Errorf("stream = %+v, want exactly one copy of %+v", envs, env)
	}
}

// TestPromoteDueBatchBound: one pass promotes at most limit entries,
// oldest first; the rest stay for later passes.
func TestPromoteDueBatchBound(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
	defer cancel()

	q := newTestQueue(t)
	d := newTestDelayed(t, q)
	client := rawClient(t)

	const n = 10
	for i := range n {
		if err := d.Schedule(ctx, envelopeForStep(fmt.Sprintf("step-%d", i)), t0.Add(time.Duration(i)*time.Millisecond)); err != nil {
			t.Fatalf("Schedule %d: %v", i, err)
		}
	}
	res, err := d.PromoteDue(ctx, t0.Add(time.Second), 4)
	if err != nil {
		t.Fatalf("PromoteDue: %v", err)
	}
	if res.Promoted != 4 {
		t.Errorf("Promoted = %d, want the limit 4", res.Promoted)
	}
	if remaining, err := d.Len(ctx); err != nil || remaining != n-4 {
		t.Errorf("Len = %d (err %v), want %d", remaining, err, n-4)
	}
	envs := streamEnvelopes(ctx, t, client, q)
	if len(envs) != 4 {
		t.Fatalf("stream holds %d entries, want 4", len(envs))
	}
	for i, env := range envs {
		if want := fmt.Sprintf("step-%d", i); env.StepID != want {
			t.Errorf("promoted entry %d is %q, want %q (oldest first)", i, env.StepID, want)
		}
	}
}

// TestPromoteDueConcurrent is acceptance criterion 2: the Lua move is
// atomic, so concurrent promoters racing over the same due set neither
// lose nor duplicate entries.
func TestPromoteDueConcurrent(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
	defer cancel()

	q := newTestQueue(t)
	d := newTestDelayed(t, q)
	client := rawClient(t)

	const n = 200
	for i := range n {
		if err := d.Schedule(ctx, envelopeForStep(fmt.Sprintf("step-%d", i)), t0); err != nil {
			t.Fatalf("Schedule %d: %v", i, err)
		}
	}

	// Every entry is due, so a pass promoting zero means the set is empty
	// and the promoter can stop.
	const promoters = 8
	var totalPromoted atomic.Int64
	var wg sync.WaitGroup
	errs := make(chan error, promoters)
	for range promoters {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				res, err := d.PromoteDue(ctx, t0.Add(time.Second), 7)
				if err != nil {
					errs <- err
					return
				}
				if res.Quarantined != 0 {
					errs <- fmt.Errorf("unexpected quarantined count %d", res.Quarantined)
					return
				}
				if res.Promoted == 0 {
					return
				}
				totalPromoted.Add(int64(res.Promoted))
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent PromoteDue: %v", err)
	}

	if got := totalPromoted.Load(); got != n {
		t.Errorf("promoters reported %d promotions in total, want %d (no loss, no duplication)", got, n)
	}
	seen := make(map[string]int)
	for _, env := range streamEnvelopes(ctx, t, client, q) {
		seen[env.StepID]++
	}
	if len(seen) != n {
		t.Errorf("stream holds %d distinct steps, want %d", len(seen), n)
	}
	for step, count := range seen {
		if count != 1 {
			t.Errorf("step %s promoted %d times, want exactly once", step, count)
		}
	}
	if remaining, err := d.Len(ctx); err != nil || remaining != 0 {
		t.Errorf("Len = %d (err %v), want 0 at quiescence", remaining, err)
	}
}

// TestPromoteDueQuarantinesMalformed: a member the script cannot decode
// into XADD args is moved to the quarantine list with contents preserved
// — never silently dropped, and never left wedging the batch window
// (its due score would otherwise re-select it first on every tick).
func TestPromoteDueQuarantinesMalformed(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
	defer cancel()

	q := newTestQueue(t)
	d := newTestDelayed(t, q)
	client := rawClient(t)

	// Raw malformed members, all scored earlier than the valid entry so
	// they occupy the front of the batch window.
	malformed := []string{"not json", "{}", `["run_id","x"]`}
	for i, member := range malformed {
		if err := client.ZAdd(ctx, d.Key(), redis.Z{
			Score:  float64(t0.Add(time.Duration(i-10) * time.Second).UnixMilli()),
			Member: member,
		}).Err(); err != nil {
			t.Fatalf("raw ZADD %q: %v", member, err)
		}
	}
	valid := envelopeForStep("survivor")
	if err := d.Schedule(ctx, valid, t0.Add(-time.Second)); err != nil {
		t.Fatalf("Schedule valid: %v", err)
	}

	res, err := d.PromoteDue(ctx, t0, 16)
	if err != nil {
		t.Fatalf("PromoteDue: %v", err)
	}
	if res.Promoted != 1 || res.Quarantined != len(malformed) {
		t.Errorf("PromoteDue = %+v, want Promoted 1, Quarantined %d", res, len(malformed))
	}
	if envs := streamEnvelopes(ctx, t, client, q); len(envs) != 1 || envs[0] != valid {
		t.Errorf("stream = %+v, want exactly the valid envelope", envs)
	}
	quarantined, err := client.LRange(ctx, d.MalformedKey(), 0, -1).Result()
	if err != nil {
		t.Fatalf("LRANGE %s: %v", d.MalformedKey(), err)
	}
	if len(quarantined) != len(malformed) {
		t.Fatalf("quarantine holds %d members, want %d", len(quarantined), len(malformed))
	}
	got := make(map[string]bool, len(quarantined))
	for _, member := range quarantined {
		got[member] = true
	}
	for _, member := range malformed {
		if !got[member] {
			t.Errorf("quarantine is missing raw member %q — contents must be preserved", member)
		}
	}
	if remaining, err := d.Len(ctx); err != nil || remaining != 0 {
		t.Errorf("Len = %d (err %v), want 0 (no member left wedging the window)", remaining, err)
	}
}

// TestConsumerPromotesDelayedEntries proves the wiring end to end: a
// consumer's promoter duty moves a due entry onto the stream, and the
// same consumer's read loop delivers it to the handler. Real time here,
// tuned down — the fake-clock coverage is PromoteDue's tests above.
func TestConsumerPromotesDelayedEntries(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
	defer cancel()

	q := newTestQueue(t)
	d := newTestDelayed(t, q)

	want := envelopeForStep("delayed-step")
	if err := d.Schedule(ctx, want, time.Now().Add(-time.Second)); err != nil {
		t.Fatalf("Schedule: %v", err)
	}

	deliveries := make(chan queue.Delivery, 1)
	handler := func(_ context.Context, del queue.Delivery) error {
		deliveries <- del
		return nil
	}
	cfg := fastCfg()
	cfg.PromoterTick = 50 * time.Millisecond
	cfg.DelayedKey = d.Key()
	stop := startConsumer(t, q.NewConsumer("", handler, cfg))

	select {
	case del := <-deliveries:
		if del.Envelope != want || del.DeliveryCount != 1 {
			t.Errorf("delivery = %+v, want envelope %+v with DeliveryCount 1", del, want)
		}
	case <-ctx.Done():
		t.Fatal("promoted entry was never delivered to the handler")
	}
	waitForStats(ctx, t, q, func(s queue.StreamStats) bool { return s.Pending == 0 })
	if n, err := d.Len(ctx); err != nil || n != 0 {
		t.Errorf("Len = %d (err %v), want 0", n, err)
	}
	if err := stop(); err != nil {
		t.Errorf("Run returned %v on clean shutdown, want nil", err)
	}
}
