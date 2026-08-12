package steplog

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync/atomic"

	"github.com/google/uuid"

	"github.com/mathcslearner/agentloom/internal/store"
)

// seqCounter allocates an attempt's per-line sequence numbers. Shared by
// every WithAttrs/WithGroup derivative of the attempt's handler — the
// ring is per attempt, not per derived logger.
type seqCounter = atomic.Int64

// truncationSuffix marks a message cut at MaxLineBytes.
const truncationSuffix = "… (truncated)"

// captureHandler is the tee's durable side: one per step attempt,
// converting slog records into queued lines. It never writes to the
// terminal — the fanout pairs it with the base handler for that.
type captureHandler struct {
	sink    *Sink
	runID   uuid.UUID
	stepID  string
	attempt int32
	traceID string
	seq     *seqCounter

	// attrs are the WithAttrs-accumulated attributes, each remembered with
	// the group path open when it was added; groups is the currently open
	// path for future attrs and the record's own.
	attrs  []pathAttr
	groups []string
}

// pathAttr is one accumulated attribute and the group path it lives under.
type pathAttr struct {
	path []string
	attr slog.Attr
}

// Enabled gates capture on the sink's configured minimum level. Filtered
// records consume no seq — they are invisible, not dropped.
func (c *captureHandler) Enabled(_ context.Context, l slog.Level) bool {
	return l >= c.sink.cfg.Level
}

// Handle converts one record into a line and enqueues it (non-blocking;
// see Sink.enqueue). Always returns nil: capture failure modes are
// dropped lines, never executor-visible errors.
func (c *captureHandler) Handle(_ context.Context, r slog.Record) error {
	root := map[string]any{}
	for _, pa := range c.attrs {
		putAttr(root, pa.path, pa.attr)
	}
	r.Attrs(func(a slog.Attr) bool {
		putAttr(root, c.groups, a)
		return true
	})

	var fields []byte
	if len(root) > 0 {
		b, err := json.Marshal(root)
		if err != nil {
			// attrValue already defuses arbitrary values; this is belt and
			// suspenders for exotic map keys introduced by future edits.
			b, _ = json.Marshal(map[string]any{"_fields_error": err.Error()}) //nolint:errcheck // plain map
		}
		if len(b) > c.sink.cfg.MaxLineBytes {
			b, _ = json.Marshal(map[string]any{"_fields_truncated_bytes": len(b)}) //nolint:errcheck // plain map
		}
		fields = b
	}

	msg := r.Message
	if len(msg) > c.sink.cfg.MaxLineBytes {
		msg = msg[:c.sink.cfg.MaxLineBytes] + truncationSuffix
	}
	at := r.Time
	if at.IsZero() {
		at = c.sink.cfg.Now()
	}
	c.sink.enqueue(line{
		runID: c.runID, stepID: c.stepID, attempt: c.attempt,
		seq:   c.seq.Add(1),
		level: levelString(r.Level), message: msg, fields: fields,
		traceID: c.traceID, loggedAt: at.UTC(),
	})
	return nil
}

// WithAttrs returns a derivative sharing the sink, identity, and seq
// counter, with the new attrs remembered under the open group path.
func (c *captureHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	if len(attrs) == 0 {
		return c
	}
	d := *c
	d.attrs = make([]pathAttr, 0, len(c.attrs)+len(attrs))
	d.attrs = append(d.attrs, c.attrs...)
	for _, a := range attrs {
		d.attrs = append(d.attrs, pathAttr{path: c.groups, attr: a})
	}
	return &d
}

// WithGroup returns a derivative opening one more group level.
func (c *captureHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		return c
	}
	d := *c
	d.groups = make([]string, 0, len(c.groups)+1)
	d.groups = append(d.groups, c.groups...)
	d.groups = append(d.groups, name)
	return &d
}

// putAttr inserts one attribute into the nested fields object at path,
// following slog's group semantics (empty-keyed groups inline; groups
// become nested objects).
func putAttr(root map[string]any, path []string, a slog.Attr) {
	a.Value = a.Value.Resolve()
	if a.Equal(slog.Attr{}) {
		return
	}
	dst := root
	for _, g := range path {
		dst = childMap(dst, g)
	}
	if a.Value.Kind() == slog.KindGroup {
		if a.Key != "" {
			dst = childMap(dst, a.Key)
		}
		for _, ga := range a.Value.Group() {
			putAttr(dst, nil, ga)
		}
		return
	}
	if a.Key == "" {
		return
	}
	dst[a.Key] = attrValue(a.Value)
}

// childMap returns (creating if needed) the nested object at key. A
// non-object already there is displaced — group semantics win.
func childMap(m map[string]any, key string) map[string]any {
	if child, ok := m[key].(map[string]any); ok {
		return child
	}
	child := map[string]any{}
	m[key] = child
	return child
}

// attrValue converts one resolved slog value into a JSON-marshalable
// value. Errors stringify (they marshal uselessly as {}), and anything
// json.Marshal rejects falls back to fmt.Sprint — a log line must never
// fail to capture because of an exotic payload.
func attrValue(v slog.Value) any {
	switch v.Kind() {
	case slog.KindString:
		return v.String()
	case slog.KindInt64:
		return v.Int64()
	case slog.KindUint64:
		return v.Uint64()
	case slog.KindFloat64:
		return v.Float64()
	case slog.KindBool:
		return v.Bool()
	case slog.KindDuration:
		return v.Duration().String()
	case slog.KindTime:
		return v.Time()
	default:
		raw := v.Any()
		if e, ok := raw.(error); ok {
			return e.Error()
		}
		if _, err := json.Marshal(raw); err != nil {
			return fmt.Sprint(raw)
		}
		return raw
	}
}

// levelString canonicalizes any slog level onto the store's closed
// vocabulary (the schema CHECK): the four standard bands, with custom
// levels bucketed by severity.
func levelString(l slog.Level) string {
	switch {
	case l < slog.LevelInfo:
		return store.LogLevelDebug
	case l < slog.LevelWarn:
		return store.LogLevelInfo
	case l < slog.LevelError:
		return store.LogLevelWarn
	default:
		return store.LogLevelError
	}
}

// fanoutHandler tees records to every member handler: the base (terminal)
// handler first, the capture handler second. Enabled when any member is,
// with per-member Enabled re-checked in Handle so each side keeps its own
// level policy.
type fanoutHandler []slog.Handler

func (f fanoutHandler) Enabled(ctx context.Context, l slog.Level) bool {
	for _, h := range f {
		if h.Enabled(ctx, l) {
			return true
		}
	}
	return false
}

func (f fanoutHandler) Handle(ctx context.Context, r slog.Record) error {
	var errs []error
	for _, h := range f {
		if !h.Enabled(ctx, r.Level) {
			continue
		}
		if err := h.Handle(ctx, r.Clone()); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (f fanoutHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	out := make(fanoutHandler, len(f))
	for i, h := range f {
		out[i] = h.WithAttrs(attrs)
	}
	return out
}

func (f fanoutHandler) WithGroup(name string) slog.Handler {
	out := make(fanoutHandler, len(f))
	for i, h := range f {
		out[i] = h.WithGroup(name)
	}
	return out
}
