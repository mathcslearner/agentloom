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
	"time"

	"github.com/mathcslearner/agentloom/internal/blackboard"
	"github.com/mathcslearner/agentloom/internal/dag"
	"github.com/mathcslearner/agentloom/internal/exec"
	"github.com/mathcslearner/agentloom/internal/store"
	"github.com/mathcslearner/agentloom/internal/store/gen"
	"github.com/mathcslearner/agentloom/internal/tokens"
)

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
