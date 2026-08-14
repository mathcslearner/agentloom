//go:build integration

package engine_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/mathcslearner/agentloom/internal/engine"
	"github.com/mathcslearner/agentloom/internal/exec"
	"github.com/mathcslearner/agentloom/internal/llm"
	"github.com/mathcslearner/agentloom/internal/store"
)

// llmRetryDef: one llm step whose provider fails once (a 429) then
// succeeds, under an explicit deterministic retry policy.
const llmRetryDef = `{
	"schema_version": 1,
	"name": "llm-retry-429",
	"steps": [
		{"id": "call", "type": "llm",
		 "config": {"model": "mock/sim-1", "prompt": "flaky request please", "max_tokens": 64},
		 "retry": {"max_attempts": 3, "backoff": {"initial": "1s", "cap": "1m", "multiplier": 2}, "jitter": "none"}},
		{"id": "after", "type": "noop"}
	],
	"edges": [{"from": "call", "to": "after"}]
}`

// TestLLMProviderTransientRetries is ticket 8.6's injected-429 acceptance:
// the mock provider returns a scripted 429 on the first call and succeeds
// on the second, and the engine retries per the M5 policy — attempt
// history exactly [transient, succeeded], backoff honored on the injected
// clock, and usage recorded only on the successful attempt (a failed
// provider call reports none).
func TestLLMProviderTransientRetries(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s, h, runID := setup(t, llmRetryDef)
	clk := newFakeClock(testNow)
	d := startDispatcher(t, s, h.Queue())

	// A scripted mock: the "flaky"-matching rule returns a 429 first (→
	// transient), then a success. The 429 is exactly the provider transient
	// the retry engine must absorb.
	providers, err := llm.NewRegistryFromKeys(llm.ProviderKeys{Mock: &llm.MockConfig{
		Rules: []llm.MockRule{{
			Substring: "flaky",
			Respond: []llm.MockOutcome{
				{Status: 429, Message: "rate limited"},
				{Text: "recovered"},
			},
		}},
	}})
	if err != nil {
		t.Fatalf("NewRegistryFromKeys: %v", err)
	}
	reg, err := exec.NewRegistry(exec.NewLLMExecutor(providers), exec.NoopExecutor{})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	eng, err := engine.New(s, reg, "worker-a",
		engine.WithDispatchNudge(d.Nudge),
		engine.WithClock(clk.Now),
		engine.WithRetryScheduler(h.Delayed()))
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	h.Spawn("worker-a", eng.Handle, retryWorkerConfig())

	// Attempt 1's 429 classifies transient and schedules retry 1 at t0+1s.
	due1 := testNow.Add(time.Second)
	waitRetryScheduled(t, s, h, runID, "call", due1)

	// Fire the retry; attempt 2 succeeds and the run completes.
	clk.Set(due1)
	if res := h.PromoteDue(ctx, due1, 16); res.Promoted != 1 {
		t.Fatalf("promoted %d entries at due time, want 1", res.Promoted)
	}
	waitRun(t, s, runID, store.RunStatusSucceeded)
	h.WaitQuiescent(ctx)
	h.RequireHandledOncePerClaim()

	requireStepStatuses(t, s, runID, map[string]string{
		"call": store.StepStatusSucceeded, "after": store.StepStatusSucceeded,
	})
	requireAttemptOutcomes(t, s, runID, "call", []string{
		store.AttemptOutcomeTransient, store.StepStatusSucceeded,
	})
	if got := countEvents(t, s, runID, store.EventStepRetryScheduled); got != 1 {
		t.Errorf("step_retry_scheduled events = %d, want 1", got)
	}

	// The successful attempt echoed the scripted success text.
	requireLLMOutputText(t, s, runID, "call", "recovered")

	// Usage: none on the failed attempt (a 429 has no completion), present
	// on the succeeded one — the durable record M10 meters.
	atts, err := s.Attempts().ListByStep(ctx, runID, "call")
	if err != nil {
		t.Fatalf("listing attempts: %v", err)
	}
	if len(atts) != 2 {
		t.Fatalf("attempt count = %d, want 2", len(atts))
	}
	if len(atts[0].Usage) != 0 {
		t.Errorf("failed attempt recorded usage %s, want none", atts[0].Usage)
	}
	var u exec.Usage
	if len(atts[1].Usage) == 0 {
		t.Fatal("succeeded attempt has no usage")
	}
	if err := json.Unmarshal(atts[1].Usage, &u); err != nil {
		t.Fatalf("unmarshaling attempt usage: %v", err)
	}
	if u.InputTokens <= 0 || u.OutputTokens <= 0 {
		t.Errorf("succeeded attempt usage = %+v, want positive token counts", u)
	}
}
