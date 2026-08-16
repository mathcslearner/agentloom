package exec

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/mathcslearner/agentloom/internal/blackboard"
	"github.com/mathcslearner/agentloom/internal/dag"
)

// fakeBoard is an in-memory blackboard.Board for the executor unit test.
type fakeBoard struct {
	entries map[string][]blackboard.Entry
	failCAS bool // when true, the next Put returns a version conflict
}

func newFakeBoard() *fakeBoard { return &fakeBoard{entries: map[string][]blackboard.Entry{}} }

func (b *fakeBoard) Get(_ context.Context, key string) (blackboard.Entry, bool, error) {
	vs := b.entries[key]
	if len(vs) == 0 {
		return blackboard.Entry{}, false, nil
	}
	return vs[len(vs)-1], true, nil
}

func (b *fakeBoard) History(_ context.Context, key string) ([]blackboard.Entry, error) {
	return b.entries[key], nil
}

func (b *fakeBoard) List(context.Context, blackboard.ListFilter) ([]blackboard.Entry, error) {
	return nil, nil
}

func (b *fakeBoard) Put(_ context.Context, args blackboard.PutArgs) (blackboard.Entry, error) {
	if b.failCAS {
		return blackboard.Entry{}, &blackboard.VersionConflictError{Key: args.Key, Expected: 0, Current: 1}
	}
	v := len(b.entries[args.Key]) + 1
	e := blackboard.Entry{Key: args.Key, Version: v, Value: args.Value, Tags: args.Tags}
	b.entries[args.Key] = append(b.entries[args.Key], e)
	return e, nil
}

func bbStep(t *testing.T, cfg map[string]any, board blackboard.Board) StepContext {
	t.Helper()
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal cfg: %v", err)
	}
	return StepContext{StepType: dag.StepBlackboardWrite, Config: raw, Attempt: 1, Blackboard: board}
}

func TestBlackboardWriteExecutorWritesAndReads(t *testing.T) {
	board := newFakeBoard()
	// Seed a key to read back.
	_, _ = board.Put(context.Background(), blackboard.PutArgs{Key: "draft", Value: json.RawMessage(`"hi"`)})

	out, err := BlackboardWriteExecutor{}.Execute(context.Background(),
		bbStep(t, map[string]any{"key": "note", "value": map[string]any{"ok": true}, "read_key": "draft"}, board))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	var got blackboardWriteOutput
	if err := json.Unmarshal(out.Data, &got); err != nil {
		t.Fatalf("output: %v", err)
	}
	if len(got.Wrote) == 0 || len(got.Read) == 0 {
		t.Fatalf("output missing wrote/read: %s", out.Data)
	}
}

func TestBlackboardWriteExecutorCASConflictIsTransient(t *testing.T) {
	board := newFakeBoard()
	board.failCAS = true
	zero := 0
	_, err := BlackboardWriteExecutor{}.Execute(context.Background(),
		bbStep(t, map[string]any{"key": "k", "value": 1, "expected_version": zero}, board))
	var ce *ClassifiedError
	if !errors.As(err, &ce) || ce.Class != dag.ClassTransient {
		t.Fatalf("CAS conflict = %v, want transient ClassifiedError", err)
	}
}

func TestBlackboardWriteExecutorNoBoardIsPermanent(t *testing.T) {
	sc := StepContext{StepType: dag.StepBlackboardWrite, Config: json.RawMessage(`{"key":"k","value":1}`), Attempt: 1}
	_, err := BlackboardWriteExecutor{}.Execute(context.Background(), sc)
	var ce *ClassifiedError
	if !errors.As(err, &ce) || ce.Class != dag.ClassPermanent {
		t.Fatalf("no board = %v, want permanent ClassifiedError", err)
	}
}

// TestLLMTokenCounterResolvesModel: the llm executor's TokenCounter hook
// returns a counter for the resolved model, and errors on an unresolvable
// model so the engine falls back.
func TestLLMTokenCounterResolvesModel(t *testing.T) {
	e := recExecutor(t, &recordingProvider{resp: okResponse()})
	counter, err := e.TokenCounter(llmStep(t, map[string]any{"model": "rec/sim-1", "prompt": "hi"}))
	if err != nil {
		t.Fatalf("TokenCounter: %v", err)
	}
	if counter == nil || counter.ID() == "" {
		t.Fatalf("counter = %v", counter)
	}
	// An unresolvable model errors (engine then uses the fallback counter).
	if _, err := e.TokenCounter(llmStep(t, map[string]any{"model": "nope/x", "prompt": "hi"})); err == nil {
		t.Fatal("TokenCounter(unresolvable) = nil error, want error")
	}
}
