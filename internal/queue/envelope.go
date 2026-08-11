package queue

import (
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/google/uuid"
)

// EnvelopeVersion is the envelope format this codec speaks. ADR-005 defines
// version 1; evolution within a version is additive (decoders ignore
// unknown fields), and any incompatible change bumps this constant together
// with the consumers-decode-before-producers-emit deployment rule.
const EnvelopeVersion = 1

// Envelope field names on the wire (flat Redis Stream field–value pairs,
// per ADR-005 — never a JSON blob, so entries stay inspectable with XRANGE
// during an incident).
const (
	fieldVersion      = "v"
	fieldRunID        = "run_id"
	fieldStepID       = "step_id"
	fieldReason       = "reason"
	fieldTraceParent  = "traceparent"
	fieldTraceState   = "tracestate"
	fieldEnqueuedAtMS = "enqueued_at_ms"
)

// Enqueue-reason vocabulary. Future reasons (DLQ requeue, unpark,
// approval timeout) land with their owning milestones; adding one is
// additive within version 1, which is why Decode checks presence but not
// vocabulary.
const (
	// ReasonStepReady: the step became ready for execution.
	ReasonStepReady = "step_ready"
	// ReasonRetry: a retry re-dispatch scheduled through the delayed set
	// (ticket 5.2, ADR-006). Retry envelopes are built without EnqueuedAt
	// so identical (run, step) retries encode to byte-identical delayed
	// members — ZADD's move-the-fire-time dedup (ADR-005).
	ReasonRetry = "retry"
)

// Envelope is a task message: a pointer to durable state, never a payload
// (ADR-005). It names which step to look at; Postgres holds everything
// else, so duplicates are boring (one read, ack-and-drop) and redelivered
// messages can never carry stale instructions.
type Envelope struct {
	// RunID identifies the run. Required.
	RunID uuid.UUID
	// StepID identifies the step within the run. Opaque to the queue layer
	// (it may carry an `{id}#k` instance suffix once M13/M14 expansion
	// exists). Required.
	StepID string
	// Reason is the enqueue reason carried through from task_outbox
	// (ADR-004). Required. The vocabulary is small and closed, so it is
	// safe as a metric label — unlike RunID/StepID, which never are.
	Reason string
	// TraceParent and TraceState are the W3C trace context, reserved for
	// M7; absent until then.
	TraceParent string
	TraceState  string
	// EnqueuedAt is the producer's injected-clock time at enqueue.
	// Informational only — it feeds dispatch-latency metrics (M7) and is
	// never an input to logic, because producer and consumer clocks are
	// not comparable. The zero value omits the field.
	EnqueuedAt time.Time
}

// validate reports the first missing required field. Encode calls it so a
// malformed envelope can never reach the wire.
func (e Envelope) validate() error {
	if e.RunID == uuid.Nil {
		return errors.New("queue: envelope run_id is required")
	}
	if e.StepID == "" {
		return errors.New("queue: envelope step_id is required")
	}
	if e.Reason == "" {
		return errors.New("queue: envelope reason is required")
	}
	return nil
}

// Encode renders the envelope as stream field–value pairs, always stamping
// the current version. Optional fields are omitted when empty, not written
// as empty strings.
func (e Envelope) Encode() (map[string]any, error) {
	if err := e.validate(); err != nil {
		return nil, err
	}
	values := map[string]any{
		fieldVersion: strconv.Itoa(EnvelopeVersion),
		fieldRunID:   e.RunID.String(),
		fieldStepID:  e.StepID,
		fieldReason:  e.Reason,
	}
	if e.TraceParent != "" {
		values[fieldTraceParent] = e.TraceParent
	}
	if e.TraceState != "" {
		values[fieldTraceState] = e.TraceState
	}
	if !e.EnqueuedAt.IsZero() {
		values[fieldEnqueuedAtMS] = strconv.FormatInt(e.EnqueuedAt.UnixMilli(), 10)
	}
	return values, nil
}

// DecodeEnvelope parses stream field–value pairs (the shape go-redis
// returns in XMessage.Values) into an Envelope. Unknown fields are ignored
// — that is the additive-evolution rule from ADR-005. Failures unwrap to
// ErrBadEnvelope: an *UnknownVersionError for a well-formed version this
// decoder does not speak, a *MalformedEnvelopeError for everything else
// (including a missing or non-integer version).
func DecodeEnvelope(values map[string]any) (Envelope, error) {
	raw, ok := stringField(values, fieldVersion)
	if !ok {
		return Envelope{}, &MalformedEnvelopeError{Field: fieldVersion, cause: errMissing}
	}
	v, err := strconv.Atoi(raw)
	if err != nil {
		return Envelope{}, &MalformedEnvelopeError{Field: fieldVersion, cause: fmt.Errorf("not an integer: %q", raw)}
	}
	if v != EnvelopeVersion {
		return Envelope{}, &UnknownVersionError{Version: v}
	}

	var env Envelope
	raw, ok = stringField(values, fieldRunID)
	if !ok {
		return Envelope{}, &MalformedEnvelopeError{Field: fieldRunID, cause: errMissing}
	}
	if env.RunID, err = uuid.Parse(raw); err != nil {
		return Envelope{}, &MalformedEnvelopeError{Field: fieldRunID, cause: err}
	}
	if env.StepID, ok = stringField(values, fieldStepID); !ok {
		return Envelope{}, &MalformedEnvelopeError{Field: fieldStepID, cause: errMissing}
	}
	if env.Reason, ok = stringField(values, fieldReason); !ok {
		return Envelope{}, &MalformedEnvelopeError{Field: fieldReason, cause: errMissing}
	}
	env.TraceParent, _ = stringField(values, fieldTraceParent)
	env.TraceState, _ = stringField(values, fieldTraceState)
	if raw, ok = stringField(values, fieldEnqueuedAtMS); ok {
		ms, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			return Envelope{}, &MalformedEnvelopeError{Field: fieldEnqueuedAtMS, cause: fmt.Errorf("not an integer: %q", raw)}
		}
		env.EnqueuedAt = time.UnixMilli(ms).UTC()
	}
	return env, nil
}

var errMissing = errors.New("missing or empty")

// stringField extracts a non-empty string field. Stream values arrive from
// go-redis as strings; any other type means the entry was not written by
// this codec and counts as absent.
func stringField(values map[string]any, key string) (string, bool) {
	v, ok := values[key]
	if !ok {
		return "", false
	}
	s, ok := v.(string)
	if !ok || s == "" {
		return "", false
	}
	return s, true
}
