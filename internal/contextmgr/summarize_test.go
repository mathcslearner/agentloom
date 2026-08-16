package contextmgr_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/mathcslearner/agentloom/internal/blackboard"
	"github.com/mathcslearner/agentloom/internal/contextmgr"
	"github.com/mathcslearner/agentloom/internal/dag"
)

// writeBoard is an in-memory append-per-key blackboard for summarize tests: a
// Put appends the next version and Get returns the head, mirroring the store's
// versioning so parent_version chaining can be asserted.
type writeBoard struct {
	versions map[string][]blackboard.Entry
	putErr   error // when set, every Put fails with it
}

func newWriteBoard() *writeBoard { return &writeBoard{versions: map[string][]blackboard.Entry{}} }

func (b *writeBoard) Get(_ context.Context, key string) (blackboard.Entry, bool, error) {
	vs := b.versions[key]
	if len(vs) == 0 {
		return blackboard.Entry{}, false, nil
	}
	return vs[len(vs)-1], true, nil
}

func (b *writeBoard) History(_ context.Context, key string) ([]blackboard.Entry, error) {
	return append([]blackboard.Entry(nil), b.versions[key]...), nil
}

func (b *writeBoard) List(context.Context, blackboard.ListFilter) ([]blackboard.Entry, error) {
	return nil, nil
}

func (b *writeBoard) Put(_ context.Context, args blackboard.PutArgs) (blackboard.Entry, error) {
	if b.putErr != nil {
		return blackboard.Entry{}, b.putErr
	}
	next := len(b.versions[args.Key]) + 1
	e := blackboard.Entry{Key: args.Key, Version: next, Value: args.Value, Tags: args.Tags}
	b.versions[args.Key] = append(b.versions[args.Key], e)
	return e, nil
}

// fakeSummarizer returns a fixed short summary text (or an error) and records
// its calls so cache/fallback behavior can be asserted.
type fakeSummarizer struct {
	text     string
	resource string
	err      error
	calls    int
	spans    []string // the span text of each call, in order
}

func (s *fakeSummarizer) Summarize(_ context.Context, req contextmgr.SummarizeRequest) (contextmgr.SummaryResult, error) {
	s.calls++
	s.spans = append(s.spans, req.SpanText)
	if s.err != nil {
		return contextmgr.SummaryResult{}, s.err
	}
	res := s.resource
	if res == "" {
		res = "mock:cheap"
	}
	return contextmgr.SummaryResult{
		Text: s.text, Resource: res,
		Usage: &contextmgr.SummaryUsage{InputTokens: 100, OutputTokens: 10},
	}, nil
}

// TestCompactSummarizeShrinks folds the oldest non-pinned turns into a summary,
// writes it to the blackboard, keeps the pinned source untouched, and brings the
// request under budget.
func TestCompactSummarizeShrinks(t *testing.T) {
	t.Parallel()
	c := mockCounter()
	entries := []contextmgr.Entry{
		ent(c, 0, "pin", strings.Repeat("P", 40), true, 0),
		ent(c, 1, "t1", strings.Repeat("a", 400), false, 0),
		ent(c, 2, "t2", strings.Repeat("b", 400), false, 0),
		ent(c, 3, "t3", strings.Repeat("c", 40), false, 0),
	}
	asm := asmOf(c, entries...)
	m := measurer(c)
	full, _ := m(asm.Preamble)
	budget := full - 150 // force summarization of the oldest turns

	board := newWriteBoard()
	summ := &fakeSummarizer{text: "digest of the early turns"}
	cmp, err := contextmgr.Compact(context.Background(), asm, contextmgr.Policy{
		Budget: budget, Summarizer: summ, Board: board,
		Pipeline: []dag.CompactionStrategy{{Strategy: dag.SummarizeStrategy, Model: "mock/cheap"}},
	}, c, m)
	if err != nil {
		t.Fatalf("Compact: %v", err)
	}
	if cmp.FinalTokens > budget {
		t.Errorf("FinalTokens %d over budget %d", cmp.FinalTokens, budget)
	}
	if summ.calls != 1 {
		t.Fatalf("summarizer calls = %d, want 1", summ.calls)
	}
	// The pinned source survives byte-identical.
	if !strings.Contains(cmp.Preamble, strings.Repeat("P", 40)) {
		t.Error("pinned source altered by summarize")
	}
	// The summary text is present; the summarized turns are gone.
	if !strings.Contains(cmp.Preamble, "digest of the early turns") {
		t.Error("summary text missing from preamble")
	}
	// The summary landed on the blackboard at version 1 with no parent.
	if got := board.versions["context_summary"]; len(got) != 1 {
		t.Fatalf("blackboard versions = %d, want 1", len(got))
	}
	var sv contextmgr.SummaryValue
	if err := json.Unmarshal(board.versions["context_summary"][0].Value, &sv); err != nil {
		t.Fatalf("unmarshal summary value: %v", err)
	}
	if sv.Text != "digest of the early turns" || sv.ParentVersion != 0 || sv.Resource != "mock:cheap" {
		t.Errorf("summary value = %+v", sv)
	}
	// The revision and Compacted.Summaries carry the overhead usage.
	if len(cmp.Summaries) != 1 || cmp.Summaries[0].InputTokens != 100 || cmp.Summaries[0].Version != 1 {
		t.Errorf("summaries = %+v", cmp.Summaries)
	}
	if len(cmp.Revisions) != 1 || cmp.Revisions[0].Strategy != "summarize" || !cmp.Revisions[0].Changed {
		t.Errorf("revisions = %+v", cmp.Revisions)
	}
}

