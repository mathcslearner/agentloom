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
	"github.com/mathcslearner/agentloom/internal/queue/queuetest"
)

// t0 is an arbitrary fixed instant: the promoter's now is caller-supplied
// and the Lua script never reads Redis server time, so these tests drive
// promotion with fully synthetic clocks.
var t0 = time.UnixMilli(1_754_000_000_000).UTC()

// TestPromoteDueFakeClock is acceptance criterion 1 (and 3): with a fully
// synthetic now, due entries promote within one pass with an exact
// promotion lag, and not-yet-due entries never promote.
func TestPromoteDueFakeClock(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
	defer cancel()

	h := queuetest.New(t)
	d := h.Delayed()

	due := envelopeForStep("due")
	future := envelopeForStep("future")
	if err := d.Schedule(ctx, due, t0.Add(-time.Second)); err != nil {
		t.Fatalf("Schedule due: %v", err)
	}
	if err := d.Schedule(ctx, future, t0.Add(10*time.Second)); err != nil {
		t.Fatalf("Schedule future: %v", err)
	}

	res := h.PromoteDue(ctx, t0, 16)
	if res.Promoted != 1 || res.Quarantined != 0 {
		t.Errorf("PromoteDue at t0 = %+v, want Promoted 1, Quarantined 0", res)
	}
	if res.MaxLag != time.Second {
		t.Errorf("MaxLag = %v, want exactly 1s (now − fireAt)", res.MaxLag)
	}
	envs := h.StreamEnvelopes(ctx)
	if len(envs) != 1 || envs[0] != due {
		t.Errorf("stream after promotion = %+v, want exactly the due envelope %+v", envs, due)
	}
	if n := h.DelayedLen(ctx); n != 1 {
		t.Errorf("Len = %d, want 1 (the future entry stays)", n)
	}

	// A second pass at the same now must find nothing: due entries promote
	// once, not-yet-due entries never promote.
	res = h.PromoteDue(ctx, t0, 16)
	if res.Promoted != 0 || res.MaxLag != 0 {
		t.Errorf("second pass at t0 = %+v, want nothing promoted", res)
	}

	// Advance the fake clock past the future entry's fire time.
	res = h.PromoteDue(ctx, t0.Add(10*time.Second), 16)
	if res.Promoted != 1 || res.MaxLag != 0 {
		t.Errorf("pass at t0+10s = %+v, want Promoted 1 with zero lag (promoted exactly on time)", res)
	}
	if envs := h.StreamEnvelopes(ctx); len(envs) != 2 || envs[1] != future {
		t.Errorf("stream after second promotion = %+v, want [due, future]", envs)
	}
	if n := h.DelayedLen(ctx); n != 0 {
		t.Errorf("Len = %d, want 0", n)
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

	h := queuetest.New(t)
	d := h.Delayed()

	env := minimalEnvelope()
	if err := d.Schedule(ctx, env, t0.Add(time.Second)); err != nil {
		t.Fatalf("first Schedule: %v", err)
	}
	if err := d.Schedule(ctx, env, t0.Add(time.Minute)); err != nil {
		t.Fatalf("second Schedule: %v", err)
	}
	if n := h.DelayedLen(ctx); n != 1 {
		t.Fatalf("Len = %d, want 1 (identical envelope must not queue twice)", n)
	}

	// At the original fire time nothing is due — the fire time moved.
	res := h.PromoteDue(ctx, t0.Add(time.Second), 16)
	if res.Promoted != 0 {
		t.Errorf("promoted %d at the original fire time, want 0 (fire time moved to t0+1m)", res.Promoted)
	}
	res = h.PromoteDue(ctx, t0.Add(time.Minute), 16)
	if res.Promoted != 1 {
		t.Errorf("promoted %d at the moved fire time, want 1", res.Promoted)
	}
	if envs := h.StreamEnvelopes(ctx); len(envs) != 1 || envs[0] != env {
		t.Errorf("stream = %+v, want exactly one copy of %+v", envs, env)
	}
}

// TestCancelRemovesScheduledMember (ticket 15.4): Cancel ZREMs the exact
// member Schedule added, so the early-decision cleanup empties the delayed set;
// cancelling an absent member is a no-op (the stale-but-harmless case).
func TestCancelRemovesScheduledMember(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
	defer cancel()

	h := queuetest.New(t)
	d := h.Delayed()
	env := minimalEnvelope()

	if err := d.Schedule(ctx, env, t0.Add(time.Hour)); err != nil {
		t.Fatalf("Schedule: %v", err)
	}
	if n := h.DelayedLen(ctx); n != 1 {
		t.Fatalf("Len after Schedule = %d, want 1", n)
	}
	if err := d.Cancel(ctx, env); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	if n := h.DelayedLen(ctx); n != 0 {
		t.Errorf("Len after Cancel = %d, want 0", n)
	}
	// Cancelling again (or an unscheduled member) is a no-op, not an error.
	if err := d.Cancel(ctx, env); err != nil {
		t.Errorf("Cancel of absent member: %v, want nil (no-op)", err)
	}
}

// TestPromoteDueBatchBound: one pass promotes at most limit entries,
// oldest first; the rest stay for later passes.
func TestPromoteDueBatchBound(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
	defer cancel()

	h := queuetest.New(t)
	d := h.Delayed()

	const n = 10
	for i := range n {
		if err := d.Schedule(ctx, envelopeForStep(fmt.Sprintf("step-%d", i)), t0.Add(time.Duration(i)*time.Millisecond)); err != nil {
			t.Fatalf("Schedule %d: %v", i, err)
		}
	}
	res := h.PromoteDue(ctx, t0.Add(time.Second), 4)
	if res.Promoted != 4 {
		t.Errorf("Promoted = %d, want the limit 4", res.Promoted)
	}
	if remaining := h.DelayedLen(ctx); remaining != n-4 {
		t.Errorf("Len = %d, want %d", remaining, n-4)
	}
	envs := h.StreamEnvelopes(ctx)
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

	h := queuetest.New(t)
	d := h.Delayed()

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
	for _, env := range h.StreamEnvelopes(ctx) {
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
	if remaining := h.DelayedLen(ctx); remaining != 0 {
		t.Errorf("Len = %d, want 0 at quiescence", remaining)
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

	h := queuetest.New(t)
	d := h.Delayed()

	// Raw malformed members, all scored earlier than the valid entry so
	// they occupy the front of the batch window.
	malformed := []string{"not json", "{}", `["run_id","x"]`}
	for i, member := range malformed {
		if err := h.Client().ZAdd(ctx, d.Key(), redis.Z{
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

	res := h.PromoteDue(ctx, t0, 16)
	if res.Promoted != 1 || res.Quarantined != len(malformed) {
		t.Errorf("PromoteDue = %+v, want Promoted 1, Quarantined %d", res, len(malformed))
	}
	if envs := h.StreamEnvelopes(ctx); len(envs) != 1 || envs[0] != valid {
		t.Errorf("stream = %+v, want exactly the valid envelope", envs)
	}
	quarantined := h.MalformedMembers(ctx)
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
	if remaining := h.DelayedLen(ctx); remaining != 0 {
		t.Errorf("Len = %d, want 0 (no member left wedging the window)", remaining)
	}
}

// TestConsumerPromotesDelayedEntries proves the wiring end to end: a
// consumer's promoter duty moves a due entry onto the stream, and the
// same consumer's read loop delivers it to the handler. Real time here,
// tuned down — the fake-clock coverage is PromoteDue's tests above. The
// consumer's DelayedKey is left empty: Spawn defaulting it to the
// harness's isolated delayed set is part of the contract under test.
func TestConsumerPromotesDelayedEntries(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
	defer cancel()

	h := queuetest.New(t)

	want := envelopeForStep("delayed-step")
	if err := h.Delayed().Schedule(ctx, want, time.Now().Add(-time.Second)); err != nil {
		t.Fatalf("Schedule: %v", err)
	}

	deliveries := make(chan queue.Delivery, 1)
	handler := func(_ context.Context, del queue.Delivery) error {
		deliveries <- del
		return nil
	}
	cfg := queuetest.FastConfig()
	cfg.PromoterTick = 50 * time.Millisecond
	c := h.Spawn("", handler, cfg)

	select {
	case del := <-deliveries:
		if del.Envelope != want || del.DeliveryCount != 1 {
			t.Errorf("delivery = %+v, want envelope %+v with DeliveryCount 1", del, want)
		}
	case <-ctx.Done():
		t.Fatal("promoted entry was never delivered to the handler")
	}
	h.WaitQuiescent(ctx)
	if err := c.Kill(); err != nil {
		t.Errorf("Run returned %v on clean shutdown, want nil", err)
	}
}
