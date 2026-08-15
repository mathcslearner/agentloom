package validate

// The validation chain (ticket 11.1, ADR-013): resolving a step's authored
// dag.ValidationPolicy against the registry into runnable entries
// (pre-flight — unknown validator / bad config are permanent errors before
// any spend), and running the chain over a step output into one aggregated
// verdict.

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"time"

	"github.com/mathcslearner/agentloom/internal/dag"
)

// resolvedEntry is one chain entry ready to run: the validator, its
// pre-validated config, the effective target pointer, and whether it is
// cost-bearing (which gates it behind the cheap validators — ADR-013).
type resolvedEntry struct {
	name        string
	validator   Validator
	config      json.RawMessage
	target      string
	costBearing bool
}

// Chain is a step's resolved validation chain (ADR-013): the ordered
// validators, each bound to its config and target. Built by Resolve at
// claim, run by Run after the step's executor succeeds. Immutable after
// Resolve; safe for concurrent Run calls.
type Chain struct {
	stepType dag.StepType
	entries  []resolvedEntry
}

// Empty reports whether the chain has no validators (a nil chain or a
// resolved empty one — the engine treats both as "no validation").
func (c *Chain) Empty() bool { return c == nil || len(c.entries) == 0 }

// Len reports the number of validators in the chain.
func (c *Chain) Len() int {
	if c == nil {
		return 0
	}
	return len(c.entries)
}

// Resolve turns a step's authored validation policy into a runnable Chain
// (ADR-013's pre-flight gate). For each entry it looks the validator up
// (unknown → *UnknownValidatorError, permanent), validates the entry's
// config against the validator's compiled schema (violation →
// *ConfigValidationError, permanent), computes the effective target (the
// entry's target, or the step-type default — /text for llm-family), and
// reads the cost-bearing flag from the manifest. A nil policy or one with
// no validators resolves to nil (no chain). Returns the first error
// encountered, so a bad chain fails before any money is spent.
func Resolve(reg *Registry, policy *dag.ValidationPolicy, stepType dag.StepType) (*Chain, error) {
	if policy == nil || len(policy.Validators) == 0 {
		return nil, nil
	}
	if reg == nil {
		// No validator registry wired but the step authored a chain: the
		// named validators cannot exist, so this is the unknown-validator
		// case — permanent, before spend.
		return nil, &UnknownValidatorError{Name: policy.Validators[0].Name}
	}
	entries := make([]resolvedEntry, 0, len(policy.Validators))
	for _, spec := range policy.Validators {
		v, err := reg.Get(spec.Name)
		if err != nil {
			return nil, err
		}
		if err := reg.ValidateConfig(spec.Name, spec.Config); err != nil {
			return nil, err
		}
		entries = append(entries, resolvedEntry{
			name:        spec.Name,
			validator:   v,
			config:      spec.Config,
			target:      effectiveTarget(spec.Target, stepType),
			costBearing: v.Manifest().Capabilities.CostBearing,
		})
	}
	return &Chain{stepType: stepType, entries: entries}, nil
}

