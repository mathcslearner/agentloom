package engine

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/mathcslearner/agentloom/internal/dag"
	"github.com/mathcslearner/agentloom/internal/exec"
)

func TestClassifyFailure(t *testing.T) {
	t.Parallel()

	registryMissErr := func() error {
		r, err := exec.NewRegistry()
		if err != nil {
			t.Fatalf("NewRegistry: %v", err)
		}
		_, err = r.Get("no_such_type")
		return err
	}

	invalidConfigErr := func() error {
		// The real shape executors return: DecodeStepConfig failure wrapped
		// in *InvalidConfigError.
		return fmt.Errorf("sleep: %w", &exec.InvalidConfigError{StepType: "sleep"})
	}

	cases := []struct {
		name string
		err  error
		want dag.ErrorClass
	}{
		{"unclassified defaults to transient", errors.New("connection reset by peer"), dag.ClassTransient},
		{"wrapped unclassified defaults to transient", fmt.Errorf("calling tool: %w", context.DeadlineExceeded), dag.ClassTransient},
		{"declared transient honored", exec.Transientf("rate limited"), dag.ClassTransient},
		{"declared permanent honored", exec.Permanentf("no such model"), dag.ClassPermanent},
		{"wrapped declared permanent honored", fmt.Errorf("provider: %w", exec.Permanentf("bad request")), dag.ClassPermanent},
		{"registry miss is permanent (row 4)", registryMissErr(), dag.ClassPermanent},
		{"invalid config is permanent (row 5)", invalidConfigErr(), dag.ClassPermanent},
		{
			"misdeclared reserved class falls back to transient",
			&exec.ClassifiedError{Class: dag.ClassValidationFailed, Err: errors.New("judge rejected")},
			dag.ClassTransient,
		},
		{
			"misdeclared engine-owned class falls back to transient",
			&exec.ClassifiedError{Class: dag.ClassTimeout, Err: errors.New("took too long")},
			dag.ClassTransient,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := classifyFailure(tc.err); got != tc.want {
				t.Errorf("classifyFailure = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestMisdeclaredClass(t *testing.T) {
	t.Parallel()

	if declared, ok := misdeclaredClass(&exec.ClassifiedError{Class: dag.ClassCancelled, Err: errors.New("x")}); !ok || declared != dag.ClassCancelled {
		t.Errorf("misdeclaredClass(cancelled) = (%q, %v), want (cancelled, true)", declared, ok)
	}
	if _, ok := misdeclaredClass(exec.Permanentf("x")); ok {
		t.Error("misdeclaredClass(permanent) reported misuse")
	}
	if _, ok := misdeclaredClass(errors.New("plain")); ok {
		t.Error("misdeclaredClass(plain error) reported misuse")
	}
}

func TestDecodeRetryPolicy(t *testing.T) {
	t.Parallel()

	good := []byte(`{"max_attempts": 3, "backoff": {"initial": "1s", "cap": "1m", "multiplier": 2}, "jitter": "full", "retry_on": ["transient", "timeout"]}`)
	p, err := decodeRetryPolicy(good)
	if err != nil {
		t.Fatalf("decodeRetryPolicy: %v", err)
	}
	if p.MaxAttempts != 3 || p.Backoff.Initial != "1s" || !p.Retries(dag.ClassTimeout) || p.Retries(dag.ClassPermanent) {
		t.Errorf("decoded policy = %+v", p)
	}

	for name, raw := range map[string][]byte{
		"empty":             nil,
		"garbage":           []byte(`{{`),
		"zero max_attempts": []byte(`{"max_attempts": 0}`),
	} {
		if _, err := decodeRetryPolicy(raw); err == nil {
			t.Errorf("decodeRetryPolicy(%s): expected error, got nil", name)
		}
	}
}
