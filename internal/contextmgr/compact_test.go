package contextmgr_test

import (
	"errors"
	"strings"
	"testing"

	"pgregory.net/rapid"

	"github.com/mathcslearner/agentloom/internal/contextmgr"
	"github.com/mathcslearner/agentloom/internal/dag"
	"github.com/mathcslearner/agentloom/internal/tokens"
)

// measurer returns a MeasureFunc over the bare counter (the leaf tests budget
// the preamble directly; the engine budgets the whole framed request).
func measurer(c tokens.Counter) contextmgr.MeasureFunc {
	return func(preamble string) (int, error) { return c.Count(preamble), nil }
}

// ent builds a literal entry with a precomputed token count.
func ent(c tokens.Counter, idx int, name, content string, pinned bool, priority int) contextmgr.Entry {
	return contextmgr.Entry{
		SourceIndex: idx, Kind: dag.SourceLiteral, Name: name,
		Content: content, Tokens: c.Count(content), Pinned: pinned, Priority: priority,
	}
}

// asmOf assembles an Assembly from entries, deriving matching source reports.
func asmOf(c tokens.Counter, entries ...contextmgr.Entry) contextmgr.Assembly {
	reports := make([]contextmgr.SourceReport, len(entries))
	for i, e := range entries {
		reports[i] = contextmgr.SourceReport{
			Index: e.SourceIndex, Kind: e.Kind, Name: e.Name,
			Status: contextmgr.Included, Tokens: e.Tokens, Pinned: e.Pinned,
		}
	}
	pre := contextmgr.Render(entries)
	return contextmgr.Assembly{
		Preamble: pre, Sources: reports, Entries: entries,
		ContextTokens: c.Count(pre), CounterID: c.ID(),
	}
}

func intp(i int) *int { return &i }

// keptSet returns the names present in a compacted result.
func keptSet(cmp contextmgr.Compacted) map[string]bool {
	out := map[string]bool{}
	for _, e := range cmp.Entries {
		out[e.Name] = true
	}
	return out
}

// TestCompactUnderBudgetNoop: an assembly already under budget runs no strategy
// and returns the preamble unchanged with no revisions.
func TestCompactUnderBudgetNoop(t *testing.T) {
	t.Parallel()
	c := mockCounter()
	asm := asmOf(c, ent(c, 0, "a", strings.Repeat("x", 40), false, 0))
	m := measurer(c)
	total, _ := m(asm.Preamble)

	cmp, err := contextmgr.Compact(asm, contextmgr.Policy{Budget: total + 100, Pipeline: []dag.CompactionStrategy{
		{Strategy: dag.DropLowestPriority},
	}}, c, m)
	if err != nil {
		t.Fatalf("Compact: %v", err)
	}
	if cmp.Preamble != asm.Preamble {
		t.Error("preamble changed for an under-budget assembly")
	}
	if len(cmp.Revisions) != 0 {
		t.Errorf("want 0 revisions, got %d", len(cmp.Revisions))
	}
	if cmp.FinalTokens > total+100 {
		t.Errorf("FinalTokens %d over budget", cmp.FinalTokens)
	}
}

// TestCompactSlidingWindow keeps only the last N non-pinned entries; pinned
// entries survive regardless of position and do not count toward N.
func TestCompactSlidingWindow(t *testing.T) {
	t.Parallel()
	c := mockCounter()
	entries := []contextmgr.Entry{
		ent(c, 0, "pin", strings.Repeat("p", 40), true, 0),
		ent(c, 1, "e1", strings.Repeat("a", 40), false, 0),
		ent(c, 2, "e2", strings.Repeat("b", 40), false, 0),
		ent(c, 3, "e3", strings.Repeat("cc", 40), false, 0),
		ent(c, 4, "e4", strings.Repeat("dd", 40), false, 0),
	}
	asm := asmOf(c, entries...)
	m := measurer(c)
	// Budget = exactly the survivors (pin + last 2 non-pinned).
	budget, _ := m(contextmgr.Render([]contextmgr.Entry{entries[0], entries[3], entries[4]}))

	cmp, err := contextmgr.Compact(asm, contextmgr.Policy{Budget: budget, Pipeline: []dag.CompactionStrategy{
		{Strategy: dag.SlidingWindow, N: intp(2)},
	}}, c, m)
	if err != nil {
		t.Fatalf("Compact: %v", err)
	}
	kept := keptSet(cmp)
	for _, name := range []string{"pin", "e3", "e4"} {
		if !kept[name] {
			t.Errorf("sliding_window dropped %q, want kept", name)
		}
	}
	for _, name := range []string{"e1", "e2"} {
		if kept[name] {
			t.Errorf("sliding_window kept %q, want dropped", name)
		}
	}
	if cmp.FinalTokens > budget {
		t.Errorf("FinalTokens %d over budget %d", cmp.FinalTokens, budget)
	}
	if len(cmp.Revisions) != 1 || cmp.Revisions[0].Strategy != "sliding_window" || !cmp.Revisions[0].Changed {
		t.Errorf("unexpected revisions: %+v", cmp.Revisions)
	}
}

