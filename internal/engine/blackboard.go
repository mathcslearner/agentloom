package engine

// Blackboard integration in the execution pipeline (ticket 12.2, ADR-014):
// the engine binds a step-scoped board onto each StepContext (claim.go) and
// applies a step's declarative `blackboard` writes in the completion
// transaction, atomically with the success CAS. Declarative writes are pure
// to plan (resolve each write's From pointer into the step's output) and are
// applied under the run lock the success CAS already holds — so a fenced
// zombie completion never writes, and the entry is durable exactly with the
// step's success.

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/mathcslearner/agentloom/internal/blackboard"
	"github.com/mathcslearner/agentloom/internal/dag"
	"github.com/mathcslearner/agentloom/internal/exec"
	"github.com/mathcslearner/agentloom/internal/store"
	"github.com/mathcslearner/agentloom/internal/store/gen"
	"github.com/mathcslearner/agentloom/internal/tokens"
)

// defaultThreadContentPointer is the JSON pointer an auto-thread append selects
// from an agent's output for the message body — an llm-family step always sets
// a canonical /text, so this resolves for every agent. An output that lacks it
// falls back to the whole output (auto-threading never fails a succeeded step).
const defaultThreadContentPointer = "/text"

// stepCounter resolves the token counter for a step's blackboard writes: the
// executor's TokenCounter hook (the llm executor resolves the model's
// tokenizer) when it implements TokenCounterProvider and succeeds, else the
// chars/4 fallback — the honest choice for a non-model step or an
// unresolvable model.
func (e *Engine) stepCounter(executor exec.Executor, sc exec.StepContext) tokens.Counter {
	if tp, ok := executor.(exec.TokenCounterProvider); ok {
		if c, err := tp.TokenCounter(sc); err == nil && c != nil {
			return c
		}
	}
	return tokens.Fallback()
}

// plannedBBWrite is one declarative blackboard write with its From pointer
// already resolved against the step's output — the pure product of
// planBlackboardWrites, ready to insert inside the completion transaction.
type plannedBBWrite struct {
	key   string
	value json.RawMessage
	tags  []string
}

// planBlackboardWrites resolves a step's materialized `blackboard` block
// against its output (pure, no database reads). Each write's From JSON
// pointer selects the value to store; a pointer that does not resolve is a
// deterministic data error (the same output resolves the same way on every
// retry), so the caller records a permanent step failure. A step with no
// blackboard block plans nothing.
func planBlackboardWrites(step gen.RunStep, output json.RawMessage) ([]plannedBBWrite, error) {
	if len(step.BlackboardPolicy) == 0 {
		return nil, nil
	}
	var bp dag.BlackboardPolicy
	if err := json.Unmarshal(step.BlackboardPolicy, &bp); err != nil {
		// Corrupt materialized policy — deterministic, so permanent.
		return nil, fmt.Errorf("decoding blackboard policy: %w", err)
	}
	writes := make([]plannedBBWrite, 0, len(bp.Write))
	for _, w := range bp.Write {
		val, err := blackboard.ResolvePointer(output, w.From)
		if err != nil {
			return nil, fmt.Errorf("blackboard write to key %q: %w", w.Key, err)
		}
		tags := append([]string(nil), w.Tags...)
		if w.Pinned {
			tags = append(tags, blackboard.TagPinned)
		}
		writes = append(writes, plannedBBWrite{key: w.Key, value: val, tags: tags})
	}
	return writes, nil
}

// applyBlackboardWrites inserts a step's planned declarative writes on the
// completion transaction's Querier, token-counted with the step's counter.
// Each is unconditional (no CAS) and attributed to the completing step; the
// success CAS already fenced this completion, so the writes need no fence of
// their own. Called only inside the completion transaction, under the run
// lock the success CAS holds.
func (e *Engine) applyBlackboardWrites(ctx context.Context, q store.Querier, step gen.RunStep, writes []plannedBBWrite, counter tokens.Counter, now time.Time) error {
	for _, w := range writes {
		value := blackboard.CanonicalValue(w.value)
		tags, err := blackboard.NormalizeTags(w.tags)
		if err != nil {
			// The tag grammar was validated at submit; a violation here is
			// corrupt materialized state — permanent, surfaced by rolling back.
			return fmt.Errorf("blackboard write to key %q: %w", w.key, err)
		}
		count := counter.Count(string(value))
		if _, err := store.PutBlackboardEntry(ctx, q, store.BlackboardPutArgs{
			RunID:         step.RunID,
			Key:           w.key,
			Value:         value,
			TokenCount:    clampCount(count),
			TokenCounter:  counter.ID(),
			Tags:          tags,
			AuthorStepID:  step.StepID,
			AuthorAttempt: step.AttemptCount,
			Now:           now,
		}); err != nil {
			return err
		}
	}
	return nil
}

