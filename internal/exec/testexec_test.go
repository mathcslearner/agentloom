package exec

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mathcslearner/agentloom/internal/dag"
)

func TestNoop(t *testing.T) {
	t.Parallel()

	// Config is ignored, present or not — even unknown fields, since noop
	// never decodes it.
	for _, cfg := range []json.RawMessage{nil, json.RawMessage(`{}`), json.RawMessage(`{"junk": 1}`)} {
		out, err := (NoopExecutor{}).Execute(context.Background(), StepContext{StepType: dag.StepNoop, Config: cfg})
		if err != nil {
			t.Errorf("Execute(config=%s): %v", cfg, err)
		}
		if out.Data != nil {
			t.Errorf("Execute(config=%s) output = %s, want empty", cfg, out.Data)
		}
	}
}

func TestEcho(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		config json.RawMessage
		input  json.RawMessage
		want   string
	}{
		{"config input wins", json.RawMessage(`{"input": {"a": 1}}`), json.RawMessage(`{"b":2}`), `{"a":1}`},
		{"falls back to rendered input", json.RawMessage(`{}`), json.RawMessage(`{"b":2}`), `{"b":2}`},
		{"absent config falls back", nil, json.RawMessage(`{"b":2}`), `{"b":2}`},
		{"nothing to echo", nil, nil, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			out, err := (EchoExecutor{}).Execute(context.Background(), StepContext{
				StepType: dag.StepEcho, Config: tc.config, Input: tc.input,
			})
			if err != nil {
				t.Fatalf("Execute: %v", err)
			}
			if string(out.Data) != tc.want {
				t.Errorf("output = %q, want %q", out.Data, tc.want)
			}
		})
	}

	t.Run("bad config", func(t *testing.T) {
		t.Parallel()
		_, err := (EchoExecutor{}).Execute(context.Background(), StepContext{
			StepType: dag.StepEcho, Config: json.RawMessage(`{"unknown_field": 1}`),
		})
		if !errors.Is(err, ErrInvalidConfig) {
			t.Errorf("error = %v, want ErrInvalidConfig", err)
		}
	})
}

func TestJoin(t *testing.T) {
	t.Parallel()

	// Pass-through of the rendered input; mode is readiness semantics, so
	// any valid config is inert here.
	for _, cfg := range []json.RawMessage{nil, json.RawMessage(`{}`), json.RawMessage(`{"mode": "any"}`)} {
		out, err := (JoinExecutor{}).Execute(context.Background(), StepContext{
			StepType: dag.StepJoin, Config: cfg, Input: json.RawMessage(`{"merged":true}`),
		})
		if err != nil {
			t.Errorf("Execute(config=%s): %v", cfg, err)
		}
		if string(out.Data) != `{"merged":true}` {
			t.Errorf("Execute(config=%s) output = %s, want the input passed through", cfg, out.Data)
		}
	}
}

func TestBranch(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		config json.RawMessage
		input  json.RawMessage
		want   string
	}{
		{"config input wins", json.RawMessage(`{"input": {"category": "refund"}}`), json.RawMessage(`{"b":2}`), `{"category":"refund"}`},
		{"falls back to rendered input", json.RawMessage(`{}`), json.RawMessage(`{"b":2}`), `{"b":2}`},
		{"absent config falls back", nil, json.RawMessage(`{"b":2}`), `{"b":2}`},
		{"no input at all", nil, nil, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			out, err := (BranchExecutor{}).Execute(context.Background(), StepContext{
				StepType: dag.StepBranch, Config: tc.config, Input: tc.input,
			})
			if err != nil {
				t.Fatalf("Execute: %v", err)
			}
			if string(out.Data) != tc.want {
				t.Errorf("output = %q, want %q", out.Data, tc.want)
			}
		})
	}

	t.Run("bad config", func(t *testing.T) {
		t.Parallel()
		_, err := (BranchExecutor{}).Execute(context.Background(), StepContext{
			StepType: dag.StepBranch, Config: json.RawMessage(`{"unknown_field": 1}`),
		})
		if !errors.Is(err, ErrInvalidConfig) {
			t.Errorf("error = %v, want ErrInvalidConfig", err)
		}
	})
}