// TestCompactDropLowestPriority evicts the lowest-priority non-pinned entry
// first, one at a time, never touching a pinned entry.
func TestCompactDropLowestPriority(t *testing.T) {
	t.Parallel()
	c := mockCounter()
	entries := []contextmgr.Entry{
		ent(c, 0, "hi", strings.Repeat("h", 40), false, 5),
		ent(c, 1, "lo", strings.Repeat("l", 40), false, 1),
		ent(c, 2, "mid", strings.Repeat("m", 40), false, 3),
		ent(c, 3, "pin", strings.Repeat("p", 40), true, 0),
	}
	asm := asmOf(c, entries...)
	m := measurer(c)
	// Budget = everything except the lowest priority ("lo"): exactly one drop.
	budget, _ := m(contextmgr.Render([]contextmgr.Entry{entries[0], entries[2], entries[3]}))

	cmp, err := contextmgr.Compact(asm, contextmgr.Policy{Budget: budget, Pipeline: []dag.CompactionStrategy{
		{Strategy: dag.DropLowestPriority},
	}}, c, m)
	if err != nil {
		t.Fatalf("Compact: %v", err)
	}
	kept := keptSet(cmp)
	if kept["lo"] {
		t.Error("lowest-priority entry not dropped")
	}
	for _, name := range []string{"hi", "mid", "pin"} {
		if !kept[name] {
			t.Errorf("dropped %q, want kept", name)
		}
	}
	if cmp.FinalTokens > budget {
		t.Errorf("FinalTokens %d over budget %d", cmp.FinalTokens, budget)
	}
	// The dropped source is reported Dropped.
	var loStatus contextmgr.Disposition
	for _, r := range cmp.Sources {
		if r.Name == "lo" {
			loStatus = r.Status
		}
	}
	if loStatus != contextmgr.Dropped {
		t.Errorf("lo source status = %q, want dropped", loStatus)
	}
}

// TestCompactTruncateOldest middle-out-truncates the oldest non-pinned entry
// (leaving a marker) and never truncates a pinned entry.
func TestCompactTruncateOldest(t *testing.T) {
	t.Parallel()
	c := mockCounter()
	entries := []contextmgr.Entry{
		ent(c, 0, "old", strings.Repeat("OLDCONTENT", 100), false, 0),
		ent(c, 1, "new", strings.Repeat("n", 20), false, 0),
		ent(c, 2, "pin", strings.Repeat("PINNED", 100), true, 0),
	}
	asm := asmOf(c, entries...)
	m := measurer(c)
	full, _ := m(asm.Preamble)
	budget := full - 100 // force truncation of the oldest

	cmp, err := contextmgr.Compact(asm, contextmgr.Policy{Budget: budget, Pipeline: []dag.CompactionStrategy{
		{Strategy: dag.TruncateOldest, MinTokens: intp(4)},
	}}, c, m)
	if err != nil {
		t.Fatalf("Compact: %v", err)
	}
	if cmp.FinalTokens > budget {
		t.Errorf("FinalTokens %d over budget %d", cmp.FinalTokens, budget)
	}
	if !strings.Contains(cmp.Preamble, "elided") {
		t.Error("truncated preamble has no elision marker")
	}
	// The pinned entry's content is present verbatim.
	if !strings.Contains(cmp.Preamble, strings.Repeat("PINNED", 100)) {
		t.Error("pinned entry was altered by truncate_oldest")
	}
}

// TestCompactPinnedExceedsBudget: when the pinned set alone is over budget, the
// pipeline cannot fit and reports a PinnedOnly OverBudgetError, never dropping
// the pin.
func TestCompactPinnedExceedsBudget(t *testing.T) {
	t.Parallel()
	c := mockCounter()
	asm := asmOf(c,
		ent(c, 0, "pin", strings.Repeat("P", 4000), true, 0),
		ent(c, 1, "other", strings.Repeat("o", 40), false, 0),
	)
	m := measurer(c)
	_, err := contextmgr.Compact(asm, contextmgr.Policy{Budget: 10, Pipeline: []dag.CompactionStrategy{
		{Strategy: dag.DropLowestPriority},
		{Strategy: dag.SlidingWindow, N: intp(1)},
	}}, c, m)
	var obe *contextmgr.OverBudgetError
	if !errors.As(err, &obe) {
		t.Fatalf("want *OverBudgetError, got %v", err)
	}
	if !obe.PinnedOnly {
		t.Errorf("want PinnedOnly, got %+v", obe)
	}
}

