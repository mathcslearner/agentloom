package exec

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/mathcslearner/agentloom/internal/dag"
	"github.com/mathcslearner/agentloom/internal/obs/log"
)

// Builtins returns a registry holding the M4/M5 executors: the six test
// executors (noop, echo, sleep, fail_n_times, counter, and — since 5.5 —
// effectful_echo), the two control-flow types (join, branch), whose real
// semantics live in the engine — readiness counters and the edge-firing
// rule — so their executors are trivial by design, and the three dev
// stubs (llm, tool, retrieve; devstub.go) that make the canonical example
// definitions runnable until the real executors arrive through the M8
// plugin SPI and the M9 provider layer.
func Builtins() *Registry {
	r, err := NewRegistry(NoopExecutor{}, EchoExecutor{}, NewSleep(), FailNTimesExecutor{},
		CounterExecutor{}, EffectfulEchoExecutor{}, JoinExecutor{}, BranchExecutor{},
		StubLLMExecutor{}, StubToolExecutor{}, StubRetrieveExecutor{})
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

// Execute implements Executor.
func (NoopExecutor) Execute(context.Context, StepContext) (Output, error) {
	return Output{}, nil
}

// EchoExecutor runs echo steps: return the configured input as output
// (ADR-003), falling back to the step's rendered input when the config
// has none — so echo also serves as a pass-through once M6 rendering
// exists.
type EchoExecutor struct{}

// Type implements Executor.
func (EchoExecutor) Type() string { return string(dag.StepEcho) }

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
// executing one is a pass-through of its rendered input (nil until M6
// rendering merges upstream outputs). Mode is readiness semantics, not
// execution semantics, so the config is not consulted here.
type JoinExecutor struct{}

// Type implements Executor.
func (JoinExecutor) Type() string { return string(dag.StepJoin) }

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

// FailNTimesExecutor runs fail_n_times steps: fail attempts 1..n, succeed
// from attempt n+1 on. The only state consulted is the durable attempt
// number, so the schedule holds across process crashes and reclaims —
// which is the point: it exercises M5's retry policies and the chaos
// suites deterministically.
type FailNTimesExecutor struct{}

// Type implements Executor.
func (FailNTimesExecutor) Type() string { return string(dag.StepFailNTimes) }

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
