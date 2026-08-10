package queue_test

import (
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"pgregory.net/rapid"

	"github.com/mathcslearner/agentloom/internal/queue"
)

func fullEnvelope() queue.Envelope {
	return queue.Envelope{
		RunID:       uuid.MustParse("5f0c19a1-9c2b-4f8d-a1e0-3b7f8e2d4c6a"),
		StepID:      "summarize#3",
		Reason:      queue.ReasonStepReady,
		TraceParent: "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01",
		TraceState:  "vendor=value",
		EnqueuedAt:  time.UnixMilli(1754870400123).UTC(),
	}
}

func minimalEnvelope() queue.Envelope {
	return queue.Envelope{
		RunID:  uuid.MustParse("0b8ad2c4-6f1e-4d3a-9c5b-7e2f4a6d8c0b"),
		StepID: "fetch",
		Reason: queue.ReasonStepReady,
	}
}

func TestEnvelopeRoundTrip(t *testing.T) {
	t.Parallel()

	for name, env := range map[string]queue.Envelope{"full": fullEnvelope(), "minimal": minimalEnvelope()} {
		values, err := env.Encode()
		if err != nil {
			t.Fatalf("%s: Encode: unexpected error: %v", name, err)
		}
		got, err := queue.DecodeEnvelope(values)
		if err != nil {
			t.Fatalf("%s: DecodeEnvelope: unexpected error: %v", name, err)
		}
		if got != env {
			t.Errorf("%s: round-trip = %+v, want %+v", name, got, env)
		}
	}
}

func TestEncodeOmitsEmptyOptionalFields(t *testing.T) {
	t.Parallel()

	values, err := minimalEnvelope().Encode()
	if err != nil {
		t.Fatalf("Encode: unexpected error: %v", err)
	}
	for _, key := range []string{"traceparent", "tracestate", "enqueued_at_ms"} {
		if _, ok := values[key]; ok {
			t.Errorf("Encode wrote optional field %q for an empty value", key)
		}
	}
	if got := values["v"]; got != strconv.Itoa(queue.EnvelopeVersion) {
		t.Errorf("v = %v, want %q", got, strconv.Itoa(queue.EnvelopeVersion))
	}
}

func TestEncodeRejectsMissingRequiredFields(t *testing.T) {
	t.Parallel()

	cases := map[string]queue.Envelope{
		"run_id":  {StepID: "fetch", Reason: queue.ReasonStepReady},
		"step_id": {RunID: uuid.New(), Reason: queue.ReasonStepReady},
		"reason":  {RunID: uuid.New(), StepID: "fetch"},
	}
	for field, env := range cases {
		if _, err := env.Encode(); err == nil {
			t.Errorf("Encode without %s: want error, got nil", field)
		} else if !strings.Contains(err.Error(), field) {
			t.Errorf("Encode without %s: error %q does not name the field", field, err)
		}
	}
}

func TestDecodeUnknownVersion(t *testing.T) {
	t.Parallel()

	values, err := minimalEnvelope().Encode()
	if err != nil {
		t.Fatalf("Encode: unexpected error: %v", err)
	}
	values["v"] = "2"
	_, err = queue.DecodeEnvelope(values)
	if !errors.Is(err, queue.ErrBadEnvelope) {
		t.Fatalf("errors.Is(err, ErrBadEnvelope) = false for %v", err)
	}
	var uve *queue.UnknownVersionError
	if !errors.As(err, &uve) {
		t.Fatalf("want *UnknownVersionError, got %T: %v", err, err)
	}
	if uve.Version != 2 {
		t.Errorf("Version = %d, want 2", uve.Version)
	}
}

func TestDecodeMalformed(t *testing.T) {
	t.Parallel()

	base := func() map[string]any {
		values, err := fullEnvelope().Encode()
		if err != nil {
			t.Fatalf("Encode: unexpected error: %v", err)
		}
		return values
	}
	cases := map[string]struct {
		mutate    func(map[string]any)
		wantField string
	}{
		"missing v":              {func(v map[string]any) { delete(v, "v") }, "v"},
		"empty v":                {func(v map[string]any) { v["v"] = "" }, "v"},
		"non-integer v":          {func(v map[string]any) { v["v"] = "one" }, "v"},
		"non-string v":           {func(v map[string]any) { v["v"] = 1 }, "v"},
		"missing run_id":         {func(v map[string]any) { delete(v, "run_id") }, "run_id"},
		"bad run_id":             {func(v map[string]any) { v["run_id"] = "not-a-uuid" }, "run_id"},
		"missing step_id":        {func(v map[string]any) { delete(v, "step_id") }, "step_id"},
		"empty step_id":          {func(v map[string]any) { v["step_id"] = "" }, "step_id"},
		"missing reason":         {func(v map[string]any) { delete(v, "reason") }, "reason"},
		"bad enqueued_at_ms":     {func(v map[string]any) { v["enqueued_at_ms"] = "soon" }, "enqueued_at_ms"},
		"non-integer enqueue ms": {func(v map[string]any) { v["enqueued_at_ms"] = "12.5" }, "enqueued_at_ms"},
	}
	for name, tc := range cases {
		values := base()
		tc.mutate(values)
		_, err := queue.DecodeEnvelope(values)
		if !errors.Is(err, queue.ErrBadEnvelope) {
			t.Errorf("%s: errors.Is(err, ErrBadEnvelope) = false for %v", name, err)
			continue
		}
		var mal *queue.MalformedEnvelopeError
		if !errors.As(err, &mal) {
			t.Errorf("%s: want *MalformedEnvelopeError, got %T: %v", name, err, err)
			continue
		}
		if mal.Field != tc.wantField {
			t.Errorf("%s: Field = %q, want %q", name, mal.Field, tc.wantField)
		}
	}
}

