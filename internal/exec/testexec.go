package exec

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/mathcslearner/agentloom/internal/blackboard"
	"github.com/mathcslearner/agentloom/internal/dag"
	"github.com/mathcslearner/agentloom/internal/llm"
	"github.com/mathcslearner/agentloom/internal/obs/log"
	"github.com/mathcslearner/agentloom/internal/plugin"
	"github.com/mathcslearner/agentloom/internal/retrieval"
	"github.com/mathcslearner/agentloom/internal/tools"
)

// builtinManifest is the shared shape of the builtins' self-descriptions
// (ticket 8.1, ADR-009): kind executor, name = step type, the flag table
// from ADR-009. The registry fills ConfigSchema from the dag catalog at
// registration, so manifests here never carry one.
//
// version is a real parameter of a plugin's identity even though every
// shipped builtin now sits at "1.0.0" (the last dev stub, retrieve, was
// promoted in 8.8): a future dev stub carries a pre-release version here,
// so the parameter stays.
//
//nolint:unparam // version is part of the manifest contract; all builtins happen to be 1.0.0 today
func builtinManifest(st dag.StepType, version, description string, caps plugin.Capabilities) plugin.Manifest {
	return plugin.Manifest{
		Kind:         plugin.KindExecutor,
		Name:         string(st),
		Version:      version,
		Description:  description,
		Capabilities: caps,
	}
}

// Builtins returns a registry holding the M4/M5 executors: the six test
// executors (noop, echo, sleep, fail_n_times, counter, and — since 5.5 —
// effectful_echo), the two control-flow types (join, branch), whose real
// semantics live in the engine — readiness counters and the edge-firing
// rule — so their executors are trivial by design, the production llm
// executor (ticket 8.6, backed by the given provider registry), the
// production tool executor (ticket 8.7, backed by the given tool
// registry), and the production retrieve executor (ticket 8.8, backed by
// the given retriever registry — the last dev stub, now replaced in
// place).
//
// providers routes llm-step models to providers, toolReg backs the tool
// executor, and retrievers back the retrieve executor; a nil registry is
// valid for any of the three — a step of that type then fails permanent at
// resolve/lookup time — which suits the non-executing call sites (manifest
// and registry-shape tests). The set also includes the production planner
// executor (ticket 13.3), an llm-family executor backed by the same provider
// registry. This is the registry for tests and dev/demo deployments.
// Production workers default to CoreBuiltins — see the
// AGENTLOOM_WORKER_TEST_EXECUTORS knob (ticket 6.2).
func Builtins(providers *llm.Registry, toolReg *tools.Registry, retrievers *retrieval.Registry) *Registry {
	r := CoreBuiltins(providers, toolReg, retrievers)
	for _, e := range []Executor{CounterExecutor{}, EffectfulEchoExecutor{}, BlackboardWriteExecutor{}} {
		if err := r.Register(e); err != nil {
			panic(err) // unreachable: a fixed set of distinct, non-empty types
		}
	}
	return r
}

// CoreBuiltins is Builtins minus the two test executors with filesystem
// side effects (counter, effectful_echo — both append to a caller-chosen
// path, which an authed deployment must not expose to arbitrary
// submitters; ticket 6.2, ADR-007). cmd/worker registers this set unless
// AGENTLOOM_WORKER_TEST_EXECUTORS opts the full set in. A submitted step
// of an unregistered type still validates (the dag catalog is definition
// shape, not fleet capability) and then dead-letters permanent at claim
// time via the registry miss — the established 5.4 path. providers backs
// the llm executor, toolReg the tool executor, and retrievers the retrieve
// executor (see Builtins).
func CoreBuiltins(providers *llm.Registry, toolReg *tools.Registry, retrievers *retrieval.Registry) *Registry {
	r, err := NewRegistry(NoopExecutor{}, EchoExecutor{}, NewSleep(), FailNTimesExecutor{},
		JoinExecutor{}, BranchExecutor{}, MapExecutor{}, GatherExecutor{}, HumanApprovalExecutor{},
		NewLLMExecutor(providers), NewPlannerExecutor(providers), NewAgentExecutor(providers),
		NewToolExecutor(toolReg), NewRetrieveExecutor(retrievers))
	if err != nil {
		panic(err) // unreachable: a fixed set of distinct, non-empty types
	}
	return r
}

