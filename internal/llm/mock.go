package llm

import (
	"context"
	"fmt"
	"math/rand/v2"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/mathcslearner/agentloom/internal/dag"
	"github.com/mathcslearner/agentloom/internal/plugin"
)

// ProviderMock is the mock provider's plugin name. It is addressed
// through the 8.4 registry's namespace form (`model: "mock/<model>"`),
// so it needs no vendor-prefix entry in the routing table.
const ProviderMock = "mock"

// Mock (ticket 8.5) is a deterministic, offline Provider for tests and
// load: it makes no network call, holds no HTTP client, and reads no
// clock of its own. Given a seed and a script it produces a byte-identical
// transcript across runs — the workhorse M19's load tests and every CI
// suite that exercises an llm step run on, cheaply and reproducibly.
//
// It honors the full Provider contract exactly as the real providers do:
// req.Validate failures and scripted/injected HTTP statuses surface as
// classified *Error through the shared, provider-agnostic taxonomy
// (classifyStatus); context cancellation during a simulated latency wait
// passes through unclassified so the engine keeps the timeout/cancel
// judgment (ADR-006 rows 3/8); and Usage is present on every success.
//
// The struct has no field that could hold a secret — there is nothing to
// authenticate against — so its errors are hygienic by construction, like
// the HTTP providers'.
type Mock struct {
	rules   []compiledRule
	def     MockOutcome
	inject  MockInjection
	latency LatencySpec
	sleep   func(ctx context.Context, d time.Duration) error

	mu    sync.Mutex
	rng   *rand.Rand
	calls int64 // 1-based global call ordinal, for OnCall matching
	seqs  []int // per-rule sequence cursor into Respond, parallel to rules
}

// MockConfig scripts a Mock. The zero value is valid: a mock with no
// rules and no injection deterministically echoes the last user message
// (the behavior chained llm steps need to pass real data through
// templating with zero scripting).
type MockConfig struct {
	// Seed drives every random draw (injection lotteries, latency
	// sampling). Two mocks with the same seed, driven through the same
	// sequence of calls, produce identical transcripts.
	Seed int64
	// Rules are matched in declaration order against each request; the
	// first match wins. An empty slice always falls through to Default.
	Rules []MockRule
	// Default is the outcome when no rule matches; the zero MockOutcome
	// means the built-in echo (see makeDefaultOutcome).
	Default *MockOutcome
	// Inject applies failure injection to every call, evaluated before
	// rule matching so an injected 429/500 can pre-empt any scripted
	// response.
	Inject MockInjection
	// Latency is the global latency distribution, applied to every call
	// unless the matched outcome overrides it.
	Latency LatencySpec
	// Sleep waits for d or until ctx is done, returning ctx.Err() on
	// cancellation. Nil installs a real ctx-aware timer; tests inject a
	// recording or instantaneous implementation to keep time controlled.
	Sleep func(ctx context.Context, d time.Duration) error
}

// MockRule is one scripted response rule: a match predicate plus the
// sequence of outcomes to return on successive matches.
type MockRule struct {
	// Substring matches when the request's flattened prompt text contains
	// it. Empty means this clause does not constrain the match.
	Substring string
	// Pattern is a regular expression matched against the flattened prompt
	// text. Empty means this clause does not constrain the match.
	Pattern string
	// OnCall matches when the global 1-based call ordinal equals it. Zero
	// means this clause does not constrain the match.
	OnCall int
	// Respond is the outcome sequence for successive matches of this rule:
	// the Nth match returns Respond[N-1], and the last entry is sticky
	// (every match past the end repeats it). A rule must declare at least
	// one outcome.
	Respond []MockOutcome
}

// MockOutcome is one scripted response. A non-zero Status makes it a
// scripted *Error; otherwise it is a successful ChatResponse.
type MockOutcome struct {
	// Text is a convenience for a single text block. Ignored when Blocks
	// is set.
	Text string
	// Blocks is the full response content (e.g. tool_use scripting).
	// Overrides Text when non-empty.
	Blocks []Block
	// StopReason is the response stop reason; empty defaults to "end_turn"
	// (or "tool_use" when the blocks carry a tool call).
	StopReason string
	// Usage overrides the token accounting; nil uses the deterministic
	// estimator (estimateUsage).
	Usage *Usage
	// Status, when non-zero, makes this outcome a scripted *Error with
	// this HTTP status (429, 500, ...), classified through classifyStatus.
	Status int
	// Code is the provider error code on a scripted error; empty defaults
	// per status (see scriptedError).
	Code string
	// Message is the error message on a scripted error.
	Message string
	// RetryAfter rides a scripted error's Error.RetryAfter (M9
	// backpressure input); meaningful only when Status is set.
	RetryAfter time.Duration
	// Hang blocks on ctx until it is cancelled, ignoring latency — fuel
	// for timeout/cancellation tests (mirrors the queuetest Script hang).
	Hang bool
	// Latency overrides the global latency distribution for this outcome.
	Latency *LatencySpec
}

