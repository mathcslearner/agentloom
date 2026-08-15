package cost

// EventTypeUnknownModel is the run-event type appended when a priced attempt's
// model has no catalog entry and the estimate policy priced it at the fallback
// rate (ADR-012). It is the value the store's event vocabulary registers as
// `store.EventCostUnknownModel` when the physical Events().Append lands in the
// 10.2 ledger transaction — 10.1 defines only the type string and payload
// contract, since there is no ledger/attempt-completion transaction to append
// under yet.
const EventTypeUnknownModel = "cost_unknown_model"

// UnknownModelWarning is the cost_unknown_model event payload: the unpriced
// model and the fallback rate it was priced at. The run/step/attempt context
// is carried by the event envelope (the append is per-run with a monotonic
// seq), so the payload names only what is specific to the warning. The
// operator reads it as "add a catalog entry for this model, or accept the
// conservative fallback estimate".
type UnknownModelWarning struct {
	// Model is the unpriced model resource name (e.g. "openai:gpt-6").
	Model string `json:"model"`
	// Fallback is the catalog fallback rate the attempt was priced at.
	Fallback Rate `json:"fallback"`
}

// NewUnknownModelWarning builds the warning payload for a fallback-priced
// attempt. Callers guard on Priced.Fallback before constructing one.
func NewUnknownModelWarning(model string, fallback Rate) UnknownModelWarning {
	return UnknownModelWarning{Model: model, Fallback: fallback}
}
