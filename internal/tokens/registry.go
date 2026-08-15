package tokens

import (
	"log/slog"
	"sync"

	"github.com/mathcslearner/agentloom/internal/llm"
)

// Family names a counter's tokenization family, reported on a Selection so
// callers (and metrics) can see which counter served a model without parsing
// the ID.
type Family string

// The counter families a Selection reports.
const (
	FamilyOpenAI    Family = "openai"
	FamilyAnthropic Family = "anthropic"
	FamilyMock      Family = "mock"
	FamilyFallback  Family = "fallback"
)

// Selection describes the counter Select returned: its family, the resolved
// (provider, model) it was chosen for, and whether it is the chars/4 fallback
// (an unknown model). Callers persist Counter.ID() alongside a stored count;
// Selection is for observability and the fallback decision.
type Selection struct {
	Family   Family
	Provider string
	Model    string
	Fallback bool
}

// Registry selects a token Counter per resolved (provider, model). It holds no
// per-model state beyond the memoized encodings (shared process-wide) and the
// once-per-model fallback-log guard, so a single Registry is safe to share
// across the fleet and cheap to construct.
//
// The provider name passed to Select is the RESOLVED provider — the one
// llm.Registry.Resolve returns — because model→provider routing already lives
// in internal/llm and is not duplicated here. Counters for a family are pure
// and independent of the specific model (except OpenAI's encoding, chosen by
// model prefix), so Select builds them on demand rather than caching per model.
type Registry struct {
	log *slog.Logger

	// fallbackLogged guards the "fell back to chars/4" warning to once per
	// (provider, model) per process (ADR-014: never per call).
	fallbackLogged sync.Map // map[string]*sync.Once
}

// NewRegistry builds a token-counter registry. A nil logger disables the
// fallback warning (the counters themselves never log).
func NewRegistry(log *slog.Logger) *Registry {
	return &Registry{log: log}
}

// Select returns the Counter for a resolved (provider, model) and a Selection
// describing the choice. It never returns nil: an unroutable or unknown model
// yields the chars/4 fallback counter (logged once per model). An internal
// error building a family's counter (an embedded-encoding load failure — a
// programming error, not a runtime condition) also falls back rather than
// panicking, so counting a request can never crash a step.
func (r *Registry) Select(provider, model string) (Counter, Selection) {
	switch provider {
	case llm.ProviderOpenAI:
		if c, err := newOpenAICounter(model); err == nil {
			return c, Selection{Family: FamilyOpenAI, Provider: provider, Model: model}
		}
	case llm.ProviderAnthropic:
		if c, err := newAnthropicCounter(); err == nil {
			return c, Selection{Family: FamilyAnthropic, Provider: provider, Model: model}
		}
	case llm.ProviderMock:
		return newMockCounter(), Selection{Family: FamilyMock, Provider: provider, Model: model}
	}
	return r.fallback(provider, model)
}

func (r *Registry) fallback(provider, model string) (Counter, Selection) {
	r.logFallbackOnce(provider, model)
	return newFallbackCounter(), Selection{Family: FamilyFallback, Provider: provider, Model: model, Fallback: true}
}

// logFallbackOnce emits the fallback warning at most once per (provider,
// model) per process.
func (r *Registry) logFallbackOnce(provider, model string) {
	if r.log == nil {
		return
	}
	key := provider + "\x00" + model
	onceAny, _ := r.fallbackLogged.LoadOrStore(key, &sync.Once{})
	onceAny.(*sync.Once).Do(func() {
		r.log.Warn("tokens: no counter for model; using chars/4 fallback",
			slog.String("provider", provider),
			slog.String("model", model),
		)
	})
}