func TestSleep(t *testing.T) {
	t.Parallel()

	t.Run("sleeps the configured duration", func(t *testing.T) {
		t.Parallel()
		var slept time.Duration
		e := &SleepExecutor{sleep: func(_ context.Context, d time.Duration) error {
			slept = d
			return nil
		}}
		out, err := e.Execute(context.Background(), StepContext{
			StepType: dag.StepSleep, Config: json.RawMessage(`{"duration": "1500ms"}`),
		})
		if err != nil {
			t.Fatalf("Execute: %v", err)
		}
		if slept != 1500*time.Millisecond {
			t.Errorf("slept %v, want 1.5s", slept)
		}
		if string(out.Data) != `{"slept":"1500ms"}` {
			t.Errorf("output = %s", out.Data)
		}
	})

	t.Run("cancellation surfaces as ctx.Err", func(t *testing.T) {
		t.Parallel()
		e := &SleepExecutor{sleep: sleepContext} // the real sleep, raced against a dead ctx
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, err := e.Execute(ctx, StepContext{
			StepType: dag.StepSleep, Config: json.RawMessage(`{"duration": "1h"}`),
		})
		if !errors.Is(err, context.Canceled) {
			t.Errorf("error = %v, want context.Canceled", err)
		}
	})

	t.Run("config errors", func(t *testing.T) {
		t.Parallel()
		cases := []struct {
			name   string
			config json.RawMessage
			want   string // substring of the error
		}{
			{"absent config", nil, `missing required field "duration"`},
			{"empty duration", json.RawMessage(`{}`), `missing required field "duration"`},
			{"unparseable duration", json.RawMessage(`{"duration": "soon"}`), "invalid duration"},
			{"non-positive duration", json.RawMessage(`{"duration": "-2s"}`), "must be positive"},
			{"unknown field", json.RawMessage(`{"durations": "2s"}`), "unknown field"},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				e := &SleepExecutor{sleep: func(context.Context, time.Duration) error {
					t.Error("sleep called despite invalid config")
					return nil
				}}
				_, err := e.Execute(context.Background(), StepContext{StepType: dag.StepSleep, Config: tc.config})
				if !errors.Is(err, ErrInvalidConfig) {
					t.Fatalf("error = %v, want ErrInvalidConfig", err)
				}
				if !strings.Contains(err.Error(), tc.want) {
					t.Errorf("error = %q, want substring %q", err, tc.want)
				}
			})
		}
	})
}

func TestFailNTimes(t *testing.T) {
	t.Parallel()

	config := json.RawMessage(`{"n": 2}`)
	sc := func(attempt int) StepContext {
		return StepContext{StepType: dag.StepFailNTimes, Config: config, Attempt: attempt}
	}
	e := FailNTimesExecutor{}

	for attempt := 1; attempt <= 2; attempt++ {
		_, err := e.Execute(context.Background(), sc(attempt))
		if err == nil || !strings.Contains(err.Error(), "deliberate failure") {
			t.Errorf("attempt %d: error = %v, want deliberate failure", attempt, err)
		}
	}
	out, err := e.Execute(context.Background(), sc(3))
	if err != nil {
		t.Fatalf("attempt 3: %v", err)
	}
	if string(out.Data) != `{"succeeded_on_attempt":3}` {
		t.Errorf("attempt 3 output = %s", out.Data)
	}

	// Statelessness across "processes": a fresh executor value sees the
	// same attempt number and makes the same call.
	if _, err := (FailNTimesExecutor{}).Execute(context.Background(), sc(2)); err == nil {
		t.Error("fresh executor at attempt 2 succeeded, want deliberate failure")
	}

	t.Run("config errors", func(t *testing.T) {
		t.Parallel()
		for _, cfg := range []json.RawMessage{nil, json.RawMessage(`{}`), json.RawMessage(`{"n": 0}`), json.RawMessage(`{"n": -1}`)} {
			_, err := e.Execute(context.Background(), StepContext{StepType: dag.StepFailNTimes, Config: cfg, Attempt: 1})
			if !errors.Is(err, ErrInvalidConfig) {
				t.Errorf("Execute(config=%s) error = %v, want ErrInvalidConfig", cfg, err)
			}
		}
	})
}

