package contextmgr

// Deterministic compaction (ADR-014, ticket 12.4). When an assembled context
// makes the framed request exceed the step's token budget, Compact applies the
// step's ordered strategy pipeline — drop_lowest_priority, truncate_oldest,
// sliding_window — each strategy only while still over budget, until the
// request fits or a typed OverBudgetError fires (the caller turns that into a
// permanent step failure before any provider call). Pinned entries are never
// dropped or truncated by any strategy; a pipeline that cannot fit the budget
// with the pinned set alone yields OverBudgetError, never a silently-dropped
// pin.
//
// Compaction is deterministic given the assembly and the counter: every
// ordering is by declaration position (or by an author-declared priority), and
// truncation is a binary search with a fixed elision marker. The budget is
// measured over the WHOLE framed request (the caller's MeasureFunc injects the
// preamble and counts the request), not the preamble alone, so "assembled <=
// budget" is the same arithmetic 12.6's window guardrail uses. Because BPE
// counts are not additive, each strategy re-measures the request after it acts
// rather than trusting a per-entry sum.

import (
	"fmt"
	"strings"

	"github.com/mathcslearner/agentloom/internal/dag"
	"github.com/mathcslearner/agentloom/internal/tokens"
)

// elisionMarker replaces the excised middle of a truncate_oldest entry — a
// fixed string (no embedded count) so truncation stays a pure, deterministic
// function of the input and the target.
const elisionMarker = "\n…[elided]…\n"

// Policy is the compaction input: the token budget the framed request must fit
// under and the ordered strategy pipeline applied while over it.
type Policy struct {
	// Budget is the maximum framed-request token count (measured by MeasureFunc).
	Budget int
	// Pipeline is the ordered compaction strategy list; empty means a pure
	// pre-flight guardrail (over budget → OverBudgetError, nothing dropped).
	Pipeline []dag.CompactionStrategy
}

// MeasureFunc counts the whole framed request for a candidate assembled
// preamble. The engine supplies inject-preamble-then-count-request; tests can
// supply the bare counter. Compact compares its result to Policy.Budget. An
// error is deterministic (the request could not be built) and aborts
// compaction — the caller treats it as a permanent failure.
type MeasureFunc func(preamble string) (int, error)

// EntryAction records one strategy's effect on a single entry (audit detail).
type EntryAction struct {
	SourceIndex  int    `json:"source_index"`
	Name         string `json:"name"`
	Action       string `json:"action"` // "dropped" | "truncated"
	TokensBefore int    `json:"tokens_before"`
	TokensAfter  int    `json:"tokens_after"`
}

// Revision records one strategy application — the content of a context_revision
// event (ADR-014): which strategy ran, its parameters, the framed-request
// tokens before and after, and the per-entry actions.
type Revision struct {
	// Index is the strategy's position in the pipeline.
	Index int `json:"index"`
	// Strategy is the strategy kind that ran.
	Strategy string `json:"strategy"`
	// N / MinTokens echo the strategy's parameters when set.
	N         *int `json:"n,omitempty"`
	MinTokens *int `json:"min_tokens,omitempty"`
	// Budget is the token budget in force.
	Budget int `json:"budget"`
	// TokensBefore / TokensAfter are the framed-request totals around this
	// strategy (the number compared to Budget).
	TokensBefore int `json:"tokens_before"`
	TokensAfter  int `json:"tokens_after"`
	// Changed reports whether the strategy altered the assembly (a strategy can
	// run yet no-op — a sliding_window wider than the live set, a truncate with
	// everything already at its floor).
	Changed bool `json:"changed"`
	// Actions is the per-entry drop/truncate detail.
	Actions []EntryAction `json:"actions,omitempty"`
	// Kept is the names of the entries still present after this strategy, in
	// message order.
	Kept []string `json:"kept,omitempty"`
}

// Compacted is the product of Compact: the (possibly shrunk) preamble, the
// updated per-source dispositions, the surviving entries, the assembled-context
// and framed-request token totals, and the revision audit trail.
type Compacted struct {
	Preamble      string
	Sources       []SourceReport
	Entries       []Entry
	ContextTokens int
	FinalTokens   int
	Revisions     []Revision
}

