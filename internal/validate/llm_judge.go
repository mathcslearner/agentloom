package validate

// The llm_judge validator (ticket 11.5, ADR-013): the first cost-bearing
// built-in. It grades a step output against an author-supplied rubric by
// calling a (cheap) judge model, parsing a {score, rationale} answer, and
// passing iff the score meets a threshold. It is the sharpest expression of
// differentiator #1 — a semantic quality gate whose failing rationale feeds
// 11.4's feedback-augmented semantic retry.
//
// Cost & guardrails (ADR-012/013):
//   - The judge's provider call is attributed to the serving step as
//     OVERHEAD (ADR-012 rule 4). The validator reports its token usage on
//     the verdict's per-validator result (and on a billed error), and the
//     engine ledgers it — the validator never touches the ledger itself.
//   - Judges are TERMINAL: a validator's output is never itself validated or
//     judged, and the chain never recurses (ADR-013). This validator makes a
//     single provider call and returns a verdict; there is no nested chain.
//   - Cheap-first ordering (the Chain runner) means the judge runs only when
//     every deterministic validator already passed — no paying a judge to
//     grade an output a free schema check already rejected.
//
// Provider-error policy is configurable per ADR-013: `on_error: fail`
// (default) turns a judge that cannot render a verdict into a transport
// failure of the validation stage (the ADR-006 retry engine then decides),
// while `on_error: skip` degrades to a PASS with a warning so an unavailable
// judge does not block a workflow. A fallback model chain is tried on
// provider errors before the policy fires.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/mathcslearner/agentloom/internal/jsonrepair"
	"github.com/mathcslearner/agentloom/internal/llm"
	"github.com/mathcslearner/agentloom/internal/plugin"
)

// The on_error policy values (ADR-013): fail (default) routes a judge error
// as a transport failure of the validation stage; skip degrades to a pass
// with a warning.
const (
	judgeOnErrorFail = "fail"
	judgeOnErrorSkip = "skip"
)

// Judge defaults and caps. The judge is meant to be cheap and quick: a small
// completion budget, a low temperature for a stable grade, and its own
// timeout (the validate stage is not covered by the step timeout, so the
// judge bounds its own call).
const (
	judgeDefaultMaxTokens = 512
	judgeDefaultTimeout   = 60 * time.Second
	judgeMaxTimeout       = 10 * time.Minute
	judgeDefaultOutputMax = 8000
	judgeRationaleMax     = 2000
)

// judgeSystemPrompt instructs the judge to grade against the rubric and
// answer only as JSON. Kept deliberately terse and content-free so the same
// prompt serves every rubric; the rubric and the judged output ride in the
// user message.
const judgeSystemPrompt = "You are a strict output-quality judge. " +
	"Grade the CANDIDATE OUTPUT against the RUBRIC. " +
	"Respond with ONLY a JSON object of the form " +
	`{"score": <number between 0 and 1>, "rationale": "<one or two sentences on why>"}. ` +
	"A score of 1 means the output fully satisfies the rubric; 0 means it does not satisfy it at all. " +
	"Do not include any text outside the JSON object."