func clampCount(n int) int32 {
	switch {
	case n < 0:
		return 0
	case n > math.MaxInt32:
		return math.MaxInt32
	default:
		return int32(n)
	}
}

// plannedThreadMessage is one auto-thread append with its ThreadMessage already
// marshaled — the pure product of planThreadAppend, ready to insert inside the
// completion transaction.
type plannedThreadMessage struct {
	value json.RawMessage
}

// planThreadAppend builds the handoff-thread message for a completing agent
// step (ticket 14.2, ADR-016): pure, no database reads. An agent turn is
// auto-appended to the run's thread — a new version of the conventional thread
// key carrying the message body plus author/role/iteration — so a downstream
// agent's `thread` context source sees the prior turns. Non-agent steps plan
// nothing (nil). The role comes from the merged AgentConfig (ResolveAgentStep
// stamped it); the iteration comes from the step's `#k` instance suffix (0 for
// an authored step, the loop iteration once 14.3 unrolls loops); the content is
// the step's /text output, falling back to the whole output. A decode/marshal
// failure is deterministic → the caller records a permanent step failure.
func planThreadAppend(step gen.RunStep, output json.RawMessage, now time.Time) (*plannedThreadMessage, error) {
	if dag.StepType(step.StepType) != dag.StepAgent {
		return nil, nil
	}
	var cfg struct {
		Role string `json:"role"`
	}
	if len(step.Config) > 0 {
		if err := json.Unmarshal(step.Config, &cfg); err != nil {
			return nil, fmt.Errorf("decoding agent config for thread append: %w", err)
		}
	}
	content, perr := blackboard.ResolvePointer(output, defaultThreadContentPointer)
	if perr != nil {
		// An agent output without /text (unusual for an llm-family step) still
		// contributes its whole output as the message body — the empty pointer
		// always resolves (a nil output becomes JSON null).
		content, _ = blackboard.ResolvePointer(output, "")
	}
	value, err := json.Marshal(blackboard.ThreadMessage{
		Author:    step.StepID,
		Role:      cfg.Role,
		Iteration: threadIteration(step.StepID),
		Content:   content,
		CreatedAt: now,
	})
	if err != nil {
		return nil, fmt.Errorf("marshaling thread message: %w", err)
	}
	return &plannedThreadMessage{value: value}, nil
}

// applyThreadAppend inserts the planned thread message on the completion
// transaction's Querier — a new version of the conventional thread key, tagged
// TagThread, token-counted, attributed to the completing step. Unconditional
// (no CAS) and unfenced (the success CAS already fenced this completion), atomic
// with the success under the run lock it holds.
func (e *Engine) applyThreadAppend(ctx context.Context, q store.Querier, step gen.RunStep, msg *plannedThreadMessage, counter tokens.Counter, now time.Time) error {
	value := blackboard.CanonicalValue(msg.value)
	tags, err := blackboard.NormalizeTags([]string{blackboard.TagThread})
	if err != nil {
		return fmt.Errorf("thread append to key %q: %w", blackboard.DefaultThreadKey, err)
	}
	count := counter.Count(string(value))
	if _, err := store.PutBlackboardEntry(ctx, q, store.BlackboardPutArgs{
		RunID:         step.RunID,
		Key:           blackboard.DefaultThreadKey,
		Value:         value,
		TokenCount:    clampCount(count),
		TokenCounter:  counter.ID(),
		Tags:          tags,
		AuthorStepID:  step.StepID,
		AuthorAttempt: step.AttemptCount,
		Now:           now,
	}); err != nil {
		return err
	}
	return nil
}

// threadIteration parses the loop iteration from a step's `#k` instance suffix
// (the reserved instance-id space): "writer#2" → 2, an authored id with no
// suffix → 0. 14.2's authored agents are all iteration 0; 14.3's loop unrolling
// mints the `#k` instances that carry a real iteration.
func threadIteration(stepID string) int {
	i := strings.LastIndex(stepID, "#")
	if i < 0 || i == len(stepID)-1 {
		return 0
	}
	n, err := strconv.Atoi(stepID[i+1:])
	if err != nil || n < 0 {
		return 0
	}
	return n
}
