package exec

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/mathcslearner/agentloom/internal/dag"
	"github.com/mathcslearner/agentloom/internal/plugin"
	"github.com/mathcslearner/agentloom/internal/tools"
)

// spyTool is a tools.Tool that records the invocation and returns a
// scripted result/error, for tool-executor coverage.
type spyTool struct {
	name    string
	invoked *bool
	lastInv *tools.Invocation
	result  json.RawMessage
	err     error
}

// spyToolSchema requires a string field "x", so a payload without it fails
// the pre-invocation validation gate.
const spyToolSchema = `{"type":"object","properties":{"x":{"type":"string"}},"required":["x"],"additionalProperties":false}`

func (s spyTool) Manifest() plugin.Manifest {
	return plugin.Manifest{
		Kind:         plugin.KindTool,
		Name:         s.name,
		Version:      "1.0.0",
		ConfigSchema: json.RawMessage(spyToolSchema),
	}
}

func (s spyTool) Invoke(_ context.Context, inv tools.Invocation) (json.RawMessage, error) {
	if s.invoked != nil {
		*s.invoked = true
	}
	if s.lastInv != nil {
		*s.lastInv = inv
	}
	return s.result, s.err
}

// toolStepConfig builds a `tool` step config selecting tool with the given
// input (args) JSON.
func toolStepConfig(tool, input string) json.RawMessage {
	if input == "" {
		return json.RawMessage(`{"tool":"` + tool + `"}`)
	}
	return json.RawMessage(`{"tool":"` + tool + `","input":` + input + `}`)
}

func runToolExec(t *testing.T, reg *tools.Registry, cfg json.RawMessage, key string) (Output, error) {
	t.Helper()
	return NewToolExecutor(reg).Execute(context.Background(), StepContext{
		StepType:       dag.StepTool,
		Config:         cfg,
		IdempotencyKey: key,
	})
}

func TestToolExecNilRegistryPermanent(t *testing.T) {
	t.Parallel()
	_, err := runToolExec(t, nil, toolStepConfig("json_transform", `{"x":"ok"}`), "")
	assertClassPermanent(t, err)
}

func TestToolExecUnknownToolPermanent(t *testing.T) {
	t.Parallel()
	reg, _ := tools.NewRegistry(spyTool{name: "known"})
	_, err := runToolExec(t, reg, toolStepConfig("missing", `{"x":"ok"}`), "")
	assertClassPermanent(t, err)
	if !errors.Is(err, tools.ErrUnknownTool) {
		t.Errorf("err = %v, want ErrUnknownTool reachable", err)
	}
}

func TestToolExecMissingToolFieldPermanent(t *testing.T) {
	t.Parallel()
	reg, _ := tools.NewRegistry(spyTool{name: "t"})
	_, err := runToolExec(t, reg, json.RawMessage(`{}`), "")
	if !errors.Is(err, ErrInvalidConfig) {
		t.Errorf("err = %v, want ErrInvalidConfig", err)
	}
}

func TestToolExecInvalidArgsNeverInvokes(t *testing.T) {
	t.Parallel()
	var invoked bool
	reg, _ := tools.NewRegistry(spyTool{name: "t", invoked: &invoked})
	// Missing required "x" — the args schema rejects it.
	_, err := runToolExec(t, reg, toolStepConfig("t", `{"y":1}`), "")
	assertClassPermanent(t, err)
	if !errors.Is(err, tools.ErrInvalidArgs) {
		t.Errorf("err = %v, want ErrInvalidArgs reachable", err)
	}
	if invoked {
		t.Error("tool was invoked despite invalid args — the gate must block the call")
	}
}

func TestToolExecVerbatimOutputAndKey(t *testing.T) {
	t.Parallel()
	var inv tools.Invocation
	reg, _ := tools.NewRegistry(spyTool{name: "t", lastInv: &inv, result: json.RawMessage(`{"ok":true}`)})
	out, err := runToolExec(t, reg, toolStepConfig("t", `{"x":"hi"}`), "key-abc")
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if string(out.Data) != `{"ok":true}` {
		t.Errorf("output = %s, want the tool result verbatim", out.Data)
	}
	if inv.IdempotencyKey != "key-abc" {
		t.Errorf("idempotency key = %q, want key-abc", inv.IdempotencyKey)
	}
	if string(inv.Args) != `{"x":"hi"}` {
		t.Errorf("args = %s, want the step input", inv.Args)
	}
}

func TestToolExecClassPassthrough(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		err  error
		want dag.ErrorClass
	}{
		{"transient", &tools.Error{Tool: "t", Class: dag.ClassTransient, Message: "flaky"}, dag.ClassTransient},
		{"permanent", &tools.Error{Tool: "t", Class: dag.ClassPermanent, Message: "bad"}, dag.ClassPermanent},
		{"host not allowed", &tools.HostNotAllowedError{Host: "x"}, dag.ClassPermanent},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			reg, _ := tools.NewRegistry(spyTool{name: "t", err: tc.err})
			_, err := runToolExec(t, reg, toolStepConfig("t", `{"x":"ok"}`), "")
			var ce *ClassifiedError
			if !errors.As(err, &ce) {
				t.Fatalf("err = %v, want *ClassifiedError", err)
			}
			if ce.Class != tc.want {
				t.Errorf("class = %v, want %v", ce.Class, tc.want)
			}
		})
	}
}

func TestToolExecContextErrorPassthrough(t *testing.T) {
	t.Parallel()
	reg, _ := tools.NewRegistry(spyTool{name: "t", err: context.Canceled})
	_, err := runToolExec(t, reg, toolStepConfig("t", `{"x":"ok"}`), "")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled to pass through", err)
	}
	var ce *ClassifiedError
	if errors.As(err, &ce) {
		t.Errorf("context error wrapped in *ClassifiedError: %v", err)
	}
}

// assertClassPermanent asserts err carries a permanent classification.
func assertClassPermanent(t *testing.T, err error) {
	t.Helper()
	var ce *ClassifiedError
	if !errors.As(err, &ce) {
		t.Fatalf("err = %v (%T), want *ClassifiedError", err, err)
	}
	if ce.Class != dag.ClassPermanent {
		t.Errorf("class = %v, want permanent", ce.Class)
	}
}