// Run executes the chain over a step's output and aggregates the
// per-validator verdicts into one (ADR-013's chain semantics):
//
//   - Cheap (non-cost-bearing) validators run first, in authored order, and
//     ALL of them run — so 11.4 gets the complete critique, not just the
//     first failure.
//   - Cost-bearing validators (the llm_judge, 11.5) run only if every cheap
//     validator passed — no point paying a judge to grade an output a free
//     check already rejected. If a cheap validator failed, the cost-bearing
//     entries are recorded skipped.
//   - The chain verdict is fail if any validator failed; its Score is the
//     minimum of the reported per-validator scores; its Issues are every
//     failing validator's issues, in chain order.
//
// A validator that returns a non-nil error is a transport failure of the
// validation stage itself (an llm_judge provider error, a ctx cancellation)
// — Run returns that error immediately (aborting the chain), and the engine
// routes it through the ADR-006 taxonomy rather than as a fail verdict.
func (c *Chain) Run(ctx context.Context, output json.RawMessage, attempt int, logger *slog.Logger) (Verdict, error) {
	results := make([]ValidatorResult, len(c.entries))
	var issues []Issue
	chainFailed := false
	var minScore *float64
	cheapFailed := false

	// Two passes over the authored order: cheap validators first (all run),
	// then cost-bearing ones only if no cheap validator failed.
	for pass := 0; pass < 2; pass++ {
		wantCostBearing := pass == 1
		for i, e := range c.entries {
			if e.costBearing != wantCostBearing {
				continue
			}
			if wantCostBearing && cheapFailed {
				results[i] = ValidatorResult{Name: e.name, Status: StatusSkipped}
				continue
			}
			value, targetErr := resolveTarget(output, e.target)
			start := time.Now()
			var v Verdict
			var err error
			if targetErr != nil {
				// A target that does not resolve is a content failure the
				// validator never sees — a fail verdict with a structured
				// issue, not a transport error (ADR-013).
				v = FailVerdict(Issue{
					Validator: e.name, Code: "target_not_found", Path: e.target,
					Message: "validation target pointer did not resolve in the output",
				})
			} else {
				v, err = e.validator.Validate(ctx, Input{
					StepType: c.stepType, Output: output, Value: value,
					Config: e.config, Attempt: attempt, Logger: logger,
				})
			}
			dur := time.Since(start).Milliseconds()
			if err != nil {
				// Transport failure of the validation stage — abort the chain
				// and let the engine classify (ADR-013's decision table).
				return Verdict{}, err
			}
			// Stamp the validator name on any issues that lack one, so a
			// chain verdict's issues stay attributable regardless of the
			// validator's diligence.
			for j := range v.Issues {
				if v.Issues[j].Validator == "" {
					v.Issues[j].Validator = e.name
				}
			}
			res := ValidatorResult{
				Name: e.name, Status: v.Status, Score: v.Score,
				IssueCount: len(v.Issues), DurationMS: dur,
			}
			// Lift a cost-bearing validator's per-result detail (rationale,
			// usage, and — for on_error:skip — an error status) from its
			// single-validator verdict into the chain's per-validator result
			// (11.5). A validator returns these on its own Results[0]; the
			// chain is the aggregator that surfaces them per validator.
			if len(v.Results) == 1 {
				r0 := v.Results[0]
				res.Rationale = r0.Rationale
				res.Usage = r0.Usage
				res.Error = r0.Error
				// on_error:skip returns a PASS verdict whose sole result is an
				// `error` status: the chain does not fail, but the per-validator
				// result records that the validator could not judge.
				if r0.Status == StatusError {
					res.Status = StatusError
				}
			}
			results[i] = res
			minScore = minScorePtr(minScore, v.Score)
			if !v.Passed() {
				chainFailed = true
				if !e.costBearing {
					cheapFailed = true
				}
				issues = append(issues, v.Issues...)
			}
		}
	}

	status := StatusPass
	if chainFailed {
		status = StatusFail
	}
	return Verdict{
		SchemaVersion: VerdictSchemaVersion,
		Status:        status,
		Score:         minScore,
		Issues:        issues,
		Results:       results,
	}, nil
}

// minScorePtr returns the lower of two optional scores (nil = unreported,
// ignored). The chain score is the minimum of the reported per-validator
// scores.
func minScorePtr(cur, next *float64) *float64 {
	if next == nil {
		return cur
	}
	if cur == nil || *next < *cur {
		v := *next
		return &v
	}
	return cur
}