// NoopExecutor runs noop steps: succeed immediately with no output
// (ADR-003). Config is ignored entirely — noop takes none.
type NoopExecutor struct{}

// Type implements Executor.
func (NoopExecutor) Type() string { return string(dag.StepNoop) }

// PluginManifest implements SelfDescribing (ticket 8.1).
func (NoopExecutor) PluginManifest() plugin.Manifest {
	return builtinManifest(dag.StepNoop, "1.0.0",
		"Succeed immediately with no output.",
		plugin.Capabilities{Cacheable: true})
}

// Execute implements Executor.
func (NoopExecutor) Execute(context.Context, StepContext) (Output, error) {
	return Output{}, nil
}

// EchoExecutor runs echo steps: return the configured input as output
// (ADR-003), falling back to StepContext.Input when the config has none.
// Since ticket 8.2 the configured input arrives template-rendered, which
// makes echo the data-flow probe: its output is exactly what rendering
// resolved.
type EchoExecutor struct{}

// Type implements Executor.
func (EchoExecutor) Type() string { return string(dag.StepEcho) }

// PluginManifest implements SelfDescribing (ticket 8.1).
func (EchoExecutor) PluginManifest() plugin.Manifest {
	return builtinManifest(dag.StepEcho, "1.0.0",
		"Return the configured input (or the rendered input) as output.",
		plugin.Capabilities{Cacheable: true})
}

// Execute implements Executor.
func (EchoExecutor) Execute(_ context.Context, sc StepContext) (Output, error) {
	c, err := configAs[*dag.EchoConfig](sc)
	if err != nil {
		return Output{}, err
	}
	if c != nil && len(c.Input) > 0 {
		return Output{Data: c.Input}, nil
	}
	return Output{Data: sc.Input}, nil
}

// SleepExecutor runs sleep steps: wait for the configured duration, then
// succeed. The wait honors ctx cancellation (returning ctx.Err()), and is
// injectable so unit tests never sleep for real.
type SleepExecutor struct {
	// sleep waits for d or until ctx is done, whichever comes first.
	sleep func(ctx context.Context, d time.Duration) error
}

// NewSleep returns a SleepExecutor that waits on the wall clock.
func NewSleep() *SleepExecutor {
	return &SleepExecutor{sleep: sleepContext}
}

