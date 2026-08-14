package tools

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/mathcslearner/agentloom/internal/dag"
)

// invokeJSONTransform is a helper building the args payload and invoking.
func invokeJSONTransform(ctx context.Context, t *testing.T, expr string, input string) (json.RawMessage, error) {
	t.Helper()
	args := map[string]json.RawMessage{"expr": mustJSON(t, expr)}
	if input != "" {
		args["input"] = json.RawMessage(input)
	}
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatalf("marshaling args: %v", err)
	}
	return NewJSONTransform().Invoke(ctx, Invocation{Args: raw})
}

func mustJSON(t *testing.T, s string) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("marshaling string: %v", err)
	}
	return b
}

func TestJSONTransformSuccess(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		expr  string
		input string
		want  string
	}{
		{"path", ".a", `{"a":1,"b":2}`, `1`},
		{"nested path", ".a.b", `{"a":{"b":"deep"}}`, `"deep"`},
		{"construction", "{ query: .topic, source: \"news\" }", `{"topic":"durable"}`, `{"query":"durable","source":"news"}`},
		{"multi-emit array", ".[]", `[1,2,3]`, `[1,2,3]`},
		{"zero-emit becomes empty array", ".[] | select(. > 10)", `[1,2,3]`, `[]`},
		{"absent input is null", ".", "", `null`},
		{"identity object", ".", `{"k":"v"}`, `{"k":"v"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := invokeJSONTransform(context.Background(), t, tc.expr, tc.input)
			if err != nil {
				t.Fatalf("Invoke: %v", err)
			}
			if string(got) != tc.want {
				t.Errorf("output = %s, want %s", got, tc.want)
			}
		})
	}
}

func TestJSONTransformPermanentErrors(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		expr  string
		input string
	}{
		{"compile error", ".a |", `{"a":1}`},
		{"runtime type error", ".a + \"x\"", `{"a":1}`},
		{"halt", "error(\"boom\")", `{}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := invokeJSONTransform(context.Background(), t, tc.expr, tc.input)
			assertPermanent(t, err)
		})
	}
}

func TestJSONTransformEmptyExprPermanent(t *testing.T) {
	t.Parallel()
	// Invoke directly with args the schema would reject, to prove the
	// tool's own presence check (independent of the executor's validation
	// gate) is permanent.
	_, err := NewJSONTransform().Invoke(context.Background(), Invocation{Args: json.RawMessage(`{}`)})
	assertPermanent(t, err)
}

func TestJSONTransformContextCancelPassthrough(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	// A large range under a cancelled ctx surfaces the context error, which
	// must pass through unwrapped (the engine judges cancelled/timeout).
	_, err := invokeJSONTransform(ctx, t, "range(100000000) | . + 1", "")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled to pass through", err)
	}
	var te *Error
	if errors.As(err, &te) {
		t.Errorf("context error was wrapped in a classified *Error: %v", err)
	}
}

// assertPermanent asserts err is a permanent *tools.Error.
func assertPermanent(t *testing.T, err error) {
	t.Helper()
	var te *Error
	if !errors.As(err, &te) {
		t.Fatalf("err = %v (%T), want *tools.Error", err, err)
	}
	if te.Class != dag.ClassPermanent {
		t.Errorf("class = %v, want permanent", te.Class)
	}
}

// assertTransient asserts err is a transient *tools.Error.
func assertTransient(t *testing.T, err error) {
	t.Helper()
	var te *Error
	if !errors.As(err, &te) {
		t.Fatalf("err = %v (%T), want *tools.Error", err, err)
	}
	if te.Class != dag.ClassTransient {
		t.Errorf("class = %v, want transient", te.Class)
	}
}