// MockInjection configures per-call failure injection. Rates are
// probabilities in [0, 1]; 429 is evaluated before 500.
type MockInjection struct {
	// Rate429 is the probability of injecting a transient 429 rate-limit
	// error on any given call.
	Rate429 float64
	// Rate500 is the probability of injecting a transient 500 error on any
	// given call (evaluated only when a 429 was not injected).
	Rate500 float64
	// RetryAfter rides an injected 429's Error.RetryAfter.
	RetryAfter time.Duration
}

// LatencySpec is a latency distribution. Fixed, when non-zero, is a
// constant wait; otherwise a wait is drawn uniformly from [Min, Max]
// (both zero means no wait).
type LatencySpec struct {
	Fixed time.Duration
	Min   time.Duration
	Max   time.Duration
}

// validate checks the spec's shape; construction rejects a malformed one.
func (l LatencySpec) validate() error {
	if l.Fixed < 0 || l.Min < 0 || l.Max < 0 {
		return fmt.Errorf("latency durations must be non-negative (fixed=%s min=%s max=%s)", l.Fixed, l.Min, l.Max)
	}
	if l.Max < l.Min {
		return fmt.Errorf("latency max %s is below min %s", l.Max, l.Min)
	}
	return nil
}

// draw samples a wait from the spec using rng. Fixed wins; otherwise a
// uniform draw from [Min, Max].
func (l LatencySpec) draw(rng *rand.Rand) time.Duration {
	if l.Fixed > 0 {
		return l.Fixed
	}
	if l.Max <= l.Min {
		return l.Min
	}
	span := int64(l.Max - l.Min)
	return l.Min + time.Duration(rng.Int64N(span))
}

// compiledRule is a MockRule with its regex compiled and matcher
// predicates resolved once at construction.
type compiledRule struct {
	substring string
	re        *regexp.Regexp
	onCall    int
	respond   []MockOutcome
}

// matches reports whether the rule matches a prompt at the given global
// call ordinal. An empty rule (no clauses) matches everything; multiple
// clauses are ANDed.
func (r compiledRule) matches(prompt string, call int64) bool {
	if r.substring != "" && !strings.Contains(prompt, r.substring) {
		return false
	}
	if r.re != nil && !r.re.MatchString(prompt) {
		return false
	}
	if r.onCall != 0 && int64(r.onCall) != call {
		return false
	}
	return true
}

// NewMock builds a mock provider, rejecting a malformed script at
// construction (the analogue of the HTTP providers' boot-time key check)
// so a mis-scripted test or load config fails loudly, not mid-run.
func NewMock(cfg MockConfig) (*Mock, error) {
	if err := cfg.Latency.validate(); err != nil {
		return nil, fmt.Errorf("llm: mock: %w", err)
	}
	if cfg.Inject.Rate429 < 0 || cfg.Inject.Rate429 > 1 || cfg.Inject.Rate500 < 0 || cfg.Inject.Rate500 > 1 {
		return nil, fmt.Errorf("llm: mock: injection rates must be in [0,1] (429=%g 500=%g)", cfg.Inject.Rate429, cfg.Inject.Rate500)
	}
	rules := make([]compiledRule, 0, len(cfg.Rules))
	for i, mr := range cfg.Rules {
		if len(mr.Respond) == 0 {
			return nil, fmt.Errorf("llm: mock: rule %d declares no responses", i)
		}
		cr := compiledRule{substring: mr.Substring, onCall: mr.OnCall, respond: mr.Respond}
		if mr.Pattern != "" {
			re, err := regexp.Compile(mr.Pattern)
			if err != nil {
				return nil, fmt.Errorf("llm: mock: rule %d pattern %q: %w", i, mr.Pattern, err)
			}
			cr.re = re
		}
		if mr.OnCall < 0 {
			return nil, fmt.Errorf("llm: mock: rule %d on_call %d is negative", i, mr.OnCall)
		}
		for j, o := range mr.Respond {
			if err := o.Latency.validateOptional(); err != nil {
				return nil, fmt.Errorf("llm: mock: rule %d response %d: %w", i, j, err)
			}
		}
		rules = append(rules, cr)
	}
	if cfg.Default != nil {
		if err := cfg.Default.Latency.validateOptional(); err != nil {
			return nil, fmt.Errorf("llm: mock: default response: %w", err)
		}
	}
	def := makeDefaultOutcome(cfg.Default)
	sleep := cfg.Sleep
	if sleep == nil {
		sleep = realSleep
	}
	return &Mock{
		rules:   rules,
		def:     def,
		inject:  cfg.Inject,
		latency: cfg.Latency,
		sleep:   sleep,
		//nolint:gosec // G404: a seeded PRNG is the point — deterministic simulation, never security-sensitive.
		rng:  rand.New(rand.NewPCG(uint64(cfg.Seed), uint64(cfg.Seed)^0x9e3779b97f4a7c15)),
		seqs: make([]int, len(rules)),
	}, nil
}