// sleepContext is the production sleep: a timer raced against ctx.
func sleepContext(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// Type implements Executor.
func (*SleepExecutor) Type() string { return string(dag.StepSleep) }

// PluginManifest implements SelfDescribing (ticket 8.1). Sleep is pure
// but deliberately not cacheable: a cache hit would skip the wait, which
// is the step's whole meaning (ADR-009).
func (*SleepExecutor) PluginManifest() plugin.Manifest {
	return builtinManifest(dag.StepSleep, "1.0.0",
		"Wait for the configured duration, then succeed.",
		plugin.Capabilities{})
}

// Execute implements Executor.
func (e *SleepExecutor) Execute(ctx context.Context, sc StepContext) (Output, error) {
	c, err := configAs[*dag.SleepConfig](sc)
	if err != nil {
		return Output{}, err
	}
	if c == nil || c.Duration == "" {
		return Output{}, &InvalidConfigError{StepType: string(sc.StepType), cause: fmt.Errorf("missing required field %q", "duration")}
	}
	d, perr := time.ParseDuration(c.Duration)
	if perr != nil {
		return Output{}, &InvalidConfigError{StepType: string(sc.StepType), cause: fmt.Errorf("field %q: %w", "duration", perr)}
	}
	if d <= 0 {
		return Output{}, &InvalidConfigError{StepType: string(sc.StepType), cause: fmt.Errorf("field %q must be positive, got %q", "duration", c.Duration)}
	}
	if err := e.sleep(ctx, d); err != nil {
		return Output{}, err
	}
	return Output{Data: json.RawMessage(fmt.Sprintf(`{"slept":%q}`, c.Duration))}, nil
}

// JoinExecutor runs join steps. A join is a fan-in barrier whose whole
// meaning is *when* it becomes ready (the engine's counter guard, ADR-004);
// executing one is a pass-through of StepContext.Input (nil until a
// merged-input payload exists — downstream steps reference specific
// parents via templates instead). Mode is readiness semantics, not
// execution semantics, so the config is not consulted here.
type JoinExecutor struct{}

// Type implements Executor.
func (JoinExecutor) Type() string { return string(dag.StepJoin) }

// PluginManifest implements SelfDescribing (ticket 8.1). Control flow:
// pure, but caching it is noise, so no flags (ADR-009).
func (JoinExecutor) PluginManifest() plugin.Manifest {
	return builtinManifest(dag.StepJoin, "1.0.0",
		"Fan-in barrier; readiness semantics live in the engine.",
		plugin.Capabilities{})
}

// Execute implements Executor.
func (JoinExecutor) Execute(_ context.Context, sc StepContext) (Output, error) {
	return Output{Data: sc.Input}, nil
}

// BranchExecutor runs branch steps: a pass-through whose output feeds the
// `when` predicates on its outgoing edges (the exclusive first-match rule
// lives in the engine's edge firing, ADR-003). Output is the configured
// input, falling back to the rendered input — the same convention as echo,
// and per BranchConfig's contract.
type BranchExecutor struct{}

// Type implements Executor.
func (BranchExecutor) Type() string { return string(dag.StepBranch) }

// PluginManifest implements SelfDescribing (ticket 8.1).
func (BranchExecutor) PluginManifest() plugin.Manifest {
	return builtinManifest(dag.StepBranch, "1.0.0",
		"Pass-through whose output feeds the exclusive edge-firing rule.",
		plugin.Capabilities{})
}

// Execute implements Executor.
func (BranchExecutor) Execute(_ context.Context, sc StepContext) (Output, error) {
	c, err := configAs[*dag.BranchConfig](sc)
	if err != nil {
		return Output{}, err
	}
	if c != nil && len(c.Input) > 0 {
		return Output{Data: c.Input}, nil
	}
	return Output{Data: sc.Input}, nil
}

// CounterExecutor runs counter steps: append one line recording the
// attempt number to the file at the configured path — a deliberate,
// externally observable side effect. The crash and chaos suites (4.7, M5)
// give each counter step its own file and count lines to prove effects
// fired exactly once across kills, reclaims, and retries; StepContext
// carries no step identity by design (4.1's minimal SPI), so distinctness
// comes from the per-step path. O_APPEND keeps concurrent appends from
// separate processes intact.
type CounterExecutor struct{}

// Type implements Executor.
func (CounterExecutor) Type() string { return string(dag.StepCounter) }

// PluginManifest implements SelfDescribing (ticket 8.1).
func (CounterExecutor) PluginManifest() plugin.Manifest {
	return builtinManifest(dag.StepCounter, "1.0.0",
		"Append one attempt-stamped line to a file (unjournaled test effect).",
		plugin.Capabilities{SideEffectful: true})
}

// Execute implements Executor.
func (CounterExecutor) Execute(_ context.Context, sc StepContext) (Output, error) {
	c, err := configAs[*dag.CounterConfig](sc)
	if err != nil {
		return Output{}, err
	}
	if c == nil || c.Path == "" {
		return Output{}, &InvalidConfigError{StepType: string(sc.StepType), cause: fmt.Errorf("missing required field %q", "path")}
	}
	f, err := os.OpenFile(c.Path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return Output{}, fmt.Errorf("counter: opening %s: %w", c.Path, err)
	}
	_, werr := fmt.Fprintf(f, "attempt=%d\n", sc.Attempt)
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		return Output{}, fmt.Errorf("counter: appending to %s: %w", c.Path, werr)
	}
	return Output{Data: json.RawMessage(fmt.Sprintf(`{"counted":true,"attempt":%d}`, sc.Attempt))}, nil
}

