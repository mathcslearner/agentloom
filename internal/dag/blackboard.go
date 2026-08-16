package dag

// This file is the definition-contract half of ADR-014's blackboard: the
// optional per-step `blackboard` block an author carries on the step
// envelope to declare writes to the run-scoped blackboard, and the bounds
// Validate enforces on it. The runtime half — the store, the token-counted
// entry, the write applied in the step's completion transaction — lives in
// internal/blackboard and the engine (ticket 12.2); this package only
// defines and validates the authored field.
//
// 12.2 delivers declarative *writes*. Declarative *reads into a prompt* are
// context assembly (12.3, ADR-014's context sources), which selects
// blackboard entries by key/tag and assembles them into the provider
// request; programmatic reads are available now through StepContext.

// MaxBlackboardWrites bounds the declarative writes one step may declare.
const MaxBlackboardWrites = 16

// BlackboardWrite is one declarative write: on the step's success, the value
// at From within the step's own output is stored under Key with the given
// tags (plus the reserved pinned tag when Pinned is set). It is the sugar
// for the common "publish this step's result to shared memory" pattern
// without a bespoke executor.
type BlackboardWrite struct {
	// Key is the blackboard key to write (the blackboard key grammar:
	// letters, digits, '_' and '-'; no dots). Required.
	Key string `json:"key"`

	// From is an RFC 6901 JSON pointer into the step's own output selecting
	// the value to store; empty ("" — the default) stores the whole output.
	// "/text" is the natural choice for an llm step (its completion text).
	From string `json:"from,omitempty"`

	// Tags are labels attached to the written version (the blackboard tag
	// grammar). Empty means no tags.
	Tags []string `json:"tags,omitempty"`

	// Pinned, when true, adds the reserved "pinned" tag so no compaction
	// strategy (12.4) may drop or truncate the entry — sugar for including
	// "pinned" in Tags.
	Pinned bool `json:"pinned,omitempty"`
}

// BlackboardPolicy is a step's authored blackboard block (ADR-014); nil on
// the step envelope means the `blackboard` key was absent (no declarative
// writes). Uniform across step types, so it lives on the step envelope, not
// in the per-type config — like cache and validation.
type BlackboardPolicy struct {
	// Write is the ordered list of declarative writes applied on the step's
	// success. Empty/absent means the block declares no writes (an authoring
	// mistake — Validate rejects an empty block, like an empty cache block).
	Write []BlackboardWrite `json:"write,omitempty"`
}