func TestCounter(t *testing.T) {
	t.Parallel()

	e := CounterExecutor{}
	sc := func(path string, attempt int) StepContext {
		cfg, err := json.Marshal(map[string]string{"path": path})
		if err != nil {
			t.Fatal(err)
		}
		return StepContext{StepType: dag.StepCounter, Config: cfg, Attempt: attempt}
	}

	t.Run("appends one attempt-stamped line per execution", func(t *testing.T) {
		t.Parallel()
		path := filepath.Join(t.TempDir(), "effects.log")
		out, err := e.Execute(context.Background(), sc(path, 1))
		if err != nil {
			t.Fatalf("Execute: %v", err)
		}
		if string(out.Data) != `{"counted":true,"attempt":1}` {
			t.Errorf("output = %s", out.Data)
		}
		// A second execution (a retry, attempt 2) appends — never truncates:
		// the file is the cross-process effect ledger the crash suite counts.
		if _, err := e.Execute(context.Background(), sc(path, 2)); err != nil {
			t.Fatalf("Execute attempt 2: %v", err)
		}
		data, err := os.ReadFile(path) // #nosec G304 -- t.TempDir path, test-only
		if err != nil {
			t.Fatalf("ReadFile: %v", err)
		}
		if got, want := string(data), "attempt=1\nattempt=2\n"; got != want {
			t.Errorf("file contents = %q, want %q", got, want)
		}
	})

	t.Run("config errors", func(t *testing.T) {
		t.Parallel()
		for _, cfg := range []json.RawMessage{nil, json.RawMessage(`{}`), json.RawMessage(`{"path": ""}`)} {
			_, err := e.Execute(context.Background(), StepContext{StepType: dag.StepCounter, Config: cfg, Attempt: 1})
			if !errors.Is(err, ErrInvalidConfig) {
				t.Errorf("Execute(config=%s) error = %v, want ErrInvalidConfig", cfg, err)
			}
		}
	})

	t.Run("unwritable path is an executor error, not invalid config", func(t *testing.T) {
		t.Parallel()
		path := filepath.Join(t.TempDir(), "no-such-dir", "effects.log")
		_, err := e.Execute(context.Background(), sc(path, 1))
		if err == nil {
			t.Fatal("Execute succeeded, want open error")
		}
		if errors.Is(err, ErrInvalidConfig) {
			t.Errorf("error = %v; an I/O failure must not classify as invalid config", err)
		}
	})
}

// fakeJournal implements EffectJournal in memory with the real journal's
// observable contract: the first Do per effect ID executes fn and stores
// its result; every later Do short-circuits to the stored result.
type fakeJournal struct {
	results map[string]json.RawMessage
}

func (f *fakeJournal) Do(ctx context.Context, effectID string, fn func(context.Context) (json.RawMessage, error)) (json.RawMessage, error) {
	if r, ok := f.results[effectID]; ok {
		return r, nil
	}
	out, err := fn(ctx)
	if err != nil {
		return nil, err
	}
	if f.results == nil {
		f.results = map[string]json.RawMessage{}
	}
	f.results[effectID] = out
	return out, nil
}

