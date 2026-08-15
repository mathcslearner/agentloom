package exec

import (
	"context"
	"errors"
	"fmt"

	"github.com/mathcslearner/agentloom/internal/cache"
	"github.com/mathcslearner/agentloom/internal/dag"
	"github.com/mathcslearner/agentloom/internal/plugin"
	"github.com/mathcslearner/agentloom/internal/tools"
)

// toolVersion is the production tool executor's plugin version (ADR-009).
// It replaces the dev stub's 0.1.0-stub in place; the bump to 1.0.0 is the
// behavior change M9's response cache keys on. The capability flags are
// unchanged from the stub (side_effectful) — an unknown tool must be
// assumed to act on the world, and the specific built-in tools carry their
// own per-tool flags in the tools registry.
const toolVersion = "1.0.0"

// ToolExecutor runs tool steps (ticket 8.7): it decodes the (already
// 8.2-rendered) ToolConfig, looks the named tool up in the tool registry,
// validates the step's args against the tool's declared JSON Schema BEFORE
// invoking (bad args → permanent, no call — the acceptance guarantee),
// makes exactly one Invoke, and persists the tool's result verbatim as the
// step output. It never retries — the M5 engine owns retry, informed by
// the ADR-006 class the tool declares.
type ToolExecutor struct {
	tools *tools.Registry
}

// NewToolExecutor builds the tool executor over a tool registry. A nil
// registry is valid — a worker that runs no tool steps needs none — and
// any tool step then fails permanent at lookup with a diagnosable
// "no tools configured" error, mirroring the llm executor's keyless path.
func NewToolExecutor(reg *tools.Registry) ToolExecutor {
	return ToolExecutor{tools: reg}
}

// Type implements Executor.
func (ToolExecutor) Type() string { return string(dag.StepTool) }

// PluginManifest implements SelfDescribing (ADR-009). side_effectful: an
// unknown tool must be assumed to act on the world; the specific tools
// declare their own flags in the tools registry, surfaced under kind tool
// in GET /v1/plugins.
func (ToolExecutor) PluginManifest() plugin.Manifest {
	return builtinManifest(dag.StepTool, toolVersion,
		"One invocation of a named tool through the tool SPI.",
		plugin.Capabilities{SideEffectful: true})
}

// Execute runs one tool attempt. Lookup and args-validation failures are
// deterministic (permanent); the tool's own errors carry an ADR-006 class
// honored through ClassifiedError; context cancellation/deadline pass
// through unwrapped so the engine judges timeout vs. cancelled.
func (e ToolExecutor) Execute(ctx context.Context, sc StepContext) (Output, error) {
	cfg, err := configAs[*dag.ToolConfig](sc)
	if err != nil {
		return Output{}, err
	}
	if cfg == nil || cfg.Tool == "" {
		// Config was validated at submit time (1.3 requires tool); hitting
		// this means corrupt stored state or a version skew — permanent.
		return Output{}, &InvalidConfigError{StepType: string(sc.StepType), cause: fmt.Errorf("missing required field %q", "tool")}
	}

	if e.tools == nil {
		// A worker with no tool registry can never run this step —
		// deterministic, so permanent, not a retry.
		return Output{}, Permanentf("no tools configured for tool %q", cfg.Tool)
	}

	// Pre-invocation gate: validate the rendered args against the tool's
	// declared schema. A violation (or an unknown tool) is permanent, and
	// the tool's Invoke is never reached.
	if err := e.tools.ValidateArgs(cfg.Tool, cfg.Input); err != nil {
		return Output{}, Permanentf("tool %q: %w", cfg.Tool, err)
	}

	tool, err := e.tools.Get(cfg.Tool)
	if err != nil {
		// Unreachable after ValidateArgs (which also reports an unknown
		// tool), but kept explicit so a future ValidateArgs change can't
		// silently drop the check.
		return Output{}, Permanentf("resolving tool %q: %w", cfg.Tool, err)
	}

	result, err := tool.Invoke(ctx, tools.Invocation{
		Args:           cfg.Input,
		IdempotencyKey: sc.IdempotencyKey,
		Logger:         sc.logger(),
	})
	if err != nil {
		return Output{}, classifyToolError(err)
	}
	return Output{Data: result}, nil
}

