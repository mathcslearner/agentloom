package queuetest

import (
	"context"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// IsQuiescent probes ticket 3.6's three quiescence invariants — stream
// drained (group lag 0: every entry delivered), PEL empty (every delivery
// acked), delayed set empty — returning a human-readable detail when one
// fails. The quarantine list is deliberately not part of quiescence: a
// chaos test may plant undecodable members on purpose; assert it
// separately via MalformedMembers. Requires the group to exist
// (EnsureGroup, or a spawned consumer's Run, must have created it).
func (h *Harness) IsQuiescent(ctx context.Context) (bool, string) {
	h.tb.Helper()
	groups, err := h.client.XInfoGroups(ctx, h.queue.Stream()).Result()
	if err != nil {
		h.tb.Fatalf("XINFO GROUPS %s: %v", h.queue.Stream(), err)
	}
	found := false
	for _, g := range groups {
		if g.Name != h.queue.Group() {
			continue
		}
		found = true
		if g.Lag != 0 {
			return false, fmt.Sprintf("stream not drained: %d undelivered entries", g.Lag)
		}
	}
	if !found {
		h.tb.Fatalf("consumer group %s does not exist on %s; call EnsureGroup first", h.queue.Group(), h.queue.Stream())
	}
	if stats := h.Stats(ctx); stats.Pending != 0 {
		return false, fmt.Sprintf("PEL not empty: %d pending entries (%+v)", stats.Pending, stats.Consumers)
	}
	if n := h.DelayedLen(ctx); n != 0 {
		return false, fmt.Sprintf("delayed set not empty: %d entries", n)
	}
	return true, ""
}

// WaitQuiescent polls IsQuiescent until it holds; on timeout it dumps
// full diagnostic state and fails the test. This is the reusable
// end-of-scenario assertion for the M4/M5 chaos suites.
func (h *Harness) WaitQuiescent(ctx context.Context) {
	h.tb.Helper()
	var detail string
	deadline := time.Now().Add(opTimeout)
	for time.Now().Before(deadline) && ctx.Err() == nil {
		ok, d := h.IsQuiescent(ctx)
		if ok {
			return
		}
		detail = d
		time.Sleep(20 * time.Millisecond)
	}
	h.DumpDiagnostics(ctx)
	h.tb.Fatalf("queue never reached quiescence: %s", detail)
}

// RequireHandledOncePerClaim asserts the journal shows no entry handled
// twice within one claim (same entry ID and delivery count). Re-handling
// an entry across claims — a reclaim after a crash — is at-least-once
// working as designed and does not fail this check; the claim CAS (2.6)
// is what turns that into effectively-once execution.
func (h *Harness) RequireHandledOncePerClaim() {
	h.tb.Helper()
	for _, dup := range h.journal.duplicateClaims() {
		h.tb.Errorf("delivered more than once per claim: %s", dup)
	}
}

// DumpDiagnostics logs the full queue state — stream stats, the PEL, the
// delayed set with scores, the quarantine list, and the invocation
// journal — for post-mortem when an assertion fails. Best-effort: dump
// errors are logged, never fatal, so the original failure survives.
func (h *Harness) DumpDiagnostics(ctx context.Context) {
	h.tb.Helper()
	if stats, err := h.queue.Stats(ctx); err != nil {
		h.tb.Logf("diagnostics: Stats: %v", err)
	} else {
		h.tb.Logf("diagnostics: stats %+v", stats)
	}
	pelQuery := &redis.XPendingExtArgs{
		Stream: h.queue.Stream(), Group: h.queue.Group(), Start: "-", End: "+", Count: 100,
	}
	if pending, err := h.client.XPendingExt(ctx, pelQuery).Result(); err != nil {
		h.tb.Logf("diagnostics: XPENDING: %v", err)
	} else {
		for _, p := range pending {
			h.tb.Logf("diagnostics: PEL entry %s consumer %s deliveries %d idle %v", p.ID, p.Consumer, p.RetryCount, p.Idle)
		}
	}
	if members, err := h.client.ZRangeWithScores(ctx, h.delayed.Key(), 0, -1).Result(); err != nil {
		h.tb.Logf("diagnostics: ZRANGE %s: %v", h.delayed.Key(), err)
	} else {
		for _, m := range members {
			h.tb.Logf("diagnostics: delayed member %v fire-at %d", m.Member, int64(m.Score))
		}
	}
	if quarantined, err := h.client.LRange(ctx, h.delayed.MalformedKey(), 0, -1).Result(); err != nil {
		h.tb.Logf("diagnostics: LRANGE %s: %v", h.delayed.MalformedKey(), err)
	} else {
		for _, m := range quarantined {
			h.tb.Logf("diagnostics: quarantined member %q", m)
		}
	}
	for _, inv := range h.journal.Invocations() {
		h.tb.Logf("diagnostics: invocation consumer %s entry %s step %s delivery %d err %v panicked %t running %t",
			inv.Consumer, inv.EntryID, inv.Step, inv.DeliveryCount, inv.Err, inv.Panicked, inv.End.IsZero())
	}
}

// WaitJournal polls the journal until cond is satisfied, returning the
// matching snapshot; on timeout it dumps diagnostics and fails. The way
// tests synchronize on "handler started/finished" without threading
// bespoke channels through every scripted handler.
func (h *Harness) WaitJournal(ctx context.Context, cond func([]Invocation) bool) []Invocation {
	h.tb.Helper()
	var invs []Invocation
	deadline := time.Now().Add(opTimeout)
	for time.Now().Before(deadline) && ctx.Err() == nil {
		invs = h.journal.Invocations()
		if cond(invs) {
			return invs
		}
		time.Sleep(10 * time.Millisecond)
	}
	h.DumpDiagnostics(ctx)
	h.tb.Fatalf("journal condition never met; last journal has %d invocations", len(invs))
	return invs
}
