package dag

// This file is the definition-contract half of ADR-014's context assembly:
// the optional per-step `context` block an author carries on the step
// envelope to declare the sources assembled into the provider request before
// the call, and the bounds Validate enforces on it. The runtime half — the
// assembly, per-source token caps, pinning, the missing-source policy, and
// the pre-flight token total — lives in internal/contextmgr and the engine
// (ticket 12.3); this package only defines and validates the authored field.
//
// 12.3 assembles the request from the sources; 12.4 compacts it when it
// exceeds the step's budget. Templating inside the block (`${{ ... }}`) is
// deliberately not supported — a dynamic query flows through a `retrieve`
// step and a `step_output` source, keeping this ticket out of the template
// engine (the 12.2 stance).

// MaxContextSources bounds the sources one step may assemble.
const MaxContextSources = 32

// MaxContextTopK bounds a retrieval source's top_k, mirroring the retrieve
// executor's cap so a context retrieval and a `retrieve` step agree.
const MaxContextTopK = 100

// ContextSourceKind is the discriminator of a context source.
type ContextSourceKind string

// Context source kinds (ADR-014). Order in a spec is precedence and message
// order: an earlier source wins a token-budget contest (12.4) and appears
// first in the assembled request.
const (
	// SourceStepOutput pulls a named upstream step's output (optionally a JSON
	// pointer into it) — the 8.2 templating ref, as a context source.
	SourceStepOutput ContextSourceKind = "step_output"
	// SourceBlackboard selects run-scoped blackboard entries by key (the head)
	// or by tag (every head carrying all listed tags).
	SourceBlackboard ContextSourceKind = "blackboard"
	// SourceRetrieval runs a retriever query and includes the ranked results.
	SourceRetrieval ContextSourceKind = "retrieval"
	// SourceLiteral is an inline constant (a system-prompt fragment,
	// instructions) included verbatim.
	SourceLiteral ContextSourceKind = "literal"
)

var contextSourceKinds = []ContextSourceKind{
	SourceStepOutput, SourceBlackboard, SourceRetrieval, SourceLiteral,
}

// ContextMissingPolicy governs what happens when a source resolves to nothing
// (an upstream step that did not succeed, a blackboard key with no head, a
// tag matching no heads, a retriever returning no results).
type ContextMissingPolicy string

// Missing-source policies (ADR-014). The default is Error — the 8.2
// strict-reference stance: a source an author names is expected to exist, and
// a silent omission would ship a subtly wrong prompt.
const (
	// MissingError fails the step permanently before any provider call.
	MissingError ContextMissingPolicy = "error"
	// MissingSkip omits the source and records it in the context_assembled
	// audit event, letting the step proceed.
	MissingSkip ContextMissingPolicy = "skip"
)

var contextMissingPolicies = []ContextMissingPolicy{MissingError, MissingSkip}

// ContextSource is one source in a step's ordered context spec. Exactly one
// kind's fields are meaningful; Validate rejects fields that do not belong to
// the declared kind.
type ContextSource struct {
	// Kind selects which of the fields below apply. Required.
	Kind ContextSourceKind `json:"kind"`

	// Name is the label the assembled request wraps this source under (a
	// stable header the model and the audit event see). Optional; when empty
	// the assembler derives "<kind>#<index>". When set it must be unique
	// within the spec.
	Name string `json:"name,omitempty"`

	// Step is the upstream step id whose output this source pulls
	// (step_output only). Required for step_output; must be a normal-edge
	// ancestor of the declaring step (the 8.2 upstream rule).
	Step string `json:"step,omitempty"`

	// Path is an RFC 6901 JSON pointer into the referenced step's output
	// (step_output only); empty ("" — the default) selects the whole output.
	// "/text" is the natural choice for an upstream llm step.
	Path string `json:"path,omitempty"`

	// Key selects a single blackboard entry's head by key (blackboard only).
	// Mutually exclusive with Tags; exactly one of Key/Tags is required for a
	// blackboard source.
	Key string `json:"key,omitempty"`

	// Tags selects every blackboard head carrying all listed tags (blackboard
	// only, AND semantics — the store's ListFilter). Mutually exclusive with
	// Key.
	Tags []string `json:"tags,omitempty"`

	// Retriever names the retriever to query (retrieval only). Required for
	// retrieval.
	Retriever string `json:"retriever,omitempty"`

	// Query is the (static) query text (retrieval only). Required for
	// retrieval. A dynamic query flows through a `retrieve` step and a
	// step_output source — templating in the context block is not supported.
	Query string `json:"query,omitempty"`

	// TopK bounds the retrieval results (retrieval only); nil defaults to the
	// retrieve executor's default (5), capped at its maximum (100).
	TopK *int `json:"top_k,omitempty"`

	// Text is the inline literal (literal only). Required for literal.
	Text string `json:"text,omitempty"`

	// MaxTokens caps this source's contribution; nil means no per-source cap.
	// When set, the assembler truncates the source's rendered text to the cap
	// with an explicit elision marker. Mutually exclusive with Pinned (a
	// pinned source is never truncated).
	MaxTokens *int `json:"max_tokens,omitempty"`

	// Pinned, when true, exempts this source from every compaction strategy
	// (12.4) — it is always included, never truncated. Mutually exclusive with
	// MaxTokens.
	Pinned bool `json:"pinned,omitempty"`

	// OnMissing governs a source that resolves to nothing; empty defaults to
	// "error".
	OnMissing ContextMissingPolicy `json:"on_missing,omitempty"`
}

// EffectiveMissingPolicy resolves OnMissing to its default (error) when unset.
func (s ContextSource) EffectiveMissingPolicy() ContextMissingPolicy {
	if s.OnMissing == "" {
		return MissingError
	}
	return s.OnMissing
}

// ContextSpec is a step's authored context block (ADR-014); nil on the step
// envelope means the `context` key was absent (the request is built from the
// config alone). Uniform across the llm family, so it lives on the step
// envelope, not in the per-type config — like cache and validation.
type ContextSpec struct {
	// Sources is the ordered list assembled into the provider request. Order
	// is precedence and message order. Empty/absent means the block declares
	// no sources (an authoring mistake — Validate rejects an empty block, like
	// an empty blackboard block).
	Sources []ContextSource `json:"sources,omitempty"`
}
