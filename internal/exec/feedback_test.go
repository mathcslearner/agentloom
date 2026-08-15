package exec

import (
	"encoding/json"
	"testing"

	"github.com/mathcslearner/agentloom/internal/cache"
	"github.com/mathcslearner/agentloom/internal/dag"
)

// TestLLMWithFeedbackPromptForm appends the critique to a prompt-form config
// and leaves every other value byte-identical.
func TestLLMWithFeedbackPromptForm(t *testing.T) {
	t.Parallel()
	e := recExecutor(t, &recordingProvider{resp: okResponse()})

	sc := llmStep(t, map[string]any{
		"model": "rec/sim-1", "prompt": "draft the intro", "max_tokens": 321, "temperature": 0.0,
	})
	out, err := e.WithFeedback(sc, Feedback{Text: "the intro was too short"})
	if err != nil {
		t.Fatalf("WithFeedback: %v", err)
	}
	var got, orig map[string]json.RawMessage
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("unmarshal rewritten: %v", err)
	}
	if err := json.Unmarshal(sc.Config, &orig); err != nil {
		t.Fatalf("unmarshal original: %v", err)
	}
	var prompt string
	if err := json.Unmarshal(got["prompt"], &prompt); err != nil {
		t.Fatalf("unmarshal prompt: %v", err)
	}
	if prompt != "draft the intro\n\nthe intro was too short" {
		t.Errorf("prompt = %q, want the original plus the critique", prompt)
	}
	for k, v := range orig {
		if k == "prompt" {
			continue
		}
		if string(got[k]) != string(v) {
			t.Errorf("field %q changed: %s -> %s", k, v, got[k])
		}
	}
}

// TestLLMWithFeedbackMessagesForm appends a trailing user message and leaves
// the prior messages intact.
func TestLLMWithFeedbackMessagesForm(t *testing.T) {
	t.Parallel()
	e := recExecutor(t, &recordingProvider{resp: okResponse()})

	sc := llmStep(t, map[string]any{
		"model": "rec/sim-1",
		"messages": []map[string]any{
			{"role": "system", "content": "be terse"},
			{"role": "user", "content": "draft the intro"},
		},
	})
	out, err := e.WithFeedback(sc, Feedback{Text: "too short, revise"})
	if err != nil {
		t.Fatalf("WithFeedback: %v", err)
	}
	var got struct {
		Messages []dag.LLMMessage `json:"messages"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("unmarshal rewritten: %v", err)
	}
	if len(got.Messages) != 3 {
		t.Fatalf("messages len = %d, want 3", len(got.Messages))
	}
	last := got.Messages[2]
	if last.Role != "user" || last.Content != "too short, revise" {
		t.Errorf("trailing message = %+v, want a user critique", last)
	}
	if got.Messages[1].Content != "draft the intro" {
		t.Errorf("prior user message changed: %+v", got.Messages[1])
	}
}

// TestWithFeedbackEmptyTextIsNoOp returns the config unchanged when the
// feedback carries no text (a step type that injects nothing).
func TestWithFeedbackEmptyTextIsNoOp(t *testing.T) {
	t.Parallel()
	e := recExecutor(t, &recordingProvider{resp: okResponse()})
	sc := llmStep(t, map[string]any{"model": "rec/sim-1", "prompt": "hi"})
	out, err := e.WithFeedback(sc, Feedback{Text: "  "})
	if err != nil {
		t.Fatalf("WithFeedback: %v", err)
	}
	if string(out) != string(sc.Config) {
		t.Errorf("expected config unchanged; got %s", out)
	}
}

// TestFeedbackChangesCacheKey is the ticket-11.4 assertion that an augmented
// prompt yields a different cache key — a semantic retry is cache-bypassed by
// construction.
func TestFeedbackChangesCacheKey(t *testing.T) {
	t.Parallel()
	e := recExecutor(t, &recordingProvider{resp: okResponse()})

	sc := llmStep(t, map[string]any{"model": "rec/sim-1", "prompt": "identical prompt", "temperature": 0.0})
	primary, err := e.CacheBinding(sc)
	if err != nil {
		t.Fatalf("CacheBinding primary: %v", err)
	}
	rewritten, err := e.WithFeedback(sc, Feedback{Text: "the critique"})
	if err != nil {
		t.Fatalf("WithFeedback: %v", err)
	}
	sc.Config = rewritten
	augmented, err := e.CacheBinding(sc)
	if err != nil {
		t.Fatalf("CacheBinding augmented: %v", err)
	}

	kp, err := cache.Key(cache.KeyInput{SchemaVersion: 1, Executor: primary.Executor, Plugin: primary.Plugin, Request: primary.Request})
	if err != nil {
		t.Fatalf("Key primary: %v", err)
	}
	ka, err := cache.Key(cache.KeyInput{SchemaVersion: 1, Executor: augmented.Executor, Plugin: augmented.Plugin, Request: augmented.Request})
	if err != nil {
		t.Fatalf("Key augmented: %v", err)
	}
	if kp == ka {
		t.Errorf("feedback did not change the cache key: both = %s", kp)
	}
}