// OverBudgetError reports a pipeline that could not fit the budget. It is
// deterministic, so the engine records a permanent step failure before any
// provider call (ADR-014's hard guarantee: overflow is never sent).
type OverBudgetError struct {
	// Budget is the token budget the request had to fit under.
	Budget int
	// Tokens is the final framed-request total, still over Budget.
	Tokens int
	// PinnedTokens is the framed-request total with only pinned entries.
	PinnedTokens int
	// PinnedOnly is true when even the pinned set alone exceeds Budget — the
	// pins cannot be honored, distinct from "the non-pinned set could not be
	// shrunk enough."
	PinnedOnly bool
	// Applied is the strategies that ran (in order).
	Applied []string
}

func (e *OverBudgetError) Error() string {
	if e.PinnedOnly {
		return fmt.Sprintf("context over budget: pinned sources alone are %d tokens, budget %d (applied: %s)",
			e.PinnedTokens, e.Budget, strings.Join(e.Applied, ","))
	}
	return fmt.Sprintf("context over budget: %d tokens after compaction, budget %d (applied: %s)",
		e.Tokens, e.Budget, strings.Join(e.Applied, ","))
}

// Compact shrinks asm's assembled context to fit pol.Budget by applying
// pol.Pipeline in order, each strategy only while the framed request is still
// over budget. It never mutates asm. On success the request fits the budget (or
// was already under it, in which case no strategy runs and Revisions is empty);
// otherwise it returns *OverBudgetError. A MeasureFunc error aborts with that
// error wrapped (deterministic — a request that cannot be built).
func Compact(asm Assembly, pol Policy, counter tokens.Counter, measure MeasureFunc) (Compacted, error) {
	// Work on a copy so the caller's Assembly is untouched (the no-compaction
	// path below returns it verbatim).
	entries := append([]Entry(nil), asm.Entries...)
	dropped := make(map[int]bool)
	truncatedByStrategy := make(map[int]bool)

	current, err := measure(renderLive(entries, dropped))
	if err != nil {
		return Compacted{}, fmt.Errorf("contextmgr: measuring assembly: %w", err)
	}

	var revisions []Revision
	var applied []string
	for idx, st := range pol.Pipeline {
		if current <= pol.Budget {
			break
		}
		before := current
		actions, err := applyStrategy(st, entries, dropped, truncatedByStrategy, pol.Budget, counter, measure)
		if err != nil {
			return Compacted{}, err
		}
		after, err := measure(renderLive(entries, dropped))
		if err != nil {
			return Compacted{}, fmt.Errorf("contextmgr: re-measuring after %s: %w", st.Strategy, err)
		}
		current = after
		applied = append(applied, string(st.Strategy))
		revisions = append(revisions, Revision{
			Index: idx, Strategy: string(st.Strategy), N: st.N, MinTokens: st.MinTokens,
			Budget: pol.Budget, TokensBefore: before, TokensAfter: after,
			Changed: len(actions) > 0, Actions: actions, Kept: keptNames(entries, dropped),
		})
	}

	live := liveEntries(entries, dropped)
	preamble := Render(live)

	if current > pol.Budget {
		pinnedTokens, perr := measure(Render(pinnedEntries(entries)))
		if perr != nil {
			return Compacted{}, fmt.Errorf("contextmgr: measuring pinned set: %w", perr)
		}
		return Compacted{}, &OverBudgetError{
			Budget: pol.Budget, Tokens: current, PinnedTokens: pinnedTokens,
			PinnedOnly: pinnedTokens > pol.Budget, Applied: applied,
		}
	}

	return Compacted{
		Preamble:      preamble,
		Sources:       compactedReports(asm.Sources, entries, dropped, truncatedByStrategy),
		Entries:       live,
		ContextTokens: counter.Count(preamble),
		FinalTokens:   current,
		Revisions:     revisions,
	}, nil
}