// judgeAnswerSchema is the JSON Schema requested via provider-native
// structured output (Anthropic forced tool-use, OpenAI response_format) so a
// provider that honors it returns exactly the answer shape; the deterministic
// JSON-repair pass is the fallback for providers that answer in free text.
var judgeAnswerSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "score": {"type": "number", "minimum": 0, "maximum": 1},
    "rationale": {"type": "string"}
  },
  "required": ["score", "rationale"],
  "additionalProperties": false
}`)

// judgeResponseFormat is the constant structured-output request (the schema
// does not depend on the validator's config).
var judgeResponseFormat = &llm.ResponseFormat{Schema: judgeAnswerSchema, Name: "agentloom_judge_verdict"}

// judgeConfig is llm_judge's config. Model, Rubric, and Threshold are
// required (non-omitempty ⇒ required in the generated schema); the rest have
// defaults.
type judgeConfig struct {
	// Model is the judge model, routed through the same provider registry as
	// an llm step (e.g. "mock/judge-1"). Required.
	Model string `json:"model"`
	// FallbackModels is an ordered cheapening/availability chain tried when
	// the primary (and each earlier fallback) returns a provider error.
	// Distinct from the primary and from each other.
	FallbackModels []string `json:"fallback_models,omitempty"`
	// Rubric is the grading criteria the judge scores against. Required,
	// non-blank. Static text — the feedback/rubric surface is deliberately
	// not templated.
	Rubric string `json:"rubric"`
	// Threshold is the minimum score to pass, in [0, 1]. Required. A score
	// >= Threshold passes; below fails.
	Threshold float64 `json:"threshold"`
	// MaxTokens bounds the judge completion; 0 uses judgeDefaultMaxTokens.
	MaxTokens int `json:"max_tokens,omitempty"`
	// Temperature is the judge sampling temperature; nil defaults to 0 (a
	// deterministic grade).
	Temperature *float64 `json:"temperature,omitempty"`
	// Timeout bounds the judge call (Go duration, e.g. "60s"); empty uses
	// judgeDefaultTimeout, capped at judgeMaxTimeout.
	Timeout string `json:"timeout,omitempty"`
	// OnError selects the provider-error policy: "fail" (default) or "skip".
	OnError string `json:"on_error,omitempty"`
	// MaxOutputChars truncates the judged output in the prompt (0 uses
	// judgeDefaultOutputMax) — a runaway output cannot inflate the judge call.
	MaxOutputChars int `json:"max_output_chars,omitempty"`
	// MaxRationaleChars truncates the stored rationale (0 uses
	// judgeRationaleMax).
	MaxRationaleChars int `json:"max_rationale_chars,omitempty"`
}

// judgeArtifact is the compiled, validated config: the resolved timeout, the
// model chain, and the effective defaults — built once per distinct config
// through the compile cache and reused across attempts.
type judgeArtifact struct {
	cfg          judgeConfig
	models       []string // primary + fallbacks, in order
	timeout      time.Duration
	onError      string
	maxTokens    int
	outputMax    int
	rationaleMax int
}

// LLMJudge is the built-in llm_judge validator: cacheable + cost_bearing. It
// holds the provider registry it routes the judge model through and a
// compileCache of validated configs.
type LLMJudge struct {
	providers *llm.Registry
	cache     *compileCache[judgeArtifact]
}

// NewLLMJudge builds the llm_judge validator over the given provider
// registry. A nil registry is valid at construction (the manifest and config
// schema are still served); every config then fails the pre-flight routing
// gate, since no model can resolve — the "keyless build" behavior the llm
// executor already has.
func NewLLMJudge(providers *llm.Registry) *LLMJudge {
	v := &LLMJudge{providers: providers}
	v.cache = newCompileCache(v.compile)
	return v
}

// Manifest implements Validator: the first cost_bearing built-in.
func (v *LLMJudge) Manifest() plugin.Manifest {
	return plugin.Manifest{
		Kind:         plugin.KindValidator,
		Name:         llmJudgeName,
		Version:      llmJudgeVersion,
		Description:  "Grade a step output against a rubric with a (cheap) LLM judge; pass when the judge's score meets the threshold.",
		Capabilities: judgeCaps,
		ConfigSchema: builtinConfigSchema(&judgeConfig{}),
	}
}

// CompileConfig implements ConfigCompiler: the pre-flight gate (ADR-013). A
// missing/blank rubric, an out-of-range threshold, a duplicate fallback, an
// unparseable/oversized timeout, a bad on_error, or a model that does not
// route against the registry is a permanent config error fired at claim,
// BEFORE the productive step spends any money. Success warms the cache.
func (v *LLMJudge) CompileConfig(config json.RawMessage) error {
	_, err := v.cache.get(config)
	return err
}

// compile is the pure builder behind the cache: decode, validate the config's
// content, and verify every model (primary + fallbacks) resolves against the
// registry so an unroutable judge is a claim-time permanent error rather than
// a mid-run surprise.
func (v *LLMJudge) compile(config []byte) (judgeArtifact, error) {
	var cfg judgeConfig
	if err := strictDecodeConfig(config, &cfg); err != nil {
		return judgeArtifact{}, fmt.Errorf("decoding config: %v", err)
	}
	if cfg.Model == "" {
		return judgeArtifact{}, fmt.Errorf("%q is required", "model")
	}
	if strings.TrimSpace(cfg.Rubric) == "" {
		return judgeArtifact{}, fmt.Errorf("%q is required and must be non-blank", "rubric")
	}
	if cfg.Threshold < 0 || cfg.Threshold > 1 {
		return judgeArtifact{}, fmt.Errorf("%q must be in [0, 1], got %v", "threshold", cfg.Threshold)
	}
	models := []string{cfg.Model}
	seen := map[string]bool{cfg.Model: true}
	for _, m := range cfg.FallbackModels {
		if strings.TrimSpace(m) == "" {
			return judgeArtifact{}, fmt.Errorf("fallback_models entries must be non-empty")
		}
		if seen[m] {
			return judgeArtifact{}, fmt.Errorf("fallback_models entry %q duplicates the primary model or an earlier fallback", m)
		}
		seen[m] = true
		models = append(models, m)
	}
	onErr := cfg.OnError
	if onErr == "" {
		onErr = judgeOnErrorFail
	}
	if onErr != judgeOnErrorFail && onErr != judgeOnErrorSkip {
		return judgeArtifact{}, fmt.Errorf("%q must be %q or %q, got %q", "on_error", judgeOnErrorFail, judgeOnErrorSkip, onErr)
	}
	timeout := judgeDefaultTimeout
	if cfg.Timeout != "" {
		d, err := time.ParseDuration(cfg.Timeout)
		if err != nil {
			return judgeArtifact{}, fmt.Errorf("%q is not a valid duration: %v", "timeout", err)
		}
		if d <= 0 || d > judgeMaxTimeout {
			return judgeArtifact{}, fmt.Errorf("%q must be positive and <= %s, got %s", "timeout", judgeMaxTimeout, d)
		}
		timeout = d
	}
	maxTokens := cfg.MaxTokens
	if maxTokens == 0 {
		maxTokens = judgeDefaultMaxTokens
	}
	if maxTokens < 1 {
		return judgeArtifact{}, fmt.Errorf("%q must be positive, got %d", "max_tokens", maxTokens)
	}
	// Routing gate: every model must resolve now (nil registry ⇒ no routable
	// providers ⇒ permanent, the llm executor's keyless behavior).
	if v.providers == nil {
		return judgeArtifact{}, fmt.Errorf("no model providers configured — this build cannot run an llm_judge")
	}
	for _, m := range models {
		if _, _, err := v.providers.Resolve("", m); err != nil {
			return judgeArtifact{}, fmt.Errorf("model %q does not route: %v", m, err)
		}
	}
	outputMax := cfg.MaxOutputChars
	if outputMax == 0 {
		outputMax = judgeDefaultOutputMax
	}
	rationaleMax := cfg.MaxRationaleChars
	if rationaleMax == 0 {
		rationaleMax = judgeRationaleMax
	}
	return judgeArtifact{
		cfg: cfg, models: models, timeout: timeout, onError: onErr,
		maxTokens: maxTokens, outputMax: outputMax, rationaleMax: rationaleMax,
	}, nil
}

// Validate calls the judge model over the targeted output and renders a
// verdict. On a provider error it walks the fallback chain, then applies the
// on_error policy. A context cancellation/deadline from the caller passes
// through unwrapped so the engine keeps the timeout/cancelled judgment.
func (v *LLMJudge) Validate(ctx context.Context, in Input) (Verdict, error) {
	art, err := v.cache.get(in.Config)
	if err != nil {
		// The config compiled at claim pre-flight; a failure here means the
		// materialized config changed under us — permanent, like cel's.
		return Verdict{}, Permanentf(llmJudgeName, err, "config no longer compiles")
	}

	answer := stringOf(in.Value)
	if art.outputMax > 0 {
		answer = truncateRunes(answer, art.outputMax)
	}
	temp := 0.0
	if art.cfg.Temperature != nil {
		temp = *art.cfg.Temperature
	}
	req := llm.ChatRequest{
		System:         judgeSystemPrompt,
		Messages:       []llm.Message{llm.UserText(buildJudgePrompt(art.cfg.Rubric, answer))},
		MaxTokens:      art.maxTokens,
		Temperature:    &temp,
		ResponseFormat: judgeResponseFormat,
	}

	resp, usage, callErr := v.callJudge(ctx, art, req)
	if callErr != nil {
		// Caller cancellation/deadline: propagate unwrapped (ADR-006 rows 3/8).
		if ctx.Err() != nil {
			return Verdict{}, ctx.Err()
		}
		// Every model in the chain errored — availability failure, no usage
		// billed. Apply the on_error policy.
		return v.applyOnError(art, "judge provider call failed", callErr, nil)
	}

	score, rationale, ok := parseJudgeAnswer(resp)
	if !ok {
		// The judge answered but the answer was not a parseable {score,
		// rationale} — a rubric/model problem, not availability, so no
		// fallback. The call BILLED, so its usage must still be metered.
		return v.applyOnError(art, "judge produced a malformed answer", errors.New("unparseable judge answer"), usage)
	}
	rationale = truncateRunes(strings.TrimSpace(rationale), art.rationaleMax)

	result := ValidatorResult{
		Name:      llmJudgeName,
		Score:     &score,
		Rationale: rationale,
		Usage:     usage,
	}
	if score >= art.cfg.Threshold {
		result.Status = StatusPass
		return Verdict{
			SchemaVersion: VerdictSchemaVersion, Status: StatusPass,
			Score: &score, Results: []ValidatorResult{result},
		}, nil
	}
	// Below threshold: a fail verdict whose rationale is the critique 11.4's
	// feedback builder folds into the next attempt's prompt.
	result.Status = StatusFail
	result.IssueCount = 1
	issue := Issue{
		Validator: llmJudgeName, Code: "rubric_below_threshold",
		Message: judgeFailMessage(score, art.cfg.Threshold, rationale),
	}
	return Verdict{
		SchemaVersion: VerdictSchemaVersion, Status: StatusFail,
		Score: &score, Issues: []Issue{issue}, Results: []ValidatorResult{result},
	}, nil
}

// callJudge tries the model chain in order, returning the first successful
// response and its usage. It falls back on provider errors and on the judge's
// own timeout; a caller-context cancellation stops immediately (the returned
// error still carries the ctx error, which Validate detects via ctx.Err()).
func (v *LLMJudge) callJudge(ctx context.Context, art judgeArtifact, req llm.ChatRequest) (llm.ChatResponse, *ValidatorUsage, error) {
	var lastErr error
	for _, m := range art.models {
		provider, served, rerr := v.providers.Resolve("", m)
		if rerr != nil {
			// Pre-flight resolved these already; unreachable in practice.
			lastErr = rerr
			continue
		}
		r := req
		r.Model = served
		cctx, cancel := context.WithTimeout(ctx, art.timeout)
		resp, err := provider.Chat(cctx, r)
		cancel()
		if err == nil {
			servedModel := resp.Model
			if servedModel == "" {
				servedModel = served
			}
			usage := &ValidatorUsage{
				Resource:     provider.Manifest().Name + ":" + servedModel,
				Model:        servedModel,
				InputTokens:  resp.Usage.InputTokens,
				OutputTokens: resp.Usage.OutputTokens,
			}
			return resp, usage, nil
		}
		// A caller cancellation/deadline is not the judge's failure — stop and
		// let Validate propagate it unwrapped.
		if ctx.Err() != nil {
			return llm.ChatResponse{}, nil, ctx.Err()
		}
		lastErr = err
	}
	return llm.ChatResponse{}, nil, lastErr
}

// applyOnError turns a judge failure into either a transport failure of the
// stage (on_error: fail → a classified *Error the engine routes through
// ADR-006) or a suppressed pass (on_error: skip → a PASS verdict whose sole
// result records the error). billedUsage is non-nil only when the judge call
// billed before failing (a malformed answer); it rides onto both branches so
// the engine meters the spend as overhead either way.
func (v *LLMJudge) applyOnError(art judgeArtifact, msg string, cause error, billedUsage *ValidatorUsage) (Verdict, error) {
	if art.onError == judgeOnErrorSkip {
		result := ValidatorResult{
			Name: llmJudgeName, Status: StatusError, Usage: billedUsage,
			Error: msg,
		}
		// A pass verdict — the chain does not fail — but the per-validator
		// result records the suppressed error (StatusError).
		return Verdict{
			SchemaVersion: VerdictSchemaVersion, Status: StatusPass,
			Results: []ValidatorResult{result},
		}, nil
	}
	// on_error: fail — a transport failure of the validation stage. A
	// provider availability failure is transient (retry may reach a healthy
	// provider); a malformed answer (billedUsage != nil) is permanent (the
	// same model on the same output produces the same shape). The billed
	// usage rides on the error so the engine meters it as overhead.
	var e *Error
	if billedUsage != nil {
		e = Permanentf(llmJudgeName, cause, "%s", msg)
	} else {
		e = Transientf(llmJudgeName, cause, "%s", msg)
	}
	return Verdict{}, e.WithUsage(billedUsage)
}

// buildJudgePrompt assembles the judge's user message: the rubric and the
// candidate output, clearly delimited so the model does not confuse
// instructions with content.
func buildJudgePrompt(rubric, output string) string {
	var b strings.Builder
	b.WriteString("RUBRIC:\n")
	b.WriteString(rubric)
	b.WriteString("\n\nCANDIDATE OUTPUT:\n")
	b.WriteString(output)
	return b.String()
}

// parseJudgeAnswer extracts {score, rationale} from a judge response: the
// provider's native structured payload when present, else the completion text
// run through the deterministic JSON-repair pass. Returns ok=false when the
// answer is not a parseable object with a numeric score in [0, 1].
func parseJudgeAnswer(resp llm.ChatResponse) (score float64, rationale string, ok bool) {
	raw := resp.Structured
	if len(raw) == 0 {
		rep := jsonrepair.Repair(resp.Text())
		if rep.Status == jsonrepair.StatusUnrepairable {
			return 0, "", false
		}
		raw = rep.Value
	}
	var ans struct {
		Score     *float64 `json:"score"`
		Rationale string   `json:"rationale"`
	}
	if err := json.Unmarshal(raw, &ans); err != nil {
		return 0, "", false
	}
	if ans.Score == nil {
		return 0, "", false
	}
	s := *ans.Score
	if s < 0 || s > 1 {
		return 0, "", false
	}
	return s, ans.Rationale, true
}

// judgeFailMessage renders a below-threshold fail message: structure-only
// (the numeric score and threshold) plus the model's rationale (which is the
// judge's own words about the output, safe to surface and the material the
// semantic-retry feedback carries).
func judgeFailMessage(score, threshold float64, rationale string) string {
	msg := fmt.Sprintf("score %.2f is below the threshold %.2f", score, threshold)
	if rationale != "" {
		msg += ": " + rationale
	}
	return msg
}

// truncateRunes truncates s to at most maxRunes runes, appending a marker
// when it cut — bounding what the judge reads and what the verdict stores.
func truncateRunes(s string, maxRunes int) string {
	if maxRunes <= 0 {
		return s
	}
	r := []rune(s)
	if len(r) <= maxRunes {
		return s
	}
	return string(r[:maxRunes]) + "…[truncated]"
}
