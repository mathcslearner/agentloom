package llm

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// A mock script is the JSON surface of the deterministic mock provider
// (ticket 8.5), so a deployment can drive the compose/worker mock with
// scripted rules instead of the config-free echo. The in-process MockConfig
// (used by tests) carries clock/latency/func fields that do not belong on a
// wire contract; this file exposes only the subset a scripted offline run
// needs — matching rules and their response sequences — and compiles it into
// a MockConfig via NewMock's ordinary validation. It is the AGENTLOOM_PRICING
// / AGENTLOOM_RESOURCES precedent: a leaf-owned strict parser the deployables
// call, the config package only carrying the source string.
//
// The flagship research→write→critique example (ticket 14.5) is the motivating
// consumer: its writer⇄critic loop only iterates when the critic returns a
// `{"verdict":"revise"}` structured response, which the stock echo mock never
// produces — so a scripted mock is what makes real loop iterations visible on
// the compose stack (and in the narrative doc's captured output).

// mockScript is the wire shape of a mock provider script. Durations, latency,
// injection, and the Sleep seam are deliberately omitted: they are in-process
// test concerns, not part of the offline-run contract. Unknown fields are
// rejected so a typo fails loudly at boot rather than silently disabling a
// rule.
type mockScript struct {
	// Seed drives the mock's deterministic draws (unused without latency/
	// injection, but carried so a script can pin it explicitly).
	Seed int64 `json:"seed"`
	// Rules are matched in declaration order against each request; first match
	// wins. Empty falls through to Default (or the built-in echo).
	Rules []mockScriptRule `json:"rules"`
	// Default is the outcome when no rule matches; omitted means the built-in
	// echo.
	Default *mockScriptOutcome `json:"default"`
}

// mockScriptRule mirrors MockRule's matchable fields.
type mockScriptRule struct {
	Substring string              `json:"substring"`
	Pattern   string              `json:"pattern"`
	OnCall    int                 `json:"on_call"`
	Respond   []mockScriptOutcome `json:"respond"`
}

// mockScriptOutcome mirrors the success/error fields of MockOutcome that a
// scripted offline run needs. A non-zero Status makes it a scripted error.
type mockScriptOutcome struct {
	Text       string          `json:"text"`
	Structured json.RawMessage `json:"structured"`
	StopReason string          `json:"stop_reason"`
	Status     int             `json:"status"`
	Code       string          `json:"code"`
	Message    string          `json:"message"`
}

func (o mockScriptOutcome) toOutcome() MockOutcome {
	return MockOutcome{
		Text:       o.Text,
		Structured: o.Structured,
		StopReason: o.StopReason,
		Status:     o.Status,
		Code:       o.Code,
		Message:    o.Message,
	}
}

// ParseMockScript decodes a mock script JSON document into a MockConfig,
// rejecting unknown fields. It does not validate the rules' semantics — that
// is NewMock's job (a script with a bad regex or an empty response sequence
// fails there), so callers should build the provider via NewMock on the
// result and surface any error at boot.
func ParseMockScript(data []byte) (MockConfig, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var s mockScript
	if err := dec.Decode(&s); err != nil {
		return MockConfig{}, fmt.Errorf("llm: parsing mock script: %w", err)
	}
	if dec.More() {
		return MockConfig{}, fmt.Errorf("llm: parsing mock script: trailing data after JSON document")
	}
	cfg := MockConfig{Seed: s.Seed}
	for _, r := range s.Rules {
		mr := MockRule{Substring: r.Substring, Pattern: r.Pattern, OnCall: r.OnCall}
		for _, o := range r.Respond {
			mr.Respond = append(mr.Respond, o.toOutcome())
		}
		cfg.Rules = append(cfg.Rules, mr)
	}
	if s.Default != nil {
		d := s.Default.toOutcome()
		cfg.Default = &d
	}
	return cfg, nil
}