func TestEffectfulEcho(t *testing.T) {
	t.Parallel()

	e := EffectfulEchoExecutor{}
	sc := func(path string, attempt int, j EffectJournal) StepContext {
		cfg, err := json.Marshal(map[string]any{"path": path, "input": map[string]bool{"sent": true}, "fail_times": 1})
		if err != nil {
			t.Fatal(err)
		}
		return StepContext{
			StepType: dag.StepEffectfulEcho, Config: cfg, Attempt: attempt,
			IdempotencyKey: "idem-key-1", Effects: j,
		}
	}

	t.Run("journals once across a retry and echoes on success", func(t *testing.T) {
		t.Parallel()
		path := filepath.Join(t.TempDir(), "notify.log")
		j := &fakeJournal{}

		// Attempt 1: the effect fires and journals, then fail_times=1 fails
		// the attempt transiently — after the journal, by design.
		_, err := e.Execute(context.Background(), sc(path, 1, j))
		var ce *ClassifiedError
		if !errors.As(err, &ce) || ce.Class != dag.ClassTransient {
			t.Fatalf("attempt 1 error = %v, want a transient ClassifiedError", err)
		}

		// Attempt 2: the journal short-circuits — no new line — and the
		// attempt is past fail_times, so the step succeeds with the echo.
		out, err := e.Execute(context.Background(), sc(path, 2, j))
		if err != nil {
			t.Fatalf("attempt 2: %v", err)
		}
		if string(out.Data) != `{"sent":true}` {
			t.Errorf("output = %s, want the configured input echoed", out.Data)
		}
		data, err := os.ReadFile(path) // #nosec G304 -- t.TempDir path, test-only
		if err != nil {
			t.Fatalf("ReadFile: %v", err)
		}
		if got, want := string(data), "key=idem-key-1 attempt=1\n"; got != want {
			t.Errorf("file contents = %q, want exactly one line from attempt 1: %q", got, want)
		}
	})

	t.Run("nil journal is a permanent wiring error", func(t *testing.T) {
		t.Parallel()
		path := filepath.Join(t.TempDir(), "notify.log")
		_, err := e.Execute(context.Background(), sc(path, 1, nil))
		var ce *ClassifiedError
		if !errors.As(err, &ce) || ce.Class != dag.ClassPermanent {
			t.Fatalf("error = %v, want a permanent ClassifiedError", err)
		}
		if _, statErr := os.Stat(path); !errors.Is(statErr, os.ErrNotExist) {
			t.Error("effect fired despite the missing journal")
		}
	})

	t.Run("config errors", func(t *testing.T) {
		t.Parallel()
		for _, cfg := range []json.RawMessage{nil, json.RawMessage(`{}`), json.RawMessage(`{"path": ""}`)} {
			_, err := e.Execute(context.Background(), StepContext{
				StepType: dag.StepEffectfulEcho, Config: cfg, Attempt: 1, Effects: &fakeJournal{},
			})
			if !errors.Is(err, ErrInvalidConfig) {
				t.Errorf("Execute(config=%s) error = %v, want ErrInvalidConfig", cfg, err)
			}
		}
	})

	t.Run("append failure surfaces as the fn error, unjournaled", func(t *testing.T) {
		t.Parallel()
		path := filepath.Join(t.TempDir(), "no-such-dir", "notify.log")
		j := &fakeJournal{}
		_, err := e.Execute(context.Background(), sc(path, 1, j))
		if err == nil {
			t.Fatal("Execute succeeded, want open error")
		}
		if len(j.results) != 0 {
			t.Error("failed effect left a journaled result")
		}
	})
}

// TestConfigAsWrongType pins the executor-vs-step-type mismatch branch: a
// context whose step type decodes to a different struct than the executor
// expects is an *InvalidConfigError, not a silent zero config.
func TestConfigAsWrongType(t *testing.T) {
	t.Parallel()

	_, err := configAs[*dag.SleepConfig](StepContext{
		StepType: dag.StepEcho, Config: json.RawMessage(`{"input": {}}`),
	})
	var ice *InvalidConfigError
	if !errors.As(err, &ice) {
		t.Fatalf("error = %v, want *InvalidConfigError", err)
	}
	if !strings.Contains(err.Error(), "expected") {
		t.Errorf("error = %q, want type-mismatch diagnosis", err)
	}
}
