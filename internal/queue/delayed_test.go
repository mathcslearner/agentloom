package queue_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/mathcslearner/agentloom/internal/queue"
)

// TestDelayedMemberDeterministic pins the property ZADD's
// move-the-fire-time dedup semantics rest on: identical envelopes encode
// to byte-identical ZSET members.
func TestDelayedMemberDeterministic(t *testing.T) {
	t.Parallel()

	first, err := queue.EncodeDelayedMember(fullEnvelope())
	if err != nil {
		t.Fatalf("EncodeDelayedMember: %v", err)
	}
	for range 20 {
		again, err := queue.EncodeDelayedMember(fullEnvelope())
		if err != nil {
			t.Fatalf("EncodeDelayedMember: %v", err)
		}
		if again != first {
			t.Fatalf("member encoding is not deterministic: %q vs %q", first, again)
		}
	}
}

// TestDelayedMemberRoundTrips proves a member decodes back to the exact
// envelope: the member is the JSON object of the stream field–value
// pairs, so unmarshaling it yields what XADD would have written and what
// DecodeEnvelope reads off the wire.
func TestDelayedMemberRoundTrips(t *testing.T) {
	t.Parallel()

	want := fullEnvelope()
	member, err := queue.EncodeDelayedMember(want)
	if err != nil {
		t.Fatalf("EncodeDelayedMember: %v", err)
	}
	var values map[string]any
	if err := json.Unmarshal([]byte(member), &values); err != nil {
		t.Fatalf("member %q is not a JSON object: %v", member, err)
	}
	got, err := queue.DecodeEnvelope(values)
	if err != nil {
		t.Fatalf("DecodeEnvelope on round-tripped member: %v", err)
	}
	if got != want {
		t.Errorf("round-tripped envelope = %+v, want %+v", got, want)
	}
}

// TestDelayedMemberRejectsInvalid: validation happens at encode time, so
// a malformed envelope can never reach the ZSET.
func TestDelayedMemberRejectsInvalid(t *testing.T) {
	t.Parallel()

	env := minimalEnvelope()
	env.StepID = ""
	if _, err := queue.EncodeDelayedMember(env); err == nil {
		t.Error("EncodeDelayedMember with missing step_id: want error, got nil")
	}
}

// TestNewDelayedKeyDefaults pins key handling: empty falls back to the
// ADR-005 default, and the quarantine list derives from the key.
func TestNewDelayedKeyDefaults(t *testing.T) {
	t.Parallel()

	q := queue.New(nil, "", "")
	if d := q.NewDelayed(""); d.Key() != queue.DefaultDelayedKey {
		t.Errorf("default key = %q, want %q", d.Key(), queue.DefaultDelayedKey)
	}
	d := q.NewDelayed("custom:delayed")
	if d.Key() != "custom:delayed" {
		t.Errorf("explicit key = %q, want %q", d.Key(), "custom:delayed")
	}
	if got, want := d.MalformedKey(), "custom:delayed:malformed"; got != want {
		t.Errorf("MalformedKey() = %q, want %q", got, want)
	}
}

// TestScheduleGuards: both failure modes are caught before any Redis
// command, so a nil client proves the guard order.
func TestScheduleGuards(t *testing.T) {
	t.Parallel()

	d := queue.New(nil, "", "").NewDelayed("")
	ctx := context.Background()

	bad := minimalEnvelope()
	bad.Reason = ""
	if err := d.Schedule(ctx, bad, time.UnixMilli(1_754_000_000_000)); err == nil {
		t.Error("Schedule with invalid envelope: want error, got nil")
	}
	if err := d.Schedule(ctx, minimalEnvelope(), time.Time{}); err == nil || !strings.Contains(err.Error(), "fireAt") {
		t.Errorf("Schedule with zero fireAt: error = %v, want mention of fireAt", err)
	}
}

// TestPromoteDueRejectsZeroNow mirrors the store layer's zero-Now guard:
// now is the injected clock reading and must be explicit.
func TestPromoteDueRejectsZeroNow(t *testing.T) {
	t.Parallel()

	d := queue.New(nil, "", "").NewDelayed("")
	if _, err := d.PromoteDue(context.Background(), time.Time{}, 1); err == nil || !strings.Contains(err.Error(), "now") {
		t.Errorf("PromoteDue with zero now: error = %v, want mention of now", err)
	}
}
