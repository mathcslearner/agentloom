package cost

import (
	"fmt"
	"strings"
	"time"
)

// Source records how a model price was resolved, for the ledger's rate
// provenance and the ADR-012 warning event.
type Source int

const (
	// SourceExact is an entry keyed by the model's exact resource name.
	SourceExact Source = iota
	// SourceWildcard is the "<provider>:*" entry, matched when no exact name
	// did.
	SourceWildcard
	// SourceFallback is the catalog fallback, used when neither a name nor a
	// wildcard matched (unknown model, estimate policy).
	SourceFallback
)

// String renders the source for logs and event payloads.
func (s Source) String() string {
	switch s {
	case SourceExact:
		return "exact"
	case SourceWildcard:
		return "wildcard"
	case SourceFallback:
		return "fallback"
	default:
		return fmt.Sprintf("Source(%d)", int(s))
	}
}

// UnknownModelPolicy selects what pre-flight pricing does when a model has no
// catalog entry (ADR-012). Post-call ledger pricing never fails a
// succeeded attempt — the caller there always passes PolicyEstimate, since the
// tokens were already spent and refusing to price them would understate spend.
type UnknownModelPolicy int

const (
	// PolicyEstimate prices an unknown model at the catalog fallback rate and
	// signals a warning (Priced.Fallback). The default.
	PolicyEstimate UnknownModelPolicy = iota
	// PolicyFail refuses to price an unknown model, returning
	// *UnknownModelError. 10.3 uses this pre-flight to block a call to an
	// unpriced model before any money is spent (a deterministic failure).
	PolicyFail
)

// ParseUnknownModelPolicy maps a policy string ("estimate" | "fail", or ""
// for the default) to the typed enum. cmd/worker calls it to map the
// validated config string onto the engine's claim-time budget check (ticket
// 10.3). An unrecognized value returns PolicyEstimate and an error; config
// already validates the string at load, so cmd/worker treats an error here as
// a boot failure.
func ParseUnknownModelPolicy(s string) (UnknownModelPolicy, error) {
	switch s {
	case "estimate", "":
		return PolicyEstimate, nil
	case "fail":
		return PolicyFail, nil
	default:
		return PolicyEstimate, fmt.Errorf("cost: unknown unknown-model policy %q (want estimate or fail)", s)
	}
}

// Priced is a resolved model price with its provenance.
type Priced struct {
	// Rate is the price to meter the attempt at.
	Rate Rate
	// Source records how Rate was resolved.
	Source Source
	// Fallback is true when the model was unknown and priced at the catalog
	// fallback (Source == SourceFallback): the caller should emit the
	// cost_unknown_model warning event.
	Fallback bool
}

// Lookup resolves a model resource name to its price effective at time at:
// exact name first, then the "<provider>:*" wildcard (provider = the segment
// before the first colon, matching internal/limits), then not-found. Within a
// name, the newest entry whose effective_from is on or before at wins; a name
// whose entries are all future-dated resolves as not-found. Nil-safe.
func (c *Catalog) Lookup(name string, at time.Time) (Rate, Source, bool) {
	if c == nil {
		return Rate{}, SourceExact, false
	}
	if r, ok := selectModel(c.models[name], at); ok {
		return r.Rate, SourceExact, true
	}
	if i := strings.IndexByte(name, ':'); i >= 0 {
		if r, ok := selectModel(c.models[name[:i]+":*"], at); ok {
			return r.Rate, SourceWildcard, true
		}
	}
	return Rate{}, SourceExact, false
}

// PriceModel resolves a model price, applying the unknown-model policy when
// neither an exact nor a wildcard entry matches. A known model returns its
// price with Fallback false; an unknown model returns the fallback rate with
// Fallback true under PolicyEstimate, or *UnknownModelError under PolicyFail.
// A catalog with no fallback configured (only reachable via a direct Parse,
// never via Load/Default) also returns *UnknownModelError, since there is
// nothing to estimate with.
func (c *Catalog) PriceModel(name string, at time.Time, policy UnknownModelPolicy) (Priced, error) {
	if r, src, ok := c.Lookup(name, at); ok {
		return Priced{Rate: r, Source: src}, nil
	}
	if policy == PolicyFail {
		return Priced{}, &UnknownModelError{Model: name}
	}
	fb, ok := c.Fallback()
	if !ok {
		return Priced{}, &UnknownModelError{Model: name}
	}
	return Priced{Rate: fb, Source: SourceFallback, Fallback: true}, nil
}

// ContextWindow resolves a model resource's context window (in tokens)
// effective at time at, using the same exact → "<provider>:*" wildcard → miss
// resolution Lookup uses (ADR-014, ticket 12.6). It returns the window, the
// source it resolved from, and whether a positive window was found: a resource
// with no entry, a wildcard without a window, or an entry whose window is unset
// (0) all return ok=false — the guardrail then treats the model as unguarded
// (nothing to compare against). The catalog fallback carries a rate but no
// window, so an unknown model is deliberately unguarded. Nil-safe.
func (c *Catalog) ContextWindow(name string, at time.Time) (int64, Source, bool) {
	if c == nil {
		return 0, SourceExact, false
	}
	if e, ok := selectModel(c.models[name], at); ok && e.ContextWindow > 0 {
		return e.ContextWindow, SourceExact, true
	}
	if i := strings.IndexByte(name, ':'); i >= 0 {
		if e, ok := selectModel(c.models[name[:i]+":*"], at); ok && e.ContextWindow > 0 {
			return e.ContextWindow, SourceWildcard, true
		}
	}
	return 0, SourceExact, false
}

// ToolPrice resolves a priced tool's per-call cost in nano-USD, effective at
// time at. A tool with no entry is free — false, cost zero — with no policy or
// warning (unpriced tools are legitimately free). Nil-safe.
func (c *Catalog) ToolPrice(name string, at time.Time) (int64, bool) {
	if c == nil {
		return 0, false
	}
	if e, ok := selectTool(c.tools[name], at); ok {
		return ToolCost(e.PerCallUSD), true
	}
	return 0, false
}

// selectModel returns the newest model entry effective on or before at. The
// slice is sorted effective_from descending, so the first entry not after at
// is the answer.
func selectModel(entries []ModelEntry, at time.Time) (ModelEntry, bool) {
	for _, e := range entries {
		if !e.EffectiveFrom.After(at) {
			return e, true
		}
	}
	return ModelEntry{}, false
}

// selectTool is selectModel for tool entries.
func selectTool(entries []ToolEntry, at time.Time) (ToolEntry, bool) {
	for _, e := range entries {
		if !e.EffectiveFrom.After(at) {
			return e, true
		}
	}
	return ToolEntry{}, false
}

// UnknownModelError is returned by PriceModel for a model with no catalog
// entry under PolicyFail (or when no fallback is configured). It carries an
// ADR-006 semantics hint for 10.3: an unpriced model under fail-closed is a
// deterministic misconfiguration, so the pre-flight failure is permanent.
type UnknownModelError struct {
	Model string
}

func (e *UnknownModelError) Error() string {
	return fmt.Sprintf("cost: no pricing for model %q", e.Model)
}
