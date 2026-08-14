// Package tools defines agentloom's tool SPI (ticket 8.7, ADR-009): the
// Tool interface a `tool` step invokes, the typed Registry the exec
// package wraps the tool executor around, and the two built-in tools
// (http_request, json_transform).
//
// A tool is a single, named, schema-described capability the engine can
// call from a `tool` step: it declares the JSON Schema of its arguments
// (generated from the same Go struct it decodes, per the ADR-003
// invariant), the framework validates a step's rendered args against that
// schema BEFORE Invoke runs (bad args → permanent failure, never a call),
// and Invoke performs exactly one capability call returning a JSON result.
//
// Like internal/llm this is a leaf package: it imports internal/plugin
// (the manifest vocabulary) and internal/dag (the ADR-006 error-class
// vocabulary) and stdlib/gojq/jsonschema, never internal/exec or the
// engine. The exec package owns the `tool` step executor (toolexec.go)
// that renders config, looks a tool up here, validates, and invokes.
package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	invopop "github.com/invopop/jsonschema"

	"github.com/mathcslearner/agentloom/internal/plugin"
)

// Tool is one named capability a `tool` step invokes (ADR-009's tool
// kind). Implementations must be safe for concurrent Invoke calls — one
// Tool instance serves every step that names it across the worker's
// consumer goroutines — and must return promptly once ctx is canceled
// (the lease heartbeat stops with the handler).
type Tool interface {
	// Manifest is the tool's self-description (ADR-009): kind tool, a
	// unique name (^[a-z][a-z0-9_]*$), a semver version feeding M9 cache
	// keys, capability flags, and — required for every tool — the JSON
	// Schema of its arguments in ConfigSchema. The registry validates the
	// identity and compiles the schema at registration.
	Manifest() plugin.Manifest

	// Invoke performs one capability call. inv.Args has already been
	// validated against the tool's args schema (the executor rejects bad
	// args before calling Invoke), so Invoke decodes them without
	// re-checking shape. The returned bytes are the step's output payload,
	// read downstream through templating and CEL; a nil result persists as
	// JSON null.
	//
	// Errors classify per ADR-006: return a *tools.Error (or a
	// *HostNotAllowedError) to declare transient/permanent, and pass ctx
	// cancellation/deadline through unwrapped so the engine keeps the
	// timeout/cancelled judgment (ADR-006 rows 3/8). An unclassified error
	// defaults to transient at the engine.
	Invoke(ctx context.Context, inv Invocation) (json.RawMessage, error)
}

// Invocation is everything a Tool sees about the call it is performing.
// The executor builds one per invocation from the claimed step's rendered
// config and durable identity; tools must treat it as read-only.
type Invocation struct {
	// Args is the tool's argument payload — the `tool` step config's
	// `input` field after 8.2 template rendering, already validated
	// against the tool's args schema.
	Args json.RawMessage

	// IdempotencyKey is the step's stable idempotency token (ticket 5.5):
	// identical across attempts, retries, reclaims, and zombie takeovers.
	// http_request sends it as an Idempotency-Key header on non-GET calls
	// so a retried request is deduplicated by an idempotency-aware server.
	// Empty when the invoker predates 5.5 wiring (bare unit contexts).
	IdempotencyKey string

	// Logger carries the run/step/attempt log fields stamped by the
	// worker. May be nil (tools fall back to slog.Default()).
	Logger *slog.Logger
}

// logger returns the invocation's logger, falling back to slog.Default()
// so tools never nil-check (the StepContext convention).
func (inv Invocation) logger() *slog.Logger {
	if inv.Logger != nil {
		return inv.Logger
	}
	return slog.Default()
}

// argsSchema reflects a tool's argument struct into a standalone JSON
// Schema (2020-12) document, the same generator and reflector settings
// dag.StepConfigSchema uses for step config: strict additionalProperties:
// false mirroring the tools' strict arg decoding, fields required unless
// tagged omitempty or pointer-typed. Generated from the Go struct the
// tool decodes, so the served schema and the validation can never drift.
func argsSchema(v any) (json.RawMessage, error) {
	r := &invopop.Reflector{Anonymous: true, ExpandedStruct: true}
	schema := r.Reflect(v)
	schema.Version = invopop.Version
	data, err := json.Marshal(schema)
	if err != nil {
		return nil, fmt.Errorf("tools: marshaling args schema for %T: %w", v, err)
	}
	return data, nil
}