// applyStrategy dispatches one strategy over the working entries, mutating the
// dropped/truncated sets and entry contents in place, and returns the per-entry
// actions it took.
func applyStrategy(st dag.CompactionStrategy, entries []Entry, dropped, truncatedByStrategy map[int]bool, budget int, counter tokens.Counter, measure MeasureFunc) ([]EntryAction, error) {
	switch st.Strategy {
	case dag.SlidingWindow:
		n := 0
		if st.N != nil {
			n = *st.N
		}
		return slidingWindow(entries, dropped, n), nil
	case dag.DropLowestPriority:
		return dropLowestPriority(entries, dropped, budget, measure)
	case dag.TruncateOldest:
		floor := 0
		if st.MinTokens != nil {
			floor = *st.MinTokens
		}
		return truncateOldest(entries, dropped, truncatedByStrategy, budget, floor, counter, measure)
	}
	// Unreachable — Validate rejects unknown/reserved strategies — but be safe.
	return nil, nil
}

// slidingWindow keeps only the last n non-pinned live entries in message order,
// dropping the earlier non-pinned entries. Pinned entries are always kept and
// do not count toward n. One-shot: it applies the window once; a still-over
// request is handled by the next strategy.
func slidingWindow(entries []Entry, dropped map[int]bool, n int) []EntryAction {
	var live []int
	for i := range entries {
		if entries[i].Pinned || dropped[entries[i].SourceIndex] {
			continue
		}
		live = append(live, i)
	}
	if len(live) <= n {
		return nil
	}
	var actions []EntryAction
	for _, i := range live[:len(live)-n] {
		dropped[entries[i].SourceIndex] = true
		actions = append(actions, EntryAction{
			SourceIndex: entries[i].SourceIndex, Name: entries[i].Name,
			Action: "dropped", TokensBefore: entries[i].Tokens, TokensAfter: 0,
		})
	}
	return actions
}

// dropLowestPriority evicts one non-pinned entry at a time — lowest priority
// first, ties broken by later declaration — re-measuring after each, until the
// request fits or no droppable entry remains.
func dropLowestPriority(entries []Entry, dropped map[int]bool, budget int, measure MeasureFunc) ([]EntryAction, error) {
	var actions []EntryAction
	for {
		cur, err := measure(renderLive(entries, dropped))
		if err != nil {
			return nil, err
		}
		if cur <= budget {
			break
		}
		victim := -1
		for i := range entries {
			e := entries[i]
			if e.Pinned || dropped[e.SourceIndex] {
				continue
			}
			if victim == -1 {
				victim = i
				continue
			}
			v := entries[victim]
			if e.Priority < v.Priority || (e.Priority == v.Priority && e.SourceIndex > v.SourceIndex) {
				victim = i
			}
		}
		if victim == -1 {
			break
		}
		dropped[entries[victim].SourceIndex] = true
		actions = append(actions, EntryAction{
			SourceIndex: entries[victim].SourceIndex, Name: entries[victim].Name,
			Action: "dropped", TokensBefore: entries[victim].Tokens, TokensAfter: 0,
		})
	}
	return actions, nil
}

// truncateOldest middle-out-truncates the oldest (lowest-index) non-pinned live
// entry above the floor, re-measuring after each, moving to the next-oldest
// while still over budget, until the request fits or every non-pinned entry is
// at its floor.
func truncateOldest(entries []Entry, dropped, truncatedByStrategy map[int]bool, budget, floor int, counter tokens.Counter, measure MeasureFunc) ([]EntryAction, error) {
	actionByIdx := make(map[int]*EntryAction)
	var order []int
	for {
		cur, err := measure(renderLive(entries, dropped))
		if err != nil {
			return nil, err
		}
		if cur <= budget {
			break
		}
		victim := -1
		for i := range entries {
			e := entries[i]
			if e.Pinned || dropped[e.SourceIndex] || e.Tokens <= floor {
				continue
			}
			if victim == -1 || e.SourceIndex < entries[victim].SourceIndex {
				victim = i
			}
		}
		if victim == -1 {
			break
		}
		e := &entries[victim]
		before := e.Tokens
		over := cur - budget
		target := e.Tokens - over
		if target < floor {
			target = floor
		}
		if target >= e.Tokens { // guarantee progress; safe because e.Tokens > floor
			target = e.Tokens - 1
		}
		newContent, newTokens := truncateMiddle(e.Content, target, counter)
		if newTokens >= before { // truncation could not shrink (marker overhead) — stop
			break
		}
		e.Content = newContent
		e.Tokens = newTokens
		truncatedByStrategy[e.SourceIndex] = true
		if a, ok := actionByIdx[e.SourceIndex]; ok {
			a.TokensAfter = newTokens
		} else {
			actionByIdx[e.SourceIndex] = &EntryAction{
				SourceIndex: e.SourceIndex, Name: e.Name, Action: "truncated",
				TokensBefore: before, TokensAfter: newTokens,
			}
			order = append(order, e.SourceIndex)
		}
	}
	actions := make([]EntryAction, 0, len(order))
	for _, idx := range order {
		actions = append(actions, *actionByIdx[idx])
	}
	return actions, nil
}

