package contextmgr_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mathcslearner/agentloom/internal/blackboard"
	"github.com/mathcslearner/agentloom/internal/contextmgr"
	"github.com/mathcslearner/agentloom/internal/dag"
	"github.com/mathcslearner/agentloom/internal/plugin"
	"github.com/mathcslearner/agentloom/internal/retrieval"
	"github.com/mathcslearner/agentloom/internal/tokens"
)

// --- fakes ---------------------------------------------------------------

// fakeBoard is an in-memory blackboard head store for read-side tests.
type fakeBoard struct {
	heads map[string]blackboard.Entry // key -> head
}

func (b *fakeBoard) Get(_ context.Context, key string) (blackboard.Entry, bool, error) {
	e, ok := b.heads[key]
	return e, ok, nil
}

func (b *fakeBoard) History(context.Context, string) ([]blackboard.Entry, error) {
	return nil, nil
}

func (b *fakeBoard) List(_ context.Context, f blackboard.ListFilter) ([]blackboard.Entry, error) {
	// Deterministic: collect heads carrying every listed tag, ordered by key.
	var keys []string
	for k := range b.heads {
		keys = append(keys, k)
	}
	// insertion-order-independent: sort.
	sortStrings(keys)
	var out []blackboard.Entry
	for _, k := range keys {
		e := b.heads[k]
		if hasAllTags(e.Tags, f.Tags) {
			out = append(out, e)
		}
	}
	return out, nil
}

func (b *fakeBoard) Put(context.Context, blackboard.PutArgs) (blackboard.Entry, error) {
	return blackboard.Entry{}, errors.New("read-only fake")
}