// effectiveTarget resolves an entry's target: the authored pointer, or the
// step-type default. llm-family steps produce {model, stop_reason, text,
// ...}, so an absent target defaults to /text — the model's actual answer —
// rather than the whole envelope (ADR-013).
func effectiveTarget(target string, stepType dag.StepType) string {
	if target != "" {
		return target
	}
	switch stepType {
	case dag.StepLLM, dag.StepPlanner, dag.StepAgent:
		return "/text"
	default:
		return ""
	}
}

// resolveTarget selects the sub-tree of output named by an RFC 6901 JSON
// pointer. An empty pointer selects the whole output. A pointer that does
// not resolve returns errTargetNotFound; the caller turns that into a fail
// verdict, never a panic.
func resolveTarget(output json.RawMessage, pointer string) (json.RawMessage, error) {
	if pointer == "" {
		return output, nil
	}
	tokens, err := parseJSONPointer(pointer)
	if err != nil {
		return nil, err
	}
	var cur any
	if uerr := json.Unmarshal(output, &cur); uerr != nil {
		return nil, errTargetNotFound
	}
	for _, tok := range tokens {
		switch node := cur.(type) {
		case map[string]any:
			next, ok := node[tok]
			if !ok {
				return nil, errTargetNotFound
			}
			cur = next
		case []any:
			idx, ierr := arrayIndex(tok, len(node))
			if ierr != nil {
				return nil, errTargetNotFound
			}
			cur = node[idx]
		default:
			return nil, errTargetNotFound
		}
	}
	sel, merr := json.Marshal(cur)
	if merr != nil {
		return nil, errTargetNotFound
	}
	return sel, nil
}

// errTargetNotFound signals a JSON pointer that did not resolve in the
// output — a content problem, mapped to a fail verdict by the chain.
var errTargetNotFound = errors.New("validation target not found")

// parseJSONPointer splits an RFC 6901 pointer into its decoded reference
// tokens ("~1" → "/", "~0" → "~"). A non-empty pointer must start with "/".
func parseJSONPointer(pointer string) ([]string, error) {
	if pointer == "" {
		return nil, nil
	}
	if pointer[0] != '/' {
		return nil, errors.New("JSON pointer must start with '/'")
	}
	parts := splitOnSlash(pointer[1:])
	tokens := make([]string, len(parts))
	for i, p := range parts {
		tokens[i] = unescapePointerToken(p)
	}
	return tokens, nil
}

// splitOnSlash splits a string on '/' without collapsing empty segments
// (an empty reference token — the "" key — is legal in RFC 6901).
func splitOnSlash(s string) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '/' {
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	return append(out, s[start:])
}

// unescapePointerToken decodes RFC 6901 escapes: "~1" → "/", "~0" → "~"
// (order matters — ~1 before ~0).
func unescapePointerToken(tok string) string {
	// Fast path: no escapes.
	if !containsByte(tok, '~') {
		return tok
	}
	out := make([]byte, 0, len(tok))
	for i := 0; i < len(tok); i++ {
		if tok[i] == '~' && i+1 < len(tok) {
			switch tok[i+1] {
			case '1':
				out = append(out, '/')
				i++
				continue
			case '0':
				out = append(out, '~')
				i++
				continue
			}
		}
		out = append(out, tok[i])
	}
	return string(out)
}

func containsByte(s string, b byte) bool {
	for i := 0; i < len(s); i++ {
		if s[i] == b {
			return true
		}
	}
	return false
}

// arrayIndex parses an RFC 6901 array index token: digits only, no leading
// zero (except "0"), in range. "-" (the "end of array" token) never
// resolves for a read.
func arrayIndex(tok string, length int) (int, error) {
	if tok == "" || (len(tok) > 1 && tok[0] == '0') {
		return 0, errors.New("invalid array index")
	}
	n := 0
	for i := 0; i < len(tok); i++ {
		if tok[i] < '0' || tok[i] > '9' {
			return 0, errors.New("invalid array index")
		}
		n = n*10 + int(tok[i]-'0')
	}
	if n >= length {
		return 0, errors.New("array index out of range")
	}
	return n, nil
}
