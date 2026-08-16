package tokens

import (
	"strconv"

	"github.com/mathcslearner/agentloom/internal/llm"
)

// fallbackVersion bumps if the chars/4 heuristic changes.
const fallbackVersion = 1

// fallbackCounter is the last-resort counter for a provider/model with no
// registered counter: ceil(len(text)/4), never below 1 for non-empty text.
// It is approximate (20–40% off on code/JSON, which is why ADR-014 keeps it
// only as a fallback), but being approximately right beats refusing to count.
// Selection logs the fallback once per (provider, model) per process.
type fallbackCounter struct{}

func newFallbackCounter() fallbackCounter { return fallbackCounter{} }

// Fallback returns the last-resort chars/4 counter directly, without a
// Registry selection or its once-per-model warning. Consumers that need a
// counter for a non-model context — the blackboard store counting a
// non-llm step's entry (ticket 12.2), a default when no model is resolved —
// use it as the honest "we have no model tokenizer here" counter. Its ID()
// is the stable "fallback/chars4@1" fingerprint, so a stored count carries
// the same provenance a Registry fallback would.
func Fallback() Counter { return newFallbackCounter() }

func (fallbackCounter) ID() string { return "fallback/chars4@" + strconv.Itoa(fallbackVersion) }

func (fallbackCounter) Count(text string) int {
	if text == "" {
		return 0
	}
	return (len(text) + 3) / 4
}

func (c fallbackCounter) CountRequest(req llm.ChatRequest) int {
	return countRequest(req, openAIFraming, c.Count)
}