// validateOptional validates a per-outcome latency override, tolerating
// the zero spec (no override).
func (l *LatencySpec) validateOptional() error {
	if l == nil {
		return nil
	}
	return l.validate()
}

// makeDefaultOutcome resolves the no-rule-matched outcome: the caller's
// override, or the built-in echo when none is supplied. The echo is
// marked so it is distinguishable from a scripted text response.
func makeDefaultOutcome(override *MockOutcome) MockOutcome {
	if override != nil {
		return *override
	}
	return MockOutcome{} // sentinel: Chat echoes the prompt (see resolveOutcome)
}

// Manifest implements Provider (ADR-009). Version 1.0.0, not a stub
// version: the mock is a permanent fixture of the system (tests, load),
// never replaced in place. Cacheable (its output is a pure function of
// its config and the request, so M9 caching is sound) but deliberately
// NOT cost-bearing — the whole point is a free, offline provider — and
// not side-effectful.
func (m *Mock) Manifest() plugin.Manifest {
	return plugin.Manifest{
		Kind:         plugin.KindModelProvider,
		Name:         ProviderMock,
		Version:      "1.0.0",
		Description:  "Deterministic offline simulation provider for tests and load.",
		Capabilities: plugin.Capabilities{Cacheable: true},
	}
}

// Chat implements Provider deterministically and offline. The whole draw
// sequence (call counter, injection lottery, latency sample, rule
// cursor) advances under one lock so a given seed and call order yield a
// reproducible transcript; the latency wait itself happens outside the
// lock so concurrent callers are not serialized on it.
func (m *Mock) Chat(ctx context.Context, req ChatRequest) (ChatResponse, error) {
	if err := req.Validate(); err != nil {
		// Decided before any draw or counter advance, exactly as the HTTP
		// providers decide it before any round-trip: an invalid request
		// fails identically every time.
		return ChatResponse{}, &Error{
			Provider: ProviderMock,
			Class:    dag.ClassPermanent,
			Code:     "client_invalid_request",
			Message:  err.Error(),
		}
	}

	prompt := flattenPrompt(req)

	m.mu.Lock()
	m.calls++
	call := m.calls
	injected := m.drawInjection()
	outcome, hang := m.resolveOutcome(prompt, call, injected)
	wait := m.drawLatency(outcome)
	m.mu.Unlock()

	// Simulate latency (or an indefinite hang) outside the lock, honoring
	// ctx. A cancellation here passes through unclassified — the engine
	// owns the timeout/cancel judgment.
	if hang {
		<-ctx.Done()
		return ChatResponse{}, fmt.Errorf("llm: mock: request aborted while hanging: %w", ctx.Err())
	}
	if wait > 0 {
		if err := m.sleep(ctx, wait); err != nil {
			return ChatResponse{}, fmt.Errorf("llm: mock: request aborted: %w", err)
		}
	}

	if outcome.Status != 0 {
		return ChatResponse{}, scriptedError(outcome)
	}
	return buildResponse(req, prompt, outcome), nil
}

// drawInjection returns a scripted-error outcome when the per-call
// injection lottery fires, or a zero outcome otherwise. Called under the
// lock. 429 is evaluated before 500.
func (m *Mock) drawInjection() MockOutcome {
	if m.inject.Rate429 > 0 && m.rng.Float64() < m.inject.Rate429 {
		return MockOutcome{Status: 429, Code: "rate_limit_error", Message: "injected rate limit", RetryAfter: m.inject.RetryAfter}
	}
	if m.inject.Rate500 > 0 && m.rng.Float64() < m.inject.Rate500 {
		return MockOutcome{Status: 500, Code: "api_error", Message: "injected server error"}
	}
	return MockOutcome{}
}

