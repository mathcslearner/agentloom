package contextmgr_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/mathcslearner/agentloom/internal/blackboard"
	"github.com/mathcslearner/agentloom/internal/contextmgr"
	"github.com/mathcslearner/agentloom/internal/dag"
)

// threadBoard is an in-memory blackboard whose History returns every version of
// a key in ascending order — the read the `thread` context source performs.
type threadBoard struct {
	versions map[string][]blackboard.Entry
}

func (b *threadBoard) Get(_ context.Context, key string) (blackboard.Entry, bool, error) {
	vs := b.versions[key]
	if len(vs) == 0 {
		return blackboard.Entry{}, false, nil
	}
	return vs[len(vs)-1], true, nil
}

func (b *threadBoard) History(_ context.Context, key string) ([]blackboard.Entry, error) {
	return append([]blackboard.Entry(nil), b.versions[key]...), nil
}

func (b *threadBoard) List(context.Context, blackboard.ListFilter) ([]blackboard.Entry, error) {
	return nil, nil
}

func (b *threadBoard) Put(context.Context, blackboard.PutArgs) (blackboard.Entry, error) {
	return blackboard.Entry{}, nil
}

func threadMsg(t *testing.T, author, role string, iter int, content string) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(blackboard.ThreadMessage{
		Author: author, Role: role, Iteration: iter,
		Content: str(t, content), CreatedAt: time.Unix(0, 0).UTC(),
	})
	if err != nil {
		t.Fatalf("marshal thread message: %v", err)
	}
	return b
}

// TestAssembleThreadRendersHistory pins that a thread source reads the whole
// version History of its key and renders ordered <message> elements carrying
// author/role/iteration — the "conversation view" (ticket 14.2).
func TestAssembleThreadRendersHistory(t *testing.T) {
	t.Parallel()

	board := &threadBoard{versions: map[string][]blackboard.Entry{
		"thread": {
			{Key: "thread", Version: 1, Value: threadMsg(t, "research", "researcher", 0, "sea turtles migrate far")},
			{Key: "thread", Version: 2, Value: threadMsg(t, "write", "writer", 0, "a draft about turtles")},
		},
	}}
	spec := dag.ContextSpec{Sources: []dag.ContextSource{{Kind: dag.SourceThread, Name: "conversation"}}}

	asm, err := contextmgr.Assemble(context.Background(), spec, mockCounter(), contextmgr.Sources{Board: board})
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	if !strings.Contains(asm.Preamble, `kind="thread"`) {
		t.Errorf("preamble missing the thread block:\n%s", asm.Preamble)
	}
	for _, want := range []string{
		`author="research" role="researcher" iteration=0 version=1`,
		`sea turtles migrate far`,
		`author="write" role="writer" iteration=0 version=2`,
		`a draft about turtles`,
	} {
		if !strings.Contains(asm.Preamble, want) {
			t.Errorf("preamble missing %q:\n%s", want, asm.Preamble)
		}
	}
	// The researcher turn must render before the writer turn (History order).
	if strings.Index(asm.Preamble, "sea turtles") > strings.Index(asm.Preamble, "a draft") {
		t.Error("thread messages rendered out of order")
	}
}

// TestAssembleThreadRoleFilter pins that the optional Role filter keeps only the
// matching role's turns.
func TestAssembleThreadRoleFilter(t *testing.T) {
	t.Parallel()

	board := &threadBoard{versions: map[string][]blackboard.Entry{
		"thread": {
			{Key: "thread", Version: 1, Value: threadMsg(t, "research", "researcher", 0, "the findings")},
			{Key: "thread", Version: 2, Value: threadMsg(t, "write", "writer", 0, "the draft")},
		},
	}}
	spec := dag.ContextSpec{Sources: []dag.ContextSource{{Kind: dag.SourceThread, Role: "researcher"}}}

	asm, err := contextmgr.Assemble(context.Background(), spec, mockCounter(), contextmgr.Sources{Board: board})
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	if !strings.Contains(asm.Preamble, "the findings") {
		t.Errorf("role filter dropped the matching researcher turn:\n%s", asm.Preamble)
	}
	if strings.Contains(asm.Preamble, "the draft") {
		t.Errorf("role filter kept a non-matching writer turn:\n%s", asm.Preamble)
	}
}