// EffectfulEchoExecutor runs effectful_echo steps (ticket 5.5): increment
// an external counter — one line appended to the configured file — through
// the side-effect journal, then echo the configured input as output. It is
// counter's journaled sibling: where counter appends on every execution
// (proving how often the engine really ran it), effectful_echo must append
// exactly once no matter how many attempts, reclaims, or zombie takeovers
// the step suffers — the journal short-circuits every re-execution to the
// journaled result. The appended line carries the idempotency key and the
// attempt number, so tests assert both the single fire and the key's
// stability from the file alone.
//
// FailTimes makes attempts 1..N fail with a transient error *after* the
// journaled effect, which is how a retrying step proves the short-circuit:
// the step takes N+1 attempts but the file gains one line.
type EffectfulEchoExecutor struct{}

// Type implements Executor.
func (EffectfulEchoExecutor) Type() string { return string(dag.StepEffectfulEcho) }

// PluginManifest implements SelfDescribing (ticket 8.1).
func (EffectfulEchoExecutor) PluginManifest() plugin.Manifest {
	return builtinManifest(dag.StepEffectfulEcho, "1.0.0",
		"Journal one file-append effect, then echo the input.",
		plugin.Capabilities{SideEffectful: true})
}

// Execute implements Executor.
func (EffectfulEchoExecutor) Execute(ctx context.Context, sc StepContext) (Output, error) {
	c, err := configAs[*dag.EffectfulEchoConfig](sc)
	if err != nil {
		return Output{}, err
	}
	if c == nil || c.Path == "" {
		return Output{}, &InvalidConfigError{StepType: string(sc.StepType), cause: fmt.Errorf("missing required field %q", "path")}
	}
	if sc.Effects == nil {
		// Firing the effect unjournaled would silently break the
		// exactly-once contract this executor exists to prove — a wiring
		// bug, so permanent, never a retry.
		return Output{}, Permanentf("effectful_echo: no side-effect journal wired into StepContext")
	}
	out, err := sc.Effects.Do(ctx, "echo", func(context.Context) (json.RawMessage, error) {
		f, err := os.OpenFile(c.Path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
		if err != nil {
			return nil, fmt.Errorf("effectful_echo: opening %s: %w", c.Path, err)
		}
		_, werr := fmt.Fprintf(f, "key=%s attempt=%d\n", sc.IdempotencyKey, sc.Attempt)
		if cerr := f.Close(); werr == nil {
			werr = cerr
		}
		if werr != nil {
			return nil, fmt.Errorf("effectful_echo: appending to %s: %w", c.Path, werr)
		}
		if len(c.Input) > 0 {
			return c.Input, nil
		}
		return sc.Input, nil
	})
	if err != nil {
		return Output{}, err
	}
	if sc.Attempt <= c.FailTimes {
		sc.logger().Info("effectful_echo: deliberate post-journal failure",
			log.Attempt(sc.Attempt), "fail_times", c.FailTimes)
		return Output{}, Transientf("effectful_echo: deliberate failure on attempt %d of %d (effect already journaled)", sc.Attempt, c.FailTimes)
	}
	return Output{Data: out}, nil
}

// BlackboardWriteExecutor runs blackboard_write steps (ticket 12.2): it
// writes a value to the run-scoped blackboard through StepContext.Blackboard
// (optionally under a version compare-and-swap guard), then echoes the
// written entry — plus, when read_key is set, the current head of that key —
// as output. It exists so the engine integration tests exercise the
// programmatic blackboard read/write API and the CAS path without a real
// llm/tool executor. Side-effectful (it mutates shared memory), so
// uncacheable — the blackboard write is the whole point of the step.
type BlackboardWriteExecutor struct{}

// Type implements Executor.
func (BlackboardWriteExecutor) Type() string { return string(dag.StepBlackboardWrite) }

// PluginManifest implements SelfDescribing (ticket 8.1).
func (BlackboardWriteExecutor) PluginManifest() plugin.Manifest {
	return builtinManifest(dag.StepBlackboardWrite, "1.0.0",
		"Write a value to the run-scoped blackboard, then echo it.",
		plugin.Capabilities{SideEffectful: true})
}

// blackboardWriteOutput is what a blackboard_write step returns: the written
// entry, and (when read_key was set) the head it read back.
type blackboardWriteOutput struct {
	Wrote json.RawMessage `json:"wrote"`
	Read  json.RawMessage `json:"read,omitempty"`
}

// Execute implements Executor.
func (BlackboardWriteExecutor) Execute(ctx context.Context, sc StepContext) (Output, error) {
	c, err := configAs[*dag.BlackboardWriteConfig](sc)
	if err != nil {
		return Output{}, err
	}
	if c == nil || c.Key == "" {
		return Output{}, &InvalidConfigError{StepType: string(sc.StepType), cause: fmt.Errorf("missing required field %q", "key")}
	}
	if sc.Blackboard == nil {
		// Requiring the board but finding none wired is a permanent wiring bug.
		return Output{}, Permanentf("blackboard_write: no blackboard wired into StepContext")
	}
	entry, err := sc.Blackboard.Put(ctx, blackboard.PutArgs{
		Key: c.Key, Value: c.Value, Tags: c.Tags, ExpectedVersion: c.ExpectedVersion,
	})
	if err != nil {
		// A CAS conflict is transient (retry re-reads the head); the leaf's
		// permanent errors (bad key/tag/value) and a fence pass through with
		// their own classification, mapped by the engine's classifier.
		var ce *blackboard.VersionConflictError
		if errors.As(err, &ce) {
			return Output{}, Transientf("blackboard_write: %v", err)
		}
		return Output{}, err
	}
	wrote, _ := json.Marshal(entry)
	out := blackboardWriteOutput{Wrote: wrote}
	if c.ReadKey != "" {
		head, ok, rerr := sc.Blackboard.Get(ctx, c.ReadKey)
		if rerr != nil {
			return Output{}, rerr
		}
		if ok {
			out.Read, _ = json.Marshal(head)
		}
	}
	data, _ := json.Marshal(out)
	return Output{Data: data}, nil
}

// FailNTimesExecutor runs fail_n_times steps: fail attempts 1..n, succeed
// from attempt n+1 on. The only state consulted is the durable attempt
// number, so the schedule holds across process crashes and reclaims —
// which is the point: it exercises M5's retry policies and the chaos
// suites deterministically.
type FailNTimesExecutor struct{}

// Type implements Executor.
func (FailNTimesExecutor) Type() string { return string(dag.StepFailNTimes) }

// PluginManifest implements SelfDescribing (ticket 8.1). Not cacheable:
// the output branches on the durable attempt number (ADR-009).
func (FailNTimesExecutor) PluginManifest() plugin.Manifest {
	return builtinManifest(dag.StepFailNTimes, "1.0.0",
		"Fail attempts 1..n, succeed from attempt n+1 on.",
		plugin.Capabilities{})
}

// Execute implements Executor.
func (FailNTimesExecutor) Execute(_ context.Context, sc StepContext) (Output, error) {
	c, err := configAs[*dag.FailNTimesConfig](sc)
	if err != nil {
		return Output{}, err
	}
	if c == nil || c.N < 1 {
		return Output{}, &InvalidConfigError{StepType: string(sc.StepType), cause: fmt.Errorf("field %q must be at least 1", "n")}
	}
	if sc.Attempt <= c.N {
		sc.logger().Info("fail_n_times: deliberate failure",
			log.Attempt(sc.Attempt), "n", c.N)
		return Output{}, fmt.Errorf("fail_n_times: deliberate failure on attempt %d of %d", sc.Attempt, c.N)
	}
	return Output{Data: json.RawMessage(fmt.Sprintf(`{"succeeded_on_attempt":%d}`, sc.Attempt))}, nil
}
