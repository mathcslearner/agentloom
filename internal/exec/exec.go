// Package exec defines the executor SPI v0 (ticket 4.1): the Executor
// interface the worker's execute pipeline (4.3) invokes for each claimed
// step, the Registry mapping step types to executors, and the five test
// executors M4/M5's fixtures and chaos suites are built from (noop, echo,
// sleep, fail_n_times, and — since 4.7 — counter).
//
// This is deliberately minimal — no middleware chain, no side-effect
// journal, no config schemas. The full plugin SPI arrives in M8 and will
// grow this package rather than replace it: Output is a struct so success
// payloads can gain fields (usage, artifacts), the Registry is an instance
// (not a package global) so M8 can construct it from plugin discovery, and
// executors decode their own config from raw JSON via the dag package's
// registered config types, which is exactly where per-plugin config
// schemas will slot in.
package exec

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/mathcslearner/agentloom/internal/dag"
)

// Output is what a successful execution produces. Data is the step's
// output payload, stored on the run_steps row and exposed to downstream
// CEL predicates and templates as `output`; nil means the step has no
// output (persisted as JSON null).
type Output struct {
	Data json.RawMessage
}

// StepContext is everything an executor may see about the step it is
// running. The worker builds one per invocation from the claimed step's
// run_steps row and attempt state; executors must treat it as read-only.
type StepContext struct {
	// StepType is the claimed step's type, as stored on the run_steps row.
	// It selects the config shape Config decodes into.
	StepType dag.StepType

	// Config is the step's raw config exactly as materialized into
	// run_steps.config (ADR-004); nil when the definition had no config
	// key. Executors decode it with configAs — decoding stays in the
	// executor, so the worker never needs to know config shapes.
	Config json.RawMessage

	// Input is the step's rendered input payload. Template rendering
	// arrives in M6; until then the engine passes through whatever raw
	// input the step carries, and executors must tolerate nil.
	Input json.RawMessage

	// Attempt is the 1-based attempt number from the claim's attempt row
	// (ADR-004: attempt_count increments at claim). It is the only
	// execution state an executor may branch on — anything in-process would
	// not survive a crash or reclaim.
	Attempt int

	// Logger carries the run/step/attempt log fields stamped by the
	// worker. May be nil (executors fall back to slog.Default()).
	Logger *slog.Logger
}

// logger returns the context's logger, falling back to slog.Default() so
// executors never nil-check (same convention as obs/log.From).
func (sc StepContext) logger() *slog.Logger {
	if sc.Logger != nil {
		return sc.Logger
	}
	return slog.Default()
}

// Executor runs one step type. Implementations must be safe for
// concurrent Execute calls (one Executor instance serves every claimed
// step of its type across the worker's consumer goroutines) and must
// return promptly once ctx is canceled — the lease heartbeater stops with
// the handler, so overstaying invites a reclaim and a fenced-out write.
type Executor interface {
	// Type is the step type this executor runs, matching the dag package's
	// StepType spelling (e.g. "sleep").
	Type() string

	// Execute runs one attempt of the step. An error return means the
	// attempt failed (retry policy arrives in M5); ctx cancellation should
	// surface as ctx.Err().
	Execute(ctx context.Context, sc StepContext) (Output, error)
}

// configAs decodes sc.Config into the typed config struct T registered
// for sc.StepType. An absent config yields a typed nil (callers
// required-field-check it, mirroring how Validate treats a missing config
// key); a config that fails strict decoding, or a T that does not match
// the step type's registered struct, is an *InvalidConfigError. Config
// was already validated at submit time, so a failure here means corrupt
// stored state or a worker/definition version skew — worth a typed error,
// not worth a recovery path.
func configAs[T dag.StepConfig](sc StepContext) (T, error) {
	var zero T
	cfg, err := dag.DecodeStepConfig(sc.StepType, sc.Config)
	if err != nil {
		return zero, &InvalidConfigError{StepType: string(sc.StepType), cause: err}
	}
	if cfg == nil {
		return zero, nil
	}
	typed, ok := cfg.(T)
	if !ok {
		return zero, &InvalidConfigError{StepType: string(sc.StepType), cause: fmt.Errorf("config decoded to %T, executor expected %T", cfg, zero)}
	}
	return typed, nil
}
