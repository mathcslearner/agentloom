package exec

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/mathcslearner/agentloom/internal/dag"
	"github.com/mathcslearner/agentloom/internal/obs/log"
)

// Builtins returns a registry holding the four M4 test executors: noop,
// echo, sleep, and fail_n_times. Real executors (llm, tool, retrieve, …)
// register through the M8 plugin SPI, not here.
func Builtins() *Registry {
	r, err := NewRegistry(NoopExecutor{}, EchoExecutor{}, NewSleep(), FailNTimesExecutor{})
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