func hasAllTags(have, want []string) bool {
	for _, w := range want {
		found := false
		for _, h := range have {
			if h == w {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j-1] > s[j]; j-- {
			s[j-1], s[j] = s[j], s[j-1]
		}
	}
}

// fakeRetriever returns a fixed ranked result set.
type fakeRetriever struct {
	docs []retrieval.ScoredDoc
	err  error
}

func (r *fakeRetriever) Manifest() plugin.Manifest {
	return plugin.Manifest{Kind: plugin.KindRetriever, Name: "fake", Version: "1.0.0"}
}
func (r *fakeRetriever) Ingest(context.Context, []retrieval.Doc) error { return nil }
func (r *fakeRetriever) Query(context.Context, string, int) ([]retrieval.ScoredDoc, error) {
	return r.docs, r.err
}

func str(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

func mockCounter() tokens.Counter {
	c, _ := tokens.NewRegistry(nil).Select("mock", "sim-1")
	return c
}

// --- tests ---------------------------------------------------------------

// TestAssembleGolden pins a full four-kind assembly to a byte-exact preamble,
// so any change to the rendering format or ordering fails loudly.
func TestAssembleGolden(t *testing.T) {
	t.Parallel()

	spec := dag.ContextSpec{Sources: []dag.ContextSource{
		{Kind: dag.SourceLiteral, Name: "instructions", Text: "Be concise.", Pinned: true},
		{Kind: dag.SourceStepOutput, Name: "draft", Step: "writer", Path: "/text"},
		{Kind: dag.SourceBlackboard, Name: "findings", Key: "findings"},
		{Kind: dag.SourceBlackboard, Name: "notes", Tags: []string{"note"}},
		{Kind: dag.SourceRetrieval, Name: "guide", Retriever: "pg_fulltext", Query: "style"},
	}}
	board := &fakeBoard{heads: map[string]blackboard.Entry{
		"findings": {Key: "findings", Version: 2, Value: str(t, "sky is blue")},
		"note_a":   {Key: "note_a", Version: 1, Value: str(t, "first note"), Tags: []string{"note"}},
		"note_b":   {Key: "note_b", Version: 1, Value: str(t, map[string]int{"n": 2}), Tags: []string{"note"}},
	}}
	src := contextmgr.Sources{
		StepOutput: func(_ context.Context, id string) (json.RawMessage, bool, error) {
			if id == "writer" {
				return str(t, map[string]string{"text": "the drafted body"}), true, nil
			}
			return nil, false, nil
		},
		Board: board,
		Retriever: func(string) (retrieval.Retriever, error) {
			return &fakeRetriever{docs: []retrieval.ScoredDoc{
				{Doc: retrieval.Doc{ID: "d1", Content: "keep it short"}, Score: 0.9},
			}}, nil
		},
	}

	asm, err := contextmgr.Assemble(context.Background(), spec, mockCounter(), src)
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	golden := readGolden(t, "four_kinds.txt")
	if asm.Preamble != golden {
		t.Errorf("preamble mismatch.\n--- got ---\n%s\n--- want ---\n%s", asm.Preamble, golden)
	}
	if asm.CounterID != mockCounter().ID() {
		t.Errorf("CounterID = %q, want %q", asm.CounterID, mockCounter().ID())
	}
	if asm.ContextTokens != mockCounter().Count(golden) {
		t.Errorf("ContextTokens = %d, want %d", asm.ContextTokens, mockCounter().Count(golden))
	}
	// Every source included, in order.
	if len(asm.Sources) != 5 {
		t.Fatalf("got %d source reports, want 5", len(asm.Sources))
	}
	for i, r := range asm.Sources {
		if r.Index != i {
			t.Errorf("source %d has index %d", i, r.Index)
		}
		if r.Status != contextmgr.Included {
			t.Errorf("source %d status = %q, want included", i, r.Status)
		}
	}
	if !asm.Sources[0].Pinned {
		t.Error("literal source should be reported pinned")
	}
}

// TestAssembleDeterministic asserts byte-identical output across repeated
// assembly of the same state — the 12.3 acceptance criterion.
func TestAssembleDeterministic(t *testing.T) {
	t.Parallel()

	spec := dag.ContextSpec{Sources: []dag.ContextSource{
		{Kind: dag.SourceBlackboard, Name: "notes", Tags: []string{"note"}},
	}}
	board := &fakeBoard{heads: map[string]blackboard.Entry{
		"z": {Key: "z", Version: 1, Value: str(t, "zed"), Tags: []string{"note"}},
		"a": {Key: "a", Version: 1, Value: str(t, "aye"), Tags: []string{"note"}},
		"m": {Key: "m", Version: 1, Value: str(t, "em"), Tags: []string{"note"}},
	}}
	src := contextmgr.Sources{Board: board}
	first, err := contextmgr.Assemble(context.Background(), spec, mockCounter(), src)
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	for i := 0; i < 5; i++ {
		got, err := contextmgr.Assemble(context.Background(), spec, mockCounter(), src)
		if err != nil {
			t.Fatalf("Assemble: %v", err)
		}
		if got.Preamble != first.Preamble {
			t.Fatalf("non-deterministic preamble on run %d", i)
		}
	}
	// Tag-selected entries render in key order (a, m, z), not map order.
	ia := strings.Index(first.Preamble, "aye")
	im := strings.Index(first.Preamble, "em")
	iz := strings.Index(first.Preamble, "zed")
	if ia >= im || im >= iz {
		t.Errorf("tag entries not in key order: aye@%d em@%d zed@%d", ia, im, iz)
	}
}

// TestMissingPolicy covers error vs skip for every kind that can resolve to
// nothing.
func TestMissingPolicy(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		src  dag.ContextSource
	}{
		{"step_output not succeeded", dag.ContextSource{Kind: dag.SourceStepOutput, Step: "ghost"}},
		{"step_output bad pointer", dag.ContextSource{Kind: dag.SourceStepOutput, Step: "writer", Path: "/nope"}},
		{"blackboard missing key", dag.ContextSource{Kind: dag.SourceBlackboard, Key: "absent"}},
		{"blackboard no tag match", dag.ContextSource{Kind: dag.SourceBlackboard, Tags: []string{"none"}}},
		{"retrieval empty", dag.ContextSource{Kind: dag.SourceRetrieval, Retriever: "pg_fulltext", Query: "q"}},
	}
	base := contextmgr.Sources{
		StepOutput: func(_ context.Context, id string) (json.RawMessage, bool, error) {
			if id == "writer" {
				return str(t, map[string]string{"text": "hi"}), true, nil
			}
			return nil, false, nil
		},
		Board:     &fakeBoard{heads: map[string]blackboard.Entry{}},
		Retriever: func(string) (retrieval.Retriever, error) { return &fakeRetriever{}, nil },
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			// Default policy (error) → *MissingSourceError.
			errSpec := dag.ContextSpec{Sources: []dag.ContextSource{tc.src}}
			_, err := contextmgr.Assemble(context.Background(), errSpec, mockCounter(), base)
			var mse *contextmgr.MissingSourceError
			if !errors.As(err, &mse) {
				t.Fatalf("error policy: err = %v, want *MissingSourceError", err)
			}
			// skip policy → included=false, a Skipped report, no error.
			skipSrc := tc.src
			skipSrc.OnMissing = dag.MissingSkip
			skipSpec := dag.ContextSpec{Sources: []dag.ContextSource{skipSrc}}
			asm, err := contextmgr.Assemble(context.Background(), skipSpec, mockCounter(), base)
			if err != nil {
				t.Fatalf("skip policy: unexpected error %v", err)
			}
			if asm.Preamble != "" {
				t.Errorf("skip policy: preamble = %q, want empty", asm.Preamble)
			}
			if len(asm.Sources) != 1 || asm.Sources[0].Status != contextmgr.Skipped {
				t.Errorf("skip policy: reports = %+v, want one Skipped", asm.Sources)
			}
		})
	}
}