// TestCompactEmptyPipelineGuardrail: over budget with no strategies is a pure
// guardrail failure — a typed OverBudgetError with nothing applied.
func TestCompactEmptyPipelineGuardrail(t *testing.T) {
	t.Parallel()
	c := mockCounter()
	asm := asmOf(c, ent(c, 0, "a", strings.Repeat("x", 400), false, 0))
	m := measurer(c)
	_, err := contextmgr.Compact(asm, contextmgr.Policy{Budget: 5}, c, m)
	var obe *contextmgr.OverBudgetError
	if !errors.As(err, &obe) {
		t.Fatalf("want *OverBudgetError, got %v", err)
	}
	if len(obe.Applied) != 0 {
		t.Errorf("want no strategies applied, got %v", obe.Applied)
	}
}

// TestCompactDeterministic: the same assembly, policy, and counter produce
// byte-identical preambles and identical revision trails.
func TestCompactDeterministic(t *testing.T) {
	t.Parallel()
	c := mockCounter()
	build := func() contextmgr.Assembly {
		return asmOf(c,
			ent(c, 0, "pin", strings.Repeat("p", 40), true, 0),
			ent(c, 1, "a", strings.Repeat("a", 200), false, 2),
			ent(c, 2, "b", strings.Repeat("b", 200), false, 1),
			ent(c, 3, "d", strings.Repeat("d", 200), false, 3),
		)
	}
	m := measurer(c)
	pol := contextmgr.Policy{Budget: 30, Pipeline: []dag.CompactionStrategy{
		{Strategy: dag.SlidingWindow, N: intp(2)},
		{Strategy: dag.TruncateOldest, MinTokens: intp(4)},
		{Strategy: dag.DropLowestPriority},
	}}
	first, err := contextmgr.Compact(build(), pol, c, m)
	if err != nil {
		var obe *contextmgr.OverBudgetError
		if !errors.As(err, &obe) {
			t.Fatalf("Compact: %v", err)
		}
	}
	for i := 0; i < 20; i++ {
		got, gerr := contextmgr.Compact(build(), pol, c, m)
		if (err == nil) != (gerr == nil) {
			t.Fatalf("determinism: error mismatch %v vs %v", err, gerr)
		}
		if err == nil && got.Preamble != first.Preamble {
			t.Fatal("determinism: preamble differs across runs")
		}
	}
}

// TestCompactProperty is the ADR-014 hard guarantee: for an arbitrary oversized
// assembly and pipeline, the pipeline output is <= budget or a typed
// OverBudgetError, and every pinned entry survives byte-identical across every
// strategy.
func TestCompactProperty(t *testing.T) {
	t.Parallel()
	c := mockCounter()
	m := measurer(c)

	rapid.Check(t, func(t *rapid.T) {
		n := rapid.IntRange(1, 8).Draw(t, "n")
		entries := make([]contextmgr.Entry, n)
		pinnedContent := map[string]string{}
		for i := 0; i < n; i++ {
			length := rapid.IntRange(0, 400).Draw(t, "len")
			content := strings.Repeat(string(rune('a'+i)), length)
			pinned := rapid.Bool().Draw(t, "pinned")
			priority := rapid.IntRange(-3, 3).Draw(t, "priority")
			name := "s" + string(rune('A'+i))
			entries[i] = ent(c, i, name, content, pinned, priority)
			if pinned {
				pinnedContent[name] = content
			}
		}
		asm := asmOf(c, entries...)
		budget := rapid.IntRange(1, 300).Draw(t, "budget")

		strategyPool := []dag.CompactionStrategy{
			{Strategy: dag.SlidingWindow, N: intp(rapid.IntRange(1, 5).Draw(t, "n"))},
			{Strategy: dag.TruncateOldest, MinTokens: intp(rapid.IntRange(0, 10).Draw(t, "min"))},
			{Strategy: dag.DropLowestPriority},
		}
		k := rapid.IntRange(0, len(strategyPool)).Draw(t, "pipelen")
		pipeline := strategyPool[:k]

		cmp, err := contextmgr.Compact(asm, contextmgr.Policy{Budget: budget, Pipeline: pipeline}, c, m)
		if err != nil {
			var obe *contextmgr.OverBudgetError
			if !errors.As(err, &obe) {
				t.Fatalf("non-OverBudget error: %v", err)
			}
			return // an over-budget failure is a valid outcome
		}
		// Hard guarantee: fits the budget.
		if cmp.FinalTokens > budget {
			t.Fatalf("FinalTokens %d > budget %d", cmp.FinalTokens, budget)
		}
		// Pinned entries survive byte-identical.
		for name, content := range pinnedContent {
			found := false
			for _, e := range cmp.Entries {
				if e.Name == name {
					found = true
					if e.Content != content {
						t.Fatalf("pinned entry %q was altered", name)
					}
				}
			}
			if !found {
				t.Fatalf("pinned entry %q was dropped", name)
			}
		}
	})
}
