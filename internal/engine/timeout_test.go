package engine

import (
	"context"
	"encoding/json"
	"errors"
	"runtime"
	"testing"
	"time"

	"github.com/mathcslearner/agentloom/internal/dag"
	"github.com/mathcslearner/agentloom/internal/exec"
)

func TestStepTimeout(t *testing.T) {
	t.Parallel()

	str := func(s string) *string { return &s }
	cases := []struct {
		name    string
		raw     *string
		want    time.Duration
		wantErr bool
	}{
		{name: "nil means none", raw: nil, want: 0},
		{name: "empty means none", raw: str(""), want: 0},
		{name: "parses", raw: str("150ms"), want: 150 * time.Millisecond},
		{name: "unparseable is corrupt", raw: str("fast"), wantErr: true},
		{name: "zero is corrupt", raw: str("0s"), wantErr: true},
		{name: "negative is corrupt", raw: str("-1s"), wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := stepTimeout(tc.raw)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("stepTimeout(%v): want error, got %v", tc.raw, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("stepTimeout: %v", err)
			}
			if got != tc.want {
				t.Fatalf("stepTimeout: want %v, got %v", tc.want, got)
			}
		})
	}
}

func TestTimedOutDiscriminatesDeadlineFromCancel(t *testing.T) {
	t.Parallel()

	live := context.Background()
	if timedOut(live) {
		t.Error("live context judged timed out")
	}

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if timedOut(cancelled) {
		t.Error("parent-style cancellation judged timed out — that class is 5.6's")
	}

	expired, cancel2 := context.WithTimeout(context.Background(), time.Nanosecond)
	defer cancel2()
	<-expired.Done()
	if !timedOut(expired) {
		t.Error("elapsed deadline not judged timed out")
	}
}

// sleepStepCtx builds the StepContext for a sleep step of the given
// configured duration.
func sleepStepCtx(duration string) exec.StepContext {
	return exec.StepContext{
		StepType: dag.StepSleep,
		Config:   json.RawMessage(`{"duration": "` + duration + `"}`),
		Attempt:  1,
	}
}

// TestRunExecutorTimeout is the ticket 5.3 unit contract: a sleep far
// longer than the timeout is cancelled at the deadline, the invocation
// reports expired, and nothing is left running afterwards — the executor
// runs on the calling goroutine and the watchdog is joined, so the
// goroutine count settles back to its baseline (the no-leak criterion).
// Deliberately not parallel: it counts goroutines.
func TestRunExecutorTimeout(t *testing.T) {
	base := runtime.NumGoroutine()

	start := time.Now()
	_, expired, execErr := runExecutor(context.Background(), exec.NewSleep(), sleepStepCtx("10s"), 20*time.Millisecond)
	elapsed := time.Since(start)

	if !expired {
		t.Error("want expired=true at the deadline")
	}
	if !errors.Is(execErr, context.DeadlineExceeded) {
		t.Errorf("want the executor to surface context.DeadlineExceeded, got %v", execErr)
	}
	if elapsed > 5*time.Second {
		t.Errorf("executor was not cancelled at the deadline (took %s)", elapsed)
	}
	waitForGoroutines(t, base)
}

// TestRunExecutorNoTimeout pins the zero-timeout path: the executor runs
// under the caller's context untouched — no deadline, no watchdog.
func TestRunExecutorNoTimeout(t *testing.T) {
	t.Parallel()

	probe := executorFunc(func(ctx context.Context, _ exec.StepContext) (exec.Output, error) {
		if _, hasDeadline := ctx.Deadline(); hasDeadline {
			t.Error("zero timeout must not install a deadline")
		}
		return exec.Output{}, nil
	})
	_, expired, execErr := runExecutor(context.Background(), probe, exec.StepContext{}, 0)
	if execErr != nil || expired {
		t.Fatalf("want clean success, got err=%v expired=%v", execErr, expired)
	}
}

// TestRunExecutorSuccessAfterDeadline pins the success-races-deadline
// rule: a result produced after the deadline still comes back as a
// success, with expired=true so the caller can log the overrun — the
// engine honors finished work instead of discarding it.
func TestRunExecutorSuccessAfterDeadline(t *testing.T) {
	t.Parallel()

	stubborn := executorFunc(func(ctx context.Context, _ exec.StepContext) (exec.Output, error) {
		<-ctx.Done() // ignore the cancellation, then succeed anyway
		return exec.Output{Data: json.RawMessage(`{"late": true}`)}, nil
	})
	out, expired, execErr := runExecutor(context.Background(), stubborn, exec.StepContext{}, 10*time.Millisecond)
	if execErr != nil {
		t.Fatalf("want success, got %v", execErr)
	}
	if !expired {
		t.Error("want expired=true when the deadline elapsed before the result")
	}
	if string(out.Data) != `{"late": true}` {
		t.Errorf("late output not preserved: %s", out.Data)
	}
}

// TestRunExecutorParentCancelIsNotTimeout pins the discriminator: a
// parent cancellation (shutdown) mid-execution is not judged a timeout
// even when a step timeout is configured — that failure keeps its 4.x
// redeliver route until 5.6 assigns the cancelled class.
func TestRunExecutorParentCancelIsNotTimeout(t *testing.T) {
	t.Parallel()

	parent, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(10 * time.Millisecond)
		cancel()
	}()
	defer cancel()
	_, expired, execErr := runExecutor(parent, exec.NewSleep(), sleepStepCtx("10s"), time.Minute)
	if expired {
		t.Error("parent cancellation judged as a timeout")
	}
	if !errors.Is(execErr, context.Canceled) {
		t.Errorf("want context.Canceled, got %v", execErr)
	}
}

// executorFunc adapts a function to exec.Executor for test doubles.
type executorFunc func(ctx context.Context, sc exec.StepContext) (exec.Output, error)

func (executorFunc) Type() string { return "test" }
func (f executorFunc) Execute(ctx context.Context, sc exec.StepContext) (exec.Output, error) {
	return f(ctx, sc)
}

// waitForGoroutines waits for the goroutine count to settle back to the
// baseline, dumping all stacks on failure so a leak names its culprit.
func waitForGoroutines(t *testing.T, base int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if runtime.NumGoroutine() <= base {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	buf := make([]byte, 1<<20)
	n := runtime.Stack(buf, true)
	t.Fatalf("goroutines leaked after a timed-out execution: baseline %d, now %d\n%s",
		base, runtime.NumGoroutine(), buf[:n])
}