// TestConfigErrors covers the permanent config failures (unwired backend,
// unknown retriever) that fail regardless of the missing-policy.
func TestConfigErrors(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		spec dag.ContextSource
		src  contextmgr.Sources
	}{
		{
			"no board wired",
			dag.ContextSource{Kind: dag.SourceBlackboard, Key: "k", OnMissing: dag.MissingSkip},
			contextmgr.Sources{},
		},
		{
			"no retriever registry",
			dag.ContextSource{Kind: dag.SourceRetrieval, Retriever: "r", Query: "q", OnMissing: dag.MissingSkip},
			contextmgr.Sources{},
		},
		{
			"unknown retriever",
			dag.ContextSource{Kind: dag.SourceRetrieval, Retriever: "ghost", Query: "q", OnMissing: dag.MissingSkip},
			contextmgr.Sources{Retriever: func(string) (retrieval.Retriever, error) {
				return nil, &retrieval.UnknownRetrieverError{Name: "ghost"}
			}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			spec := dag.ContextSpec{Sources: []dag.ContextSource{tc.spec}}
			_, err := contextmgr.Assemble(context.Background(), spec, mockCounter(), tc.src)
			var ce *contextmgr.ConfigError
			if !errors.As(err, &ce) {
				t.Fatalf("err = %v, want *ConfigError", err)
			}
		})
	}
}

// TestTransportErrorPropagates asserts a backend transport error is returned
// as neither Missing nor Config (so the engine redelivers).
func TestTransportErrorPropagates(t *testing.T) {
	t.Parallel()

	boom := errors.New("db down")
	src := contextmgr.Sources{
		StepOutput: func(context.Context, string) (json.RawMessage, bool, error) { return nil, false, boom },
	}
	spec := dag.ContextSpec{Sources: []dag.ContextSource{{Kind: dag.SourceStepOutput, Step: "x"}}}
	_, err := contextmgr.Assemble(context.Background(), spec, mockCounter(), src)
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want wrapping db down", err)
	}
	var mse *contextmgr.MissingSourceError
	var ce *contextmgr.ConfigError
	if errors.As(err, &mse) || errors.As(err, &ce) {
		t.Error("transport error must not classify as Missing or Config")
	}
}