// TestAssembleThreadMissing pins the on_missing policy for an empty thread: the
// default errors, skip omits.
func TestAssembleThreadMissing(t *testing.T) {
	t.Parallel()
	board := &threadBoard{versions: map[string][]blackboard.Entry{}}

	// Default (error): a thread that resolves to nothing fails the assembly.
	errSpec := dag.ContextSpec{Sources: []dag.ContextSource{{Kind: dag.SourceThread}}}
	if _, err := contextmgr.Assemble(context.Background(), errSpec, mockCounter(), contextmgr.Sources{Board: board}); err == nil {
		t.Fatal("Assemble: want a missing-source error for an empty thread under on_missing error")
	}

	// Skip: an empty thread is omitted and the assembly proceeds.
	skipSpec := dag.ContextSpec{Sources: []dag.ContextSource{{Kind: dag.SourceThread, OnMissing: dag.MissingSkip}}}
	asm, err := contextmgr.Assemble(context.Background(), skipSpec, mockCounter(), contextmgr.Sources{Board: board})
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	if asm.Preamble != "" {
		t.Errorf("preamble = %q, want empty for a skipped empty thread", asm.Preamble)
	}
	if len(asm.Sources) != 1 || asm.Sources[0].Status != contextmgr.Skipped {
		t.Errorf("source report = %+v, want one Skipped", asm.Sources)
	}
}

// TestThreadCompactionPinnedHandoffSurvives is the ticket 14.2 acceptance: a
// long thread compacts under truncate_oldest while a pinned handoff source
// survives byte-identical. The pinned handoff is the "explicit handoff payload"
// convention; the thread is the compactable conversation.
func TestThreadCompactionPinnedHandoffSurvives(t *testing.T) {
	t.Parallel()

	// A long thread: many turns so the assembled request is well over budget.
	turns := make([]blackboard.Entry, 0, 30)
	for i := 0; i < 30; i++ {
		turns = append(turns, blackboard.Entry{
			Key: "thread", Version: i + 1,
			Value: threadMsg(t, "writer", "writer", i, strings.Repeat("draft sentence number "+itoa(i)+" ", 8)),
		})
	}
	board := &threadBoard{versions: map[string][]blackboard.Entry{
		"thread":  turns,
		"handoff": {{Key: "handoff", Version: 1, Value: str(t, "PINNED-HANDOFF-PAYLOAD")}},
	}}
	counter := mockCounter()
	spec := dag.ContextSpec{
		Sources: []dag.ContextSource{
			{Kind: dag.SourceThread, Name: "conversation"},
			{Kind: dag.SourceBlackboard, Name: "handoff", Key: "handoff", Pinned: true},
		},
	}
	asm, err := contextmgr.Assemble(context.Background(), spec, counter, contextmgr.Sources{Board: board})
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	full := asm.ContextTokens
	budget := full / 2 // force compaction

	// measure the whole preamble (no request framing needed for this unit test).
	measure := func(preamble string) (int, error) { return counter.Count(preamble), nil }
	compacted, err := contextmgr.Compact(context.Background(), asm, contextmgr.Policy{
		Budget:   budget,
		Pipeline: []dag.CompactionStrategy{{Strategy: dag.TruncateOldest}},
	}, counter, measure)
	if err != nil {
		t.Fatalf("Compact: %v", err)
	}
	if compacted.FinalTokens >= full {
		t.Errorf("thread did not compact: tokens %d >= original %d", compacted.FinalTokens, full)
	}
	if !strings.Contains(compacted.Preamble, "PINNED-HANDOFF-PAYLOAD") {
		t.Errorf("pinned handoff did not survive compaction:\n%s", compacted.Preamble)
	}
	if !strings.Contains(compacted.Preamble, "elided") {
		t.Errorf("thread was not truncated (no elision marker):\n%s", compacted.Preamble)
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
