package exec

import (
	"context"
	"math"
	"testing"

	"github.com/mathcslearner/agentloom/internal/dag"
	"github.com/mathcslearner/agentloom/internal/llm"
)

// TestEstimatorErrorImproves is the before/after capture the ticket asks for
// (12.6): it compares M9.2's chars/4 input-token estimate against the real
// token counter (ticket 12.6) over a spread of requests, using the mock
// provider's reported InputTokens as ground truth. The mock's counter mirrors
// the mock provider's own input estimator exactly, so the counter error is
// zero by construction, while chars/4 (over the raw prompt, ignoring request
// framing) is systematically off. The test asserts the counter is never worse
// than chars/4 and is exact on the mock — the property that lets the M9.3
// estimate-error histogram tighten to zero on the offline fleet.
func TestEstimatorErrorImproves(t *testing.T) {
	t.Parallel()

	mock, err := llm.NewMock(llm.MockConfig{})
	if err != nil {
		t.Fatalf("NewMock: %v", err)
	}
	reg, err := llm.NewRegistry(mock)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	e := NewLLMExecutor(reg)

	cases := []struct {
		name string
		cfg  *dag.LLMConfig
	}{
		{"short prompt", &dag.LLMConfig{Model: "mock/sim-1", Prompt: "Hello there", MaxTokens: 64}},
		{"long prompt", &dag.LLMConfig{Model: "mock/sim-1", Prompt: "The quick brown fox jumps over the lazy dog. " +
			"Summarize the preceding sentence in one word, then explain your reasoning at length.", MaxTokens: 128}},
		{"multi-turn", &dag.LLMConfig{Model: "mock/sim-1", MaxTokens: 100, Messages: []dag.LLMMessage{
			{Role: "user", Content: "What is the capital of France?"},
			{Role: "assistant", Content: "The capital of France is Paris."},
			{Role: "user", Content: "And of Germany?"},
		}}},
	}

	var charsErr, counterErr float64
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req, berr := buildChatRequest(tc.cfg)
			if berr != nil {
				t.Fatalf("buildChatRequest: %v", berr)
			}
			// Ground truth: the mock's own reported input usage.
			req.Model = "sim-1"
			resp, cerr := mock.Chat(context.Background(), req)
			if cerr != nil {
				t.Fatalf("mock Chat: %v", cerr)
			}
			truth := resp.Usage.InputTokens

			chars := estimateLLMInputTokensChars(tc.cfg)
			counter := e.estimateInputTokens("mock", "sim-1", tc.cfg)

			ce := math.Abs(float64(counter - truth))
			che := math.Abs(float64(chars - truth))
			if ce > che {
				t.Errorf("counter error %v worse than chars/4 error %v (truth=%d counter=%d chars=%d)",
					ce, che, truth, counter, chars)
			}
			if counter != truth {
				t.Errorf("counter estimate %d != mock ground truth %d (counter must be exact on the mock)", counter, truth)
			}
			counterErr += ce
			charsErr += che
			t.Logf("%s: truth=%d counter=%d (err=%v) chars/4=%d (err=%v)", tc.name, truth, counter, ce, chars, che)
		})
	}
	t.Logf("aggregate abs error — chars/4=%v counter=%v", charsErr, counterErr)
	if counterErr > charsErr {
		t.Errorf("counter aggregate error %v worse than chars/4 %v", counterErr, charsErr)
	}
}
