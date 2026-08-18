package event

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"
)

// TestParseEnvelopeRoundTrip marshals a fresh envelope of every catalog type and
// parses it back, checking the outer fields, the payload type, and the lifted
// step id survive the wire. This is the property the pub/sub read path (16.2)
// relies on: a published message parses back into the same typed envelope the
// store projected on commit.
func TestParseEnvelopeRoundTrip(t *testing.T) {
	t.Parallel()

	runID := uuid.New()
	ts := time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC)
	for i, e := range Catalog {
		e := e
		seq := int64(i + 1)
		t.Run(string(e.Type), func(t *testing.T) {
			t.Parallel()
			orig := NewEnvelope(runID, seq, ts, e.New())
			raw, err := json.Marshal(orig)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			got, err := ParseEnvelope(raw)
			if err != nil {
				t.Fatalf("ParseEnvelope: %v", err)
			}
			if got.SchemaVersion != SchemaVersion {
				t.Errorf("schema_version = %d, want %d", got.SchemaVersion, SchemaVersion)
			}
			if got.RunID != runID || got.Seq != seq || got.Type != e.Type {
				t.Errorf("outer fields = (%s, %d, %q), want (%s, %d, %q)", got.RunID, got.Seq, got.Type, runID, seq, e.Type)
			}
			if !got.Ts.Equal(ts) {
				t.Errorf("ts = %s, want %s", got.Ts, ts)
			}
			if reflect.TypeOf(got.Payload) != reflect.TypeOf(orig.Payload) {
				t.Errorf("payload type = %T, want %T", got.Payload, orig.Payload)
			}
			if got.StepID != orig.StepID {
				t.Errorf("step_id = %q, want %q", got.StepID, orig.StepID)
			}
		})
	}
}

// TestParseEnvelopeRejects covers the three error paths a live subscriber drops
// (and heals by DB backfill): malformed JSON, an unknown envelope schema
// version, and an unknown payload type.
func TestParseEnvelopeRejects(t *testing.T) {
	t.Parallel()

	cases := map[string]string{
		"malformed json":  `{not json`,
		"unknown version": `{"schema_version":999,"run_id":"` + uuid.Nil.String() + `","seq":1,"type":"run_created","ts":"2026-08-18T12:00:00Z","payload":{}}`,
		"unknown type":    `{"schema_version":1,"run_id":"` + uuid.Nil.String() + `","seq":1,"type":"no_such_event","ts":"2026-08-18T12:00:00Z","payload":{}}`,
	}
	for name, raw := range cases {
		raw := raw
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if _, err := ParseEnvelope([]byte(raw)); err == nil {
				t.Fatalf("ParseEnvelope(%s): want error, got nil", name)
			}
		})
	}
}