// truncateMiddle keeps a head prefix and a tail suffix of text with the elision
// marker between, the largest rune split whose counted length is at most
// target. Pure and deterministic (a binary search over the rune split); the
// returned string is guaranteed <= target tokens (when the marker alone fits,
// else empty content). Middle-out preserves both the start and end of the
// entry, which usually carry the most signal.
func truncateMiddle(text string, target int, counter tokens.Counter) (string, int) {
	if counter.Count(text) <= target {
		return text, counter.Count(text)
	}
	runes := []rune(text)
	if counter.Count(elisionMarker) > target {
		return "", 0 // cannot fit even the marker; drop the content
	}
	build := func(k int) string {
		headN := (k + 1) / 2
		tailN := k / 2
		if headN+tailN > len(runes) {
			headN, tailN = len(runes), 0
		}
		return string(runes[:headN]) + elisionMarker + string(runes[len(runes)-tailN:])
	}
	lo, hi, best := 0, len(runes), 0
	for lo <= hi {
		mid := (lo + hi) / 2
		if counter.Count(build(mid)) <= target {
			best = mid
			lo = mid + 1
		} else {
			hi = mid - 1
		}
	}
	result := build(best)
	return result, counter.Count(result)
}

// renderLive renders the entries that have not been dropped, in message order.
func renderLive(entries []Entry, dropped map[int]bool) string {
	return Render(liveEntries(entries, dropped))
}

// liveEntries returns the non-dropped entries in message order.
func liveEntries(entries []Entry, dropped map[int]bool) []Entry {
	live := make([]Entry, 0, len(entries))
	for _, e := range entries {
		if dropped[e.SourceIndex] {
			continue
		}
		live = append(live, e)
	}
	return live
}

// pinnedEntries returns the pinned entries in message order (for the
// pinned-only budget check).
func pinnedEntries(entries []Entry) []Entry {
	var out []Entry
	for _, e := range entries {
		if e.Pinned {
			out = append(out, e)
		}
	}
	return out
}

// keptNames lists the non-dropped entries' names in message order.
func keptNames(entries []Entry, dropped map[int]bool) []string {
	var out []string
	for _, e := range entries {
		if dropped[e.SourceIndex] {
			continue
		}
		out = append(out, e.Name)
	}
	return out
}

// compactedReports folds the compaction outcome into the assembly's per-source
// reports: dropped sources become Dropped, strategy-truncated sources become
// Truncated with their new token count, and untouched sources keep their
// assembly disposition (Included, or per-source-cap Truncated).
func compactedReports(base []SourceReport, entries []Entry, dropped, truncatedByStrategy map[int]bool) []SourceReport {
	reports := make([]SourceReport, len(base))
	copy(reports, base)
	byIdx := make(map[int]int, len(reports))
	for i := range reports {
		byIdx[reports[i].Index] = i
	}
	for _, e := range entries {
		r := &reports[byIdx[e.SourceIndex]]
		switch {
		case dropped[e.SourceIndex]:
			r.Status = Dropped
			r.Reason = "dropped by compaction"
			r.Tokens = 0
		case truncatedByStrategy[e.SourceIndex]:
			r.Status = Truncated
			r.Reason = fmt.Sprintf("truncated by compaction to %d tokens", e.Tokens)
			r.Tokens = e.Tokens
		default:
			r.Tokens = e.Tokens
		}
	}
	return reports
}