// TestCompactSummarizeChains folds an existing summary with newer turns into a
// new summary version, so the key's history is the chain.
func TestCompactSummarizeChains(t *testing.T) {
	t.Parallel()
	c := mockCounter()
	board := newWriteBoard()
	// Seed a prior summary (version 1) on the key.
	prior, _ := json.Marshal(contextmgr.SummaryValue{SchemaVersion: 1, Text: "prior"})
	if _, err := board.Put(context.Background(), blackboard.PutArgs{Key: "context_summary", Value: prior}); err != nil {
		t.Fatal(err)
	}
	entries := []contextmgr.Entry{
		ent(c, 0, "t1", strings.Repeat("a", 400), false, 0),
		ent(c, 1, "t2", strings.Repeat("b", 400), false, 0),
	}
	asm := asmOf(c, entries...)
	m := measurer(c)
	full, _ := m(asm.Preamble)
	budget := full - 100
	summ := &fakeSummarizer{text: "chained digest"}
	cmp, err := contextmgr.Compact(context.Background(), asm, contextmgr.Policy{
		Budget: budget, Summarizer: summ, Board: board,
		Pipeline: []dag.CompactionStrategy{{Strategy: dag.SummarizeStrategy, Model: "mock/cheap"}},
	}, c, m)
	if err != nil {
		t.Fatalf("Compact: %v", err)
	}
	if got := board.versions["context_summary"]; len(got) != 2 {
		t.Fatalf("blackboard versions = %d, want 2 (chained)", len(got))
	}
	if cmp.Summaries[0].ParentVersion != 1 {
		t.Errorf("chained summary parent_version = %d, want 1", cmp.Summaries[0].ParentVersion)
	}
}

// TestCompactSummarizeFallsBack: a summarizer failure leaves the assembly
// unchanged, records the failure on the revision, and lets the next strategy
// run — the step is never blocked.
func TestCompactSummarizeFallsBack(t *testing.T) {
	t.Parallel()
	c := mockCounter()
	entries := []contextmgr.Entry{
		ent(c, 0, "t1", strings.Repeat("a", 400), false, 0),
		ent(c, 1, "t2", strings.Repeat("b", 400), false, 0),
		ent(c, 2, "t3", strings.Repeat("c", 400), false, 0),
	}
	asm := asmOf(c, entries...)
	m := measurer(c)
	full, _ := m(asm.Preamble)
	budget := full - 200
	board := newWriteBoard()
	summ := &fakeSummarizer{err: errors.New("provider down")}
	cmp, err := contextmgr.Compact(context.Background(), asm, contextmgr.Policy{
		Budget: budget, Summarizer: summ, Board: board,
		Pipeline: []dag.CompactionStrategy{
			{Strategy: dag.SummarizeStrategy, Model: "mock/cheap"},
			{Strategy: dag.SlidingWindow, N: intp(1)},
		},
	}, c, m)
	if err != nil {
		t.Fatalf("Compact: %v", err)
	}
	if len(board.versions) != 0 {
		t.Error("summary written despite summarizer failure")
	}
	if len(cmp.Revisions) != 2 {
		t.Fatalf("revisions = %d, want 2 (summarize fallback + sliding_window)", len(cmp.Revisions))
	}
	if cmp.Revisions[0].Strategy != "summarize" || cmp.Revisions[0].Changed || cmp.Revisions[0].Error == "" {
		t.Errorf("summarize revision should be an unchanged fallback with an error: %+v", cmp.Revisions[0])
	}
	if cmp.Revisions[1].Strategy != "sliding_window" || !cmp.Revisions[1].Changed {
		t.Errorf("fallback strategy did not run: %+v", cmp.Revisions[1])
	}
	if cmp.FinalTokens > budget {
		t.Errorf("FinalTokens %d over budget %d after fallback", cmp.FinalTokens, budget)
	}
}

// TestCompactSummarizeContextCancel: a caller-context cancellation surfaced by
// the summarizer is fatal (not a fallback), so the engine keeps its
// timeout/cancelled judgment.
func TestCompactSummarizeContextCancel(t *testing.T) {
	t.Parallel()
	c := mockCounter()
	entries := []contextmgr.Entry{
		ent(c, 0, "t1", strings.Repeat("a", 400), false, 0),
		ent(c, 1, "t2", strings.Repeat("b", 400), false, 0),
	}
	asm := asmOf(c, entries...)
	m := measurer(c)
	full, _ := m(asm.Preamble)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	summ := &fakeSummarizer{err: context.Canceled}
	_, err := contextmgr.Compact(ctx, asm, contextmgr.Policy{
		Budget: full - 100, Summarizer: summ, Board: newWriteBoard(),
		Pipeline: []dag.CompactionStrategy{{Strategy: dag.SummarizeStrategy, Model: "mock/cheap"}},
	}, c, m)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("want context.Canceled, got %v", err)
	}
}

// TestSummaryChatRequestDeterministic pins the summarizer request shape: fixed
// system prompt, span-then-instruction user turns, temperature 0.
func TestSummaryChatRequestDeterministic(t *testing.T) {
	t.Parallel()
	req := contextmgr.SummaryChatRequest("cheap", "the span material", 128)
	if req.Model != "cheap" || req.MaxTokens != 128 {
		t.Errorf("req = %+v", req)
	}
	if req.Temperature == nil || *req.Temperature != 0 {
		t.Errorf("temperature = %v, want 0", req.Temperature)
	}
	if len(req.Messages) != 2 || req.Messages[0].Blocks[0].Text != "the span material" {
		t.Errorf("messages = %+v", req.Messages)
	}
	// Byte-stable across calls.
	if a, b := contextmgr.SummaryChatRequest("cheap", "x", 1), contextmgr.SummaryChatRequest("cheap", "x", 1); a.System != b.System {
		t.Error("system prompt not stable")
	}
}
