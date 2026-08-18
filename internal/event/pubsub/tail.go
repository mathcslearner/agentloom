package pubsub

import (
	"context"

	"github.com/google/uuid"

	"github.com/mathcslearner/agentloom/internal/event"
)

// DefaultBackfillPageSize is the page size the Tailer reads backfill rows in.
const DefaultBackfillPageSize = 500

// Backfiller reads a run's committed events after a seq cursor, in seq order —
// the DB range scan that heals a missed or out-of-order pub/sub message
// (ADR-018). The store side implements it over Events().List; keeping it an
// interface keeps this package a leaf (no store import) and lets tests drive the
// Tailer with a scripted backfill.
type Backfiller interface {
	// EventsAfter returns up to limit envelopes of the run with seq > afterSeq,
	// ascending. Fewer than limit means the caller has reached the head.
	EventsAfter(ctx context.Context, runID uuid.UUID, afterSeq int64, limit int32) ([]event.Envelope, error)
}

// Tailer turns the at-least-once, possibly-gapped stream of a single run's live
// envelopes into a gap-free, deduped, in-order delivery (ADR-018). It tracks the
// last delivered seq; a duplicate (seq ≤ last) is dropped, the next (seq =
// last+1) is delivered, and a gap (seq > last+1) triggers a paged DB backfill
// from the cursor before the live envelope is re-evaluated. It is not safe for
// concurrent use — the 16.3 WS server drives one Tailer from one goroutine, and
// the 16.4 firehose keeps one Tailer per run.
type Tailer struct {
	runID    uuid.UUID
	lastSeq  int64
	backfill Backfiller
	deliver  func(event.Envelope)
	pageSize int32
}

// NewTailer builds a Tailer for one run. lastSeq is the client's resume cursor
// (0 for a fresh tail — every event is new). deliver is called once per
// in-order envelope; it must not block indefinitely (the WS server writes to a
// bounded per-connection buffer and closes a slow client). pageSize ≤ 0 uses
// DefaultBackfillPageSize.
func NewTailer(runID uuid.UUID, lastSeq int64, backfill Backfiller, deliver func(event.Envelope), pageSize int32) *Tailer {
	if pageSize <= 0 {
		pageSize = DefaultBackfillPageSize
	}
	return &Tailer{runID: runID, lastSeq: lastSeq, backfill: backfill, deliver: deliver, pageSize: pageSize}
}

// LastSeq is the highest seq delivered so far (the resume cursor).
func (t *Tailer) LastSeq() int64 { return t.lastSeq }

// Catchup backfills from the current cursor to the run's head, delivering every
// not-yet-seen event in order. The WS server calls it once after subscribing and
// loading the snapshot, so the client is current before the live tail begins; a
// gap during the live tail calls it again. It is idempotent: a second call with
// no new rows delivers nothing.
func (t *Tailer) Catchup(ctx context.Context) error {
	for {
		rows, err := t.backfill.EventsAfter(ctx, t.runID, t.lastSeq, t.pageSize)
		if err != nil {
			return err
		}
		for _, env := range rows {
			// Deliver strictly in-order; skip anything at/behind the cursor.
			if env.Seq == t.lastSeq+1 {
				t.deliver(env)
				t.lastSeq = env.Seq
			} else if env.Seq > t.lastSeq {
				// A hole inside the backfill page (should not happen — seq is
				// gap-free) — deliver it anyway and advance, since the DB is the
				// truth.
				t.deliver(env)
				t.lastSeq = env.Seq
			}
		}
		if len(rows) < int(t.pageSize) {
			return nil
		}
	}
}

// Offer processes one live envelope with gap detection and dedupe. It ignores an
// envelope for another run (a firehose message a per-run Tailer should not see),
// drops a duplicate, delivers the next in-order envelope, and on a gap backfills
// to the head first — after which the live envelope is delivered if it is now
// next, or dropped as already-backfilled. A backfill error is returned; the
// caller (16.3) may retry on the next message, since Catchup re-reads to head.
func (t *Tailer) Offer(ctx context.Context, env event.Envelope) error {
	if env.RunID != t.runID {
		return nil
	}
	switch {
	case env.Seq <= t.lastSeq:
		// Duplicate — already delivered.
		return nil
	case env.Seq == t.lastSeq+1:
		t.deliver(env)
		t.lastSeq = env.Seq
		return nil
	default:
		// Gap: backfill to head, then re-evaluate the live envelope.
		if err := t.Catchup(ctx); err != nil {
			return err
		}
		if env.Seq == t.lastSeq+1 {
			t.deliver(env)
			t.lastSeq = env.Seq
		}
		// Otherwise env was covered by the backfill (drop) or is still ahead of
		// the visible head (a later message re-triggers Catchup, which reads to
		// head and picks it up) — either way, no gap escapes.
		return nil
	}
}