// TestTruncationCap asserts a capped source is truncated to at most the cap,
// deterministically, and a pinned source is never truncated.
func TestTruncationCap(t *testing.T) {
	t.Parallel()

	long := strings.Repeat("abcd ", 500) // 2500 chars → ~625 mock tokens
	counter := mockCounter()
	cap5 := 5
	src := contextmgr.Sources{
		StepOutput: func(context.Context, string) (json.RawMessage, bool, error) {
			return str(t, map[string]string{"text": long}), true, nil
		},
	}
	spec := dag.ContextSpec{Sources: []dag.ContextSource{
		{Kind: dag.SourceStepOutput, Name: "big", Step: "w", Path: "/text", MaxTokens: &cap5},
	}}
	asm, err := contextmgr.Assemble(context.Background(), spec, counter, src)
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	if asm.Sources[0].Status != contextmgr.Truncated {
		t.Fatalf("status = %q, want truncated", asm.Sources[0].Status)
	}
	if asm.Sources[0].Tokens > cap5 {
		t.Errorf("truncated content is %d tokens, over cap %d", asm.Sources[0].Tokens, cap5)
	}
	// Determinism.
	again, _ := contextmgr.Assemble(context.Background(), spec, counter, src)
	if again.Preamble != asm.Preamble {
		t.Error("truncation is not deterministic")
	}
	// Pinned source with the same content: never truncated.
	pinnedSpec := dag.ContextSpec{Sources: []dag.ContextSource{
		{Kind: dag.SourceStepOutput, Name: "big", Step: "w", Path: "/text", Pinned: true},
	}}
	pinned, err := contextmgr.Assemble(context.Background(), pinnedSpec, counter, src)
	if err != nil {
		t.Fatalf("Assemble pinned: %v", err)
	}
	if pinned.Sources[0].Status != contextmgr.Included {
		t.Errorf("pinned status = %q, want included (never truncated)", pinned.Sources[0].Status)
	}
	if !strings.Contains(pinned.Preamble, long) {
		t.Error("pinned source content was truncated")
	}
}

// readGolden loads a golden file, writing it when -update is set... but keep
// it simple: the golden is committed and read verbatim.
func readGolden(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name)) // #nosec G304 -- committed fixture path, test-only
	if err != nil {
		t.Fatalf("reading golden %s: %v", name, err)
	}
	return string(b)
}

// TestTruncationNeverExceedsCap is a property check: for a spread of content
// sizes and caps, a capped source's counted contribution is always <= its cap
// (the hard guarantee 12.4's pipeline rests on).
func TestTruncationNeverExceedsCap(t *testing.T) {
	t.Parallel()

	counter := mockCounter()
	for _, contentLen := range []int{0, 1, 7, 40, 400, 4000} {
		for _, capT := range []int{1, 2, 5, 20, 100} {
			body := strings.Repeat("wörd ", contentLen) // multibyte, to exercise rune boundaries
			capT := capT
			src := contextmgr.Sources{
				StepOutput: func(context.Context, string) (json.RawMessage, bool, error) {
					return str(t, map[string]string{"text": body}), true, nil
				},
			}
			spec := dag.ContextSpec{Sources: []dag.ContextSource{
				{Kind: dag.SourceStepOutput, Step: "w", Path: "/text", MaxTokens: &capT},
			}}
			asm, err := contextmgr.Assemble(context.Background(), spec, counter, src)
			if err != nil {
				t.Fatalf("Assemble(len=%d cap=%d): %v", contentLen, capT, err)
			}
			if asm.Sources[0].Tokens > capT {
				t.Errorf("len=%d cap=%d: contribution %d exceeds cap", contentLen, capT, asm.Sources[0].Tokens)
			}
		}
	}
}