// resolveOutcome selects the outcome for this call: an injected error
// pre-empts everything; otherwise the first matching rule (advancing its
// sticky sequence cursor), else the default. Called under the lock.
// Returns the outcome and whether it is a hang.
func (m *Mock) resolveOutcome(prompt string, call int64, injected MockOutcome) (MockOutcome, bool) {
	if injected.Status != 0 {
		return injected, false
	}
	for i := range m.rules {
		if !m.rules[i].matches(prompt, call) {
			continue
		}
		respond := m.rules[i].respond
		idx := m.seqs[i]
		if idx >= len(respond)-1 {
			idx = len(respond) - 1 // sticky last
		} else {
			m.seqs[i]++
		}
		o := respond[idx]
		return o, o.Hang
	}
	return m.def, m.def.Hang
}

// drawLatency samples the wait for an outcome: its override, else the
// global spec. Called under the lock (it advances the RNG).
func (m *Mock) drawLatency(o MockOutcome) time.Duration {
	if o.Hang {
		return 0
	}
	spec := m.latency
	if o.Latency != nil {
		spec = *o.Latency
	}
	return spec.draw(m.rng)
}

// scriptedError builds the *Error for a scripted or injected non-zero
// status, classifying it through the shared provider-agnostic taxonomy.
func scriptedError(o MockOutcome) *Error {
	code := o.Code
	msg := o.Message
	if code == "" {
		switch o.Status {
		case 429:
			code = "rate_limit_error"
		case 408:
			code = "timeout"
		default:
			code = "api_error"
		}
	}
	if msg == "" {
		msg = fmt.Sprintf("mock scripted %d", o.Status)
	}
	return &Error{
		Provider:   ProviderMock,
		Class:      classifyStatus(o.Status),
		Status:     o.Status,
		Code:       code,
		Message:    msg,
		RetryAfter: o.RetryAfter,
	}
}

// buildResponse assembles the ChatResponse for a successful outcome. A
// zero outcome (the built-in default) echoes the last user text.
func buildResponse(req ChatRequest, prompt string, o MockOutcome) ChatResponse {
	blocks := o.Blocks
	if len(blocks) == 0 {
		text := o.Text
		if text == "" {
			text = "[mock] " + lastUserText(req)
		}
		blocks = []Block{TextBlock(text)}
	}
	stop := o.StopReason
	if stop == "" {
		stop = "end_turn"
		for _, b := range blocks {
			if b.Type == BlockToolUse {
				stop = "tool_use"
				break
			}
		}
	}
	usage := estimateUsage(prompt, blocks)
	if o.Usage != nil {
		usage = *o.Usage
	}
	return ChatResponse{Model: req.Model, StopReason: stop, Blocks: blocks, Usage: usage}
}

// estimateUsage produces plausible, deterministic token counts (~4 chars
// per token) so load tests get realistic accounting without a tokenizer.
func estimateUsage(prompt string, blocks []Block) Usage {
	out := 0
	for _, b := range blocks {
		switch b.Type {
		case BlockText:
			out += len(b.Text)
		case BlockToolUse:
			if b.ToolUse != nil {
				out += len(b.ToolUse.Name) + len(b.ToolUse.Input)
			}
		}
	}
	return Usage{
		InputTokens:  int64(len(prompt)/4) + 1,
		OutputTokens: int64(out/4) + 1,
	}
}

// flattenPrompt concatenates the request's system prompt and every
// message's text and tool-result content into the single string rules
// match against. The exact shape is documented as the mock's match
// surface so scripts are stable across engine changes.
func flattenPrompt(req ChatRequest) string {
	var b strings.Builder
	if req.System != "" {
		b.WriteString(req.System)
		b.WriteByte('\n')
	}
	for _, m := range req.Messages {
		for _, blk := range m.Blocks {
			switch blk.Type {
			case BlockText:
				b.WriteString(blk.Text)
				b.WriteByte('\n')
			case BlockToolResult:
				if blk.ToolResult != nil {
					b.WriteString(blk.ToolResult.Content)
					b.WriteByte('\n')
				}
			}
		}
	}
	return b.String()
}

// lastUserText returns the last user message's concatenated text, the
// echo target for the built-in default response.
func lastUserText(req ChatRequest) string {
	for i := len(req.Messages) - 1; i >= 0; i-- {
		if req.Messages[i].Role != RoleUser {
			continue
		}
		var b strings.Builder
		for _, blk := range req.Messages[i].Blocks {
			if blk.Type == BlockText {
				b.WriteString(blk.Text)
			}
		}
		if b.Len() > 0 {
			return b.String()
		}
	}
	return ""
}

// realSleep is the default latency implementation: wait for d or until
// ctx is done. The timer is the only clock the mock touches, and only
// through this injectable seam — logic under test installs a controlled
// one (the injectable-time invariant).
func realSleep(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// compile-time guard: Mock satisfies Provider.
var _ Provider = (*Mock)(nil)