// TestDecodeIgnoresUnknownFields pins the additive-evolution rule from
// ADR-005: within a version, decoders MUST ignore fields they do not
// recognize.
func TestDecodeIgnoresUnknownFields(t *testing.T) {
	t.Parallel()

	env := minimalEnvelope()
	values, err := env.Encode()
	if err != nil {
		t.Fatalf("Encode: unexpected error: %v", err)
	}
	values["future_field"] = "whatever"
	got, err := queue.DecodeEnvelope(values)
	if err != nil {
		t.Fatalf("DecodeEnvelope with unknown field: unexpected error: %v", err)
	}
	if got != env {
		t.Errorf("decoded = %+v, want %+v", got, env)
	}
}

// TestDecodeAcceptsUnknownReason pins that the reason vocabulary is not
// enforced at decode time: new reasons within version 1 are additive, and
// enforcing an enum here would make every future reason a breaking change.
func TestDecodeAcceptsUnknownReason(t *testing.T) {
	t.Parallel()

	env := minimalEnvelope()
	env.Reason = "retry_dispatch" // an M5.2-era reason this decoder predates
	values, err := env.Encode()
	if err != nil {
		t.Fatalf("Encode: unexpected error: %v", err)
	}
	got, err := queue.DecodeEnvelope(values)
	if err != nil {
		t.Fatalf("DecodeEnvelope: unexpected error: %v", err)
	}
	if got.Reason != "retry_dispatch" {
		t.Errorf("Reason = %q, want %q", got.Reason, "retry_dispatch")
	}
}

func TestEnvelopeRoundTripProperty(t *testing.T) {
	t.Parallel()

	rapid.Check(t, func(t *rapid.T) {
		env := queue.Envelope{
			RunID:       uuid.Must(uuid.FromBytes(rapid.SliceOfN(rapid.Byte(), 16, 16).Draw(t, "run_id"))),
			StepID:      rapid.StringMatching(`[a-zA-Z0-9_#-]{1,32}`).Draw(t, "step_id"),
			Reason:      rapid.StringMatching(`[a-z_]{1,24}`).Draw(t, "reason"),
			TraceParent: rapid.OneOf(rapid.Just(""), rapid.StringMatching(`[0-9a-f-]{8,55}`)).Draw(t, "traceparent"),
			TraceState:  rapid.OneOf(rapid.Just(""), rapid.StringMatching(`[a-z=,]{1,16}`)).Draw(t, "tracestate"),
		}
		if rapid.Bool().Draw(t, "with_enqueued_at") {
			// UnixMilli truncates below the millisecond, so generate whole
			// milliseconds; the codec's contract is ms precision.
			env.EnqueuedAt = time.UnixMilli(rapid.Int64Range(0, 1<<45).Draw(t, "enqueued_ms")).UTC()
		}
		if env.RunID == uuid.Nil {
			t.Skip("nil UUID is invalid by contract")
		}
		values, err := env.Encode()
		if err != nil {
			t.Fatalf("Encode: %v", err)
		}
		got, err := queue.DecodeEnvelope(values)
		if err != nil {
			t.Fatalf("DecodeEnvelope: %v", err)
		}
		if got != env {
			t.Fatalf("round-trip = %+v, want %+v", got, env)
		}
	})
}

func TestNewConsumerName(t *testing.T) {
	t.Parallel()

	seen := make(map[string]bool)
	for range 64 {
		name := queue.NewConsumerName()
		if name == "" || strings.ContainsAny(name, " \t\n") {
			t.Fatalf("consumer name %q is empty or contains whitespace", name)
		}
		if seen[name] {
			t.Fatalf("consumer name %q repeated — names must be unique per incarnation", name)
		}
		seen[name] = true
	}
}
