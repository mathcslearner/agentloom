package llm

import (
	"bytes"
	"encoding/json"
	"fmt"
	"time"
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

// mockScript is the wire shape of a mock provider script. The Sleep seam is
// deliberately omitted (an in-process test concern), but latency, token, and
// injection distributions ARE part of the offline-run contract: a load
// deployment (ticket 19.1) needs the compose fleet's mock to simulate
// realistic provider latency and token accounting. Unknown fields are rejected
// so a typo fails loudly at boot rather than silently disabling a rule.
type mockScript struct {
	// Seed drives the mock's deterministic draws (latency/token/injection
	// lotteries). Carried so a script can pin it explicitly.
	Seed int64 `json:"seed"`
	// Latency is the global latency distribution applied to every call.
	Latency *latencyWire `json:"latency"`
	// Tokens is the global token distribution: successful outcomes draw
	// their reported usage from it instead of the estimator.
	Tokens *tokenWire `json:"tokens"`
	// Inject applies global per-call failure injection.
	Inject *injectWire `json:"inject"`
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
	Latency    *latencyWire    `json:"latency"`
	Tokens     *tokenWire      `json:"tokens"`
	Usage      *usageWire      `json:"usage"`
}

// latencyWire is the wire form of LatencySpec, with durations as Go-duration
// strings ("120ms", "2s").
type latencyWire struct {
	Fixed string `json:"fixed"`
	Min   string `json:"min"`
	Max   string `json:"max"`
	P50   string `json:"p50"`
	P99   string `json:"p99"`
}

func (w *latencyWire) toSpec() (LatencySpec, error) {
	if w == nil {
		return LatencySpec{}, nil
	}
	var (
		spec LatencySpec
		err  error
	)
	if spec.Fixed, err = parseDur("fixed", w.Fixed); err != nil {
		return LatencySpec{}, err
	}
	if spec.Min, err = parseDur("min", w.Min); err != nil {
		return LatencySpec{}, err
	}
	if spec.Max, err = parseDur("max", w.Max); err != nil {
		return LatencySpec{}, err
	}
	if spec.P50, err = parseDur("p50", w.P50); err != nil {
		return LatencySpec{}, err
	}
	if spec.P99, err = parseDur("p99", w.P99); err != nil {
		return LatencySpec{}, err
	}
	return spec, nil
}

func parseDur(field, raw string) (time.Duration, error) {
	if raw == "" {
		return 0, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", field, err)
	}
	return d, nil
}

// tokenWire is the wire form of TokenDist.
type tokenWire struct {
	Input  tokenSpecWire `json:"input"`
	Output tokenSpecWire `json:"output"`
}

type tokenSpecWire struct {
	Fixed int `json:"fixed"`
	Min   int `json:"min"`
	Max   int `json:"max"`
}

func (w *tokenWire) toDist() *TokenDist {
	if w == nil {
		return nil
	}
	return &TokenDist{
		Input:  TokenSpec{Fixed: w.Input.Fixed, Min: w.Input.Min, Max: w.Input.Max},
		Output: TokenSpec{Fixed: w.Output.Fixed, Min: w.Output.Min, Max: w.Output.Max},
	}
}

// injectWire is the wire form of MockInjection.
type injectWire struct {
	Rate429    float64 `json:"rate_429"`
	Rate500    float64 `json:"rate_500"`
	RetryAfter string  `json:"retry_after"`
}

func (w *injectWire) toInjection() (MockInjection, error) {
	if w == nil {
		return MockInjection{}, nil
	}
	ra, err := parseDur("retry_after", w.RetryAfter)
	if err != nil {
		return MockInjection{}, err
	}
	return MockInjection{Rate429: w.Rate429, Rate500: w.Rate500, RetryAfter: ra}, nil
}

// usageWire is an explicit per-outcome token override.
type usageWire struct {
	Input  int64 `json:"input"`
	Output int64 `json:"output"`
}

func (o mockScriptOutcome) toOutcome() (MockOutcome, error) {
	out := MockOutcome{
		Text:       o.Text,
		Structured: o.Structured,
		StopReason: o.StopReason,
		Status:     o.Status,
		Code:       o.Code,
		Message:    o.Message,
		Tokens:     o.Tokens.toDist(),
	}
	if o.Latency != nil {
		spec, err := o.Latency.toSpec()
		if err != nil {
			return MockOutcome{}, err
		}
		out.Latency = &spec
	}
	if o.Usage != nil {
		out.Usage = &Usage{InputTokens: o.Usage.Input, OutputTokens: o.Usage.Output}
	}
	return out, nil
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
	cfg := MockConfig{Seed: s.Seed, Tokens: s.Tokens.toDist()}
	globalLatency, err := s.Latency.toSpec()
	if err != nil {
		return MockConfig{}, fmt.Errorf("llm: parsing mock script: latency: %w", err)
	}
	cfg.Latency = globalLatency
	inject, err := s.Inject.toInjection()
	if err != nil {
		return MockConfig{}, fmt.Errorf("llm: parsing mock script: inject: %w", err)
	}
	cfg.Inject = inject
	for i, r := range s.Rules {
		mr := MockRule{Substring: r.Substring, Pattern: r.Pattern, OnCall: r.OnCall}
		for j, o := range r.Respond {
			oc, err := o.toOutcome()
			if err != nil {
				return MockConfig{}, fmt.Errorf("llm: parsing mock script: rule %d response %d: %w", i, j, err)
			}
			mr.Respond = append(mr.Respond, oc)
		}
		cfg.Rules = append(cfg.Rules, mr)
	}
	if s.Default != nil {
		d, err := s.Default.toOutcome()
		if err != nil {
			return MockConfig{}, fmt.Errorf("llm: parsing mock script: default response: %w", err)
		}
		cfg.Default = &d
	}
	return cfg, nil
}