// ResourceClaim implements ResourceClaimer (ADR-010): a tool step's
// fleet-wide resource is "tool:<name>", the pool a rate-limited endpoint
// declares (e.g. "tool:http_request" for a shared third-party API). The
// token dimension is not meaningful for a tool call, so estTokens is 0 —
// the middleware then governs only the requests bucket, and a tool whose
// resource is unconfigured (pure tools like json_transform, or any tool an
// operator did not name) bypasses the limiter entirely. A corrupt config
// returns an error so the middleware defers to Execute's permanent failure.
func (ToolExecutor) ResourceClaim(sc StepContext) (string, int64, error) {
	cfg, err := configAs[*dag.ToolConfig](sc)
	if err != nil {
		return "", 0, err
	}
	if cfg == nil || cfg.Tool == "" {
		return "", 0, fmt.Errorf("missing required field %q", "tool")
	}
	return "tool:" + cfg.Tool, 0, nil
}

// CacheBinding implements CacheBinder (ADR-011, ticket 9.5): a tool step's
// cache key is built over the invoked tool's identity and the rendered
// arguments. Critically, the eligibility flags come from the INVOKED TOOL's
// manifest, not the tool executor's — the executor is side_effectful (worst
// case across tools), but a pure tool like json_transform is cacheable and
// must be. Determinism follows from the tool's own flags (cacheable and not
// side-effectful ⇒ a pure function of its args). A corrupt config or unknown
// tool returns an error so the middleware defers to Execute's permanent
// failure.
func (e ToolExecutor) CacheBinding(sc StepContext) (*CacheBinding, error) {
	cfg, err := configAs[*dag.ToolConfig](sc)
	if err != nil {
		return nil, err
	}
	if cfg == nil || cfg.Tool == "" {
		return nil, fmt.Errorf("missing required field %q", "tool")
	}
	if e.tools == nil {
		return nil, fmt.Errorf("no tools configured for tool %q", cfg.Tool)
	}
	tool, err := e.tools.Get(cfg.Tool)
	if err != nil {
		return nil, fmt.Errorf("resolving tool %q: %w", cfg.Tool, err)
	}
	m := tool.Manifest()
	deterministic := m.Capabilities.Cacheable && !m.Capabilities.SideEffectful
	return &CacheBinding{
		Executor:      cache.PluginRef{Kind: plugin.KindExecutor, Name: string(dag.StepTool), Version: toolVersion},
		Plugin:        cache.PluginRef{Kind: m.Kind, Name: m.Name, Version: m.Version},
		Caps:          m.Capabilities,
		Deterministic: deterministic,
		Request:       cache.ToolRequest{Input: cfg.Input},
	}, nil
}

// classifyToolError maps a tool failure onto the executor's classified
// contract. A *tools.Error carries its ADR-006 class; a
// *tools.HostNotAllowedError is permanent (its own type, per the ticket);
// a context error passes through unwrapped so the engine judges
// timeout/cancelled (ADR-006 rows 3/8); anything else falls to the
// engine's default-transient.
func classifyToolError(err error) error {
	var te *tools.Error
	if errors.As(err, &te) {
		return &ClassifiedError{Class: te.Class, Err: err}
	}
	var hostErr *tools.HostNotAllowedError
	if errors.As(err, &hostErr) {
		return &ClassifiedError{Class: hostErr.Class(), Err: err}
	}
	// Context cancellation/deadline (or any other pass-through) keeps its
	// identity so errors.Is against context.Canceled/DeadlineExceeded holds
	// in the engine.
	return err
}
