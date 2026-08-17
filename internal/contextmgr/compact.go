package contextmgr

// Deterministic compaction (ADR-014, tickets 12.4/12.5). When an assembled
// context makes the framed request exceed the step's token budget, Compact
// applies the step's ordered strategy pipeline — drop_lowest_priority,
// truncate_oldest, sliding_window, summarize — each strategy only while still
// over budget, until the request fits or a typed OverBudgetError fires (the
// caller turns that into a permanent step failure before any provider call).
// Pinned entries are never dropped, truncated, or summarized by any strategy; a
// pipeline that cannot fit the budget with the pinned set alone yields
// OverBudgetError, never a silently-dropped pin.
//
// The three deterministic strategies are pure functions of the assembly and the
// counter: every ordering is by message position (or by an author-declared
// priority), and truncation is a binary search with a fixed elision marker. The
// summarize strategy (12.5) calls a cheap model and so is not pure, but it is
// operationally deterministic (a fixed temperature-0 prompt, cached by the
// engine) and its failure never blocks the step — Compact falls back to the
// next deterministic strategy and records the failure on the revision. The
// budget is measured over the WHOLE framed request (the caller's MeasureFunc
// injects the preamble and counts the request), not the preamble alone, so
// "assembled <= budget" is the same arithmetic 12.6's window guardrail uses.
// Because BPE counts are not additive, each strategy re-measures the request
// after it acts rather than trusting a per-entry sum.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/mathcslearner/agentloom/internal/blackboard"
	"github.com/mathcslearner/agentloom/internal/dag"
	"github.com/mathcslearner/agentloom/internal/tokens"
)

// elisionMarker replaces the excised middle of a truncate_oldest entry — a
// fixed string (no embedded count) so truncation stays a pure, deterministic
// function of the input and the target.
const elisionMarker = "\n…[elided]…\n"

// Policy is the compaction input: the token budget the framed request must fit
// under, the ordered strategy pipeline applied while over it, and the backends
// the summarize strategy needs (nil for a pipeline with no summarize).
type Policy struct {
	// Budget is the maximum framed-request token count (measured by MeasureFunc).
	Budget int
	// Pipeline is the ordered compaction strategy list; empty means a pure
	// pre-flight guardrail (over budget → OverBudgetError, nothing dropped).
	Pipeline []dag.CompactionStrategy
	// Summarizer summarizes an evicted span (ticket 12.5); nil (or an error
	// from it) makes a summarize strategy fall back to the next strategy.
	Summarizer Summarizer
	// Board is the run/step-scoped blackboard a summarize strategy writes its
	// summary to; nil makes a summarize strategy fall back.
	Board blackboard.Board
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
	Action       string `json:"action"` // "dropped" | "truncated" | "summarized"
	TokensBefore int    `json:"tokens_before"`
	TokensAfter  int    `json:"tokens_after"`
}

// Revision records one strategy application — the content of a context_revision
// event (ADR-014): which strategy ran, its parameters, the framed-request
// tokens before and after, the per-entry actions, and (for summarize) the
// summary provenance and any failure that triggered a fallback.
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
	// everything already at its floor, or a summarizer that fell back).
	Changed bool `json:"changed"`
	// Actions is the per-entry drop/truncate/summarize detail.
	Actions []EntryAction `json:"actions,omitempty"`
	// Summaries is the summarization detail (summarize only): the blackboard
	// key@version the summary landed at and the overhead usage.
	Summaries []SummaryAction `json:"summaries,omitempty"`
	// Error, when set, is why a summarize strategy fell back (the warning event
	// content) — the summarizer was unavailable, errored, timed out, or could
	// not shrink the span. A non-summarize strategy never sets it.
	Error string `json:"error,omitempty"`
	// Kept is the names of the entries still present after this strategy, in
	// message order.
	Kept []string `json:"kept,omitempty"`
}

// Compacted is the product of Compact: the (possibly shrunk) preamble, the
// updated per-source dispositions, the surviving entries, the assembled-context
// and framed-request token totals, the revision audit trail, and the
// summarizations (for the engine's overhead pricing).
type Compacted struct {
	Preamble      string
	Sources       []SourceReport
	Entries       []Entry
	ContextTokens int
	FinalTokens   int
	Revisions     []Revision
	// Summaries aggregates every successful summarization across the pipeline —
	// the engine prices each as compaction overhead (ADR-012 rule 4).
	Summaries []SummaryAction
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
	// Revisions is the audit trail of the strategies that ran before the
	// pipeline gave up (ticket 12.5): the caller records them so a failed
	// compaction is auditable, not silent.
	Revisions []Revision
	// Summaries is any summarization that billed before the pipeline gave up —
	// the caller ledgers the overhead even on failure (the money was spent).
	Summaries []SummaryAction
}

func (e *OverBudgetError) Error() string {
	if e.PinnedOnly {
		return fmt.Sprintf("context over budget: pinned sources alone are %d tokens, budget %d (applied: %s)",
			e.PinnedTokens, e.Budget, strings.Join(e.Applied, ","))
	}
	return fmt.Sprintf("context over budget: %d tokens after compaction, budget %d (applied: %s)",
		e.Tokens, e.Budget, strings.Join(e.Applied, ","))
}

// strategyOutcome is one strategy application's audit detail, separate from the
// (possibly grown) entries slice the strategy returns.
type strategyOutcome struct {
	actions   []EntryAction
	summaries []SummaryAction
	// errMsg, when set, is a summarize fallback reason recorded on the revision
	// (the warning event); it is not a fatal error.
	errMsg string
}

// compactState is the mutable working set threaded through the strategies.
type compactState struct {
	entries          []Entry        // message order; summarize splices in synthetic entries
	dropped          map[int]bool   // by Entry.SourceIndex
	truncated        map[int]bool   // by Entry.SourceIndex (strategy truncation)
	summarizedInto   map[int]string // span SourceIndex -> "key vN" ref (for the report)
	nextSyntheticIdx int            // decreasing negative ids for summary entries
}

// Compact shrinks asm's assembled context to fit pol.Budget by applying
// pol.Pipeline in order, each strategy only while the framed request is still
// over budget. It never mutates asm. On success the request fits the budget (or
// was already under it, in which case no strategy runs and Revisions is empty);
// otherwise it returns *OverBudgetError. A MeasureFunc error, or a
// caller-context cancellation surfaced by the summarizer, aborts with that error
// wrapped (the caller treats a MeasureFunc/OverBudget failure as permanent and a
// context cancellation as transport).
func Compact(ctx context.Context, asm Assembly, pol Policy, counter tokens.Counter, measure MeasureFunc) (Compacted, error) {
	st := &compactState{
		entries:          append([]Entry(nil), asm.Entries...),
		dropped:          make(map[int]bool),
		truncated:        make(map[int]bool),
		summarizedInto:   make(map[int]string),
		nextSyntheticIdx: -1,
	}

	current, err := measure(renderLive(st.entries, st.dropped))
	if err != nil {
		return Compacted{}, fmt.Errorf("contextmgr: measuring assembly: %w", err)
	}

	var revisions []Revision
	var applied []string
	var allSummaries []SummaryAction
	for idx, stgy := range pol.Pipeline {
		if current <= pol.Budget {
			break
		}
		before := current
		outcome, ferr := applyStrategy(ctx, stgy, st, pol, counter, measure)
		if ferr != nil {
			return Compacted{}, ferr // fatal: MeasureFunc or context cancellation
		}
		after, merr := measure(renderLive(st.entries, st.dropped))
		if merr != nil {
			return Compacted{}, fmt.Errorf("contextmgr: re-measuring after %s: %w", stgy.Strategy, merr)
		}
		current = after
		applied = append(applied, string(stgy.Strategy))
		allSummaries = append(allSummaries, outcome.summaries...)
		revisions = append(revisions, Revision{
			Index: idx, Strategy: string(stgy.Strategy), N: stgy.N, MinTokens: stgy.MinTokens,
			Budget: pol.Budget, TokensBefore: before, TokensAfter: after,
			Changed: len(outcome.actions) > 0, Actions: outcome.actions,
			Summaries: outcome.summaries, Error: outcome.errMsg,
			Kept: keptNames(st.entries, st.dropped),
		})
	}

	live := liveEntries(st.entries, st.dropped)
	preamble := Render(live)

	if current > pol.Budget {
		pinnedTokens, perr := measure(Render(pinnedEntries(st.entries)))
		if perr != nil {
			return Compacted{}, fmt.Errorf("contextmgr: measuring pinned set: %w", perr)
		}
		return Compacted{}, &OverBudgetError{
			Budget: pol.Budget, Tokens: current, PinnedTokens: pinnedTokens,
			PinnedOnly: pinnedTokens > pol.Budget, Applied: applied,
			Revisions: revisions, Summaries: allSummaries,
		}
	}

	return Compacted{
		Preamble:      preamble,
		Sources:       compactedReports(asm.Sources, st),
		Entries:       live,
		ContextTokens: counter.Count(preamble),
		FinalTokens:   current,
		Revisions:     revisions,
		Summaries:     allSummaries,
	}, nil
}

// applyStrategy dispatches one strategy over the working state, mutating the
// dropped/truncated sets and entry contents in place (and, for summarize,
// splicing a synthetic entry into st.entries), and returns the audit detail. A
// non-nil error is fatal (a MeasureFunc failure or a context cancellation) and
// aborts the whole pipeline.
func applyStrategy(ctx context.Context, stgy dag.CompactionStrategy, st *compactState, pol Policy, counter tokens.Counter, measure MeasureFunc) (strategyOutcome, error) {
	switch stgy.Strategy {
	case dag.SlidingWindow:
		n := 0
		if stgy.N != nil {
			n = *stgy.N
		}
		return strategyOutcome{actions: slidingWindow(st, n)}, nil
	case dag.DropLowestPriority:
		actions, err := dropLowestPriority(st, pol.Budget, measure)
		return strategyOutcome{actions: actions}, err
	case dag.TruncateOldest:
		floor := 0
		if stgy.MinTokens != nil {
			floor = *stgy.MinTokens
		}
		actions, err := truncateOldest(st, pol.Budget, floor, counter, measure)
		return strategyOutcome{actions: actions}, err
	case dag.SummarizeStrategy:
		return summarize(ctx, stgy, st, pol, counter, measure)
	}
	// Unreachable — Validate rejects unknown/reserved strategies — but be safe.
	return strategyOutcome{}, nil
}

// slidingWindow keeps only the last n non-pinned live entries in message order,
// dropping the earlier non-pinned entries. Pinned entries are always kept and
// do not count toward n. One-shot: it applies the window once; a still-over
// request is handled by the next strategy.
func slidingWindow(st *compactState, n int) []EntryAction {
	var live []int
	for i := range st.entries {
		if st.entries[i].Pinned || st.dropped[st.entries[i].SourceIndex] {
			continue
		}
		live = append(live, i)
	}
	if len(live) <= n {
		return nil
	}
	var actions []EntryAction
	for _, i := range live[:len(live)-n] {
		st.dropped[st.entries[i].SourceIndex] = true
		actions = append(actions, EntryAction{
			SourceIndex: st.entries[i].SourceIndex, Name: st.entries[i].Name,
			Action: "dropped", TokensBefore: st.entries[i].Tokens, TokensAfter: 0,
		})
	}
	return actions
}

// dropLowestPriority evicts one non-pinned entry at a time — lowest priority
// first, ties broken by later message position — re-measuring after each, until
// the request fits or no droppable entry remains.
func dropLowestPriority(st *compactState, budget int, measure MeasureFunc) ([]EntryAction, error) {
	var actions []EntryAction
	for {
		cur, err := measure(renderLive(st.entries, st.dropped))
		if err != nil {
			return nil, err
		}
		if cur <= budget {
			break
		}
		victim := -1
		for i := range st.entries {
			e := st.entries[i]
			if e.Pinned || st.dropped[e.SourceIndex] {
				continue
			}
			if victim == -1 {
				victim = i
				continue
			}
			v := st.entries[victim]
			// Lower priority drops first; on a tie the later (higher-position)
			// entry drops first.
			if e.Priority < v.Priority || (e.Priority == v.Priority && i > victim) {
				victim = i
			}
		}
		if victim == -1 {
			break
		}
		st.dropped[st.entries[victim].SourceIndex] = true
		actions = append(actions, EntryAction{
			SourceIndex: st.entries[victim].SourceIndex, Name: st.entries[victim].Name,
			Action: "dropped", TokensBefore: st.entries[victim].Tokens, TokensAfter: 0,
		})
	}
	return actions, nil
}

// truncateOldest middle-out-truncates the oldest (lowest-position) non-pinned
// live entry above the floor, re-measuring after each, moving to the
// next-oldest while still over budget, until the request fits or every
// non-pinned entry is at its floor.
func truncateOldest(st *compactState, budget, floor int, counter tokens.Counter, measure MeasureFunc) ([]EntryAction, error) {
	actionByIdx := make(map[int]*EntryAction)
	var order []int
	for {
		cur, err := measure(renderLive(st.entries, st.dropped))
		if err != nil {
			return nil, err
		}
		if cur <= budget {
			break
		}
		victim := -1
		for i := range st.entries {
			e := st.entries[i]
			if e.Pinned || st.dropped[e.SourceIndex] || e.Tokens <= floor {
				continue
			}
			if victim == -1 || i < victim {
				victim = i
			}
		}
		if victim == -1 {
			break
		}
		e := &st.entries[victim]
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
		st.truncated[e.SourceIndex] = true
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

// summarize replaces the oldest non-pinned live span with a cheap-model summary
// (ticket 12.5): it gathers oldest-first non-pinned entries until removing them
// (for a summary of at most max_tokens) should bring the request under budget,
// calls the summarizer once, writes the summary to the blackboard, drops the
// span, and splices a synthetic summary entry in at the span's oldest position.
// A summarizer/board failure (or a summary no smaller than its span) is a
// fallback: the state is left unchanged and the reason is recorded on the
// revision (the warning event). A caller-context cancellation is fatal.
func summarize(ctx context.Context, stgy dag.CompactionStrategy, st *compactState, pol Policy, counter tokens.Counter, measure MeasureFunc) (strategyOutcome, error) {
	if pol.Summarizer == nil || pol.Board == nil {
		return strategyOutcome{errMsg: "summarizer or blackboard not configured"}, nil
	}
	maxTokens := stgy.EffectiveSummaryMaxTokens()

	cur, err := measure(renderLive(st.entries, st.dropped))
	if err != nil {
		return strategyOutcome{}, err
	}
	over := cur - pol.Budget

	// Gather the oldest non-pinned live entries until removing them (replaced by
	// a summary of at most maxTokens) should clear the overage — i.e. their
	// token sum reaches over+maxTokens — or the live non-pinned set is
	// exhausted. At least one entry is required.
	var spanPos []int
	spanTokens := 0
	for i := range st.entries {
		e := st.entries[i]
		if e.Pinned || st.dropped[e.SourceIndex] {
			continue
		}
		spanPos = append(spanPos, i)
		spanTokens += e.Tokens
		if spanTokens >= over+maxTokens {
			break
		}
	}
	if len(spanPos) == 0 {
		return strategyOutcome{errMsg: "no non-pinned entries to summarize"}, nil
	}

	spanEntries := make([]Entry, len(spanPos))
	spanNames := make([]string, len(spanPos))
	for k, p := range spanPos {
		spanEntries[k] = st.entries[p]
		spanNames[k] = st.entries[p].Name
	}
	spanText := Render(spanEntries)

	result, serr := pol.Summarizer.Summarize(ctx, SummarizeRequest{
		Model: stgy.Model, SpanText: spanText,
		MaxTokens: maxTokens, Timeout: stgy.EffectiveSummaryTimeout(),
	})
	if serr != nil {
		if ctx.Err() != nil {
			return strategyOutcome{}, ctx.Err() // fatal: caller cancelled/timed out
		}
		return strategyOutcome{errMsg: "summarizer failed: " + serr.Error()}, nil
	}
	summaryTokens := counter.Count(result.Text)
	if summaryTokens >= spanTokens {
		// No progress — a summary no smaller than its span cannot help. Fall
		// back rather than replace one block with a bigger one.
		return strategyOutcome{errMsg: fmt.Sprintf("summary (%d tokens) not smaller than span (%d tokens)", summaryTokens, spanTokens)}, nil
	}

	key := stgy.EffectiveSummaryKey()
	parentVersion := 0
	if head, ok, gerr := pol.Board.Get(ctx, key); gerr == nil && ok {
		parentVersion = head.Version
	} else if gerr != nil && ctx.Err() != nil {
		return strategyOutcome{}, ctx.Err()
	}

	// The persisted value carries NO cache_hit: it is durable content a
	// downstream summarization re-renders (and thus a cache-key input), so a
	// per-invocation cache flag would break the chain's determinism across runs
	// (ticket 13.5). result.CacheHit rides SummaryAction (audit) + the ledger.
	value, verr := json.Marshal(SummaryValue{
		SchemaVersion: SummaryValueSchemaVersion, Text: result.Text,
		SpanNames: spanNames, SpanTokens: spanTokens, SummaryTokens: summaryTokens,
		Model: stgy.Model, Resource: result.Resource,
		ParentVersion: parentVersion,
	})
	if verr != nil {
		return strategyOutcome{errMsg: "marshaling summary value: " + verr.Error()}, nil
	}
	entry, perr := pol.Board.Put(ctx, blackboard.PutArgs{
		Key: key, Value: value, Tags: []string{summaryTag},
	})
	if perr != nil {
		if ctx.Err() != nil {
			return strategyOutcome{}, ctx.Err()
		}
		return strategyOutcome{errMsg: "writing summary to blackboard: " + perr.Error()}, nil
	}

	// Commit: drop the span, splice the summary entry in at the span's oldest
	// position (so it renders where the oldest span entry was).
	ref := fmt.Sprintf("blackboard key:%s version:%d", key, entry.Version)
	var actions []EntryAction
	for _, p := range spanPos {
		e := st.entries[p]
		st.dropped[e.SourceIndex] = true
		st.summarizedInto[e.SourceIndex] = ref
		actions = append(actions, EntryAction{
			SourceIndex: e.SourceIndex, Name: e.Name,
			Action: "summarized", TokensBefore: e.Tokens, TokensAfter: 0,
		})
	}
	syntheticIdx := st.nextSyntheticIdx
	st.nextSyntheticIdx--
	summaryEntry := Entry{
		SourceIndex: syntheticIdx, Kind: kindSummary,
		Name: key, Ref: ref, Content: result.Text, Tokens: summaryTokens,
	}
	insertAt := spanPos[0]
	st.entries = append(st.entries[:insertAt], append([]Entry{summaryEntry}, st.entries[insertAt:]...)...)

	summary := SummaryAction{
		Key: key, Version: entry.Version, ParentVersion: parentVersion,
		Model: stgy.Model, Resource: result.Resource,
		SpanNames: spanNames, SpanTokens: spanTokens, SummaryTokens: summaryTokens,
		CacheHit: result.CacheHit,
	}
	if result.Usage != nil {
		summary.InputTokens = result.Usage.InputTokens
		summary.OutputTokens = result.Usage.OutputTokens
	}
	return strategyOutcome{actions: actions, summaries: []SummaryAction{summary}}, nil
}

// summaryTag is the blackboard tag every summarize strategy writes its summary
// under, so a downstream step can select summaries by tag.
const summaryTag = "context_summary"

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
// reports: dropped sources become Dropped (or Summarized when folded into a
// summary), strategy-truncated sources become Truncated with their new token
// count, untouched sources keep their assembly disposition, and each synthetic
// summary entry is appended as a new Summarized-into "summary"-kind report.
func compactedReports(base []SourceReport, st *compactState) []SourceReport {
	reports := make([]SourceReport, len(base))
	copy(reports, base)
	byIdx := make(map[int]int, len(reports))
	for i := range reports {
		byIdx[reports[i].Index] = i
	}
	var synthetic []SourceReport
	for _, e := range st.entries {
		if e.SourceIndex < 0 { // synthetic summary entry
			status := Included
			reason := ""
			switch {
			case st.dropped[e.SourceIndex]:
				status, reason = Dropped, "dropped by compaction"
			case st.truncated[e.SourceIndex]:
				status, reason = Truncated, fmt.Sprintf("truncated by compaction to %d tokens", e.Tokens)
			}
			tokens := e.Tokens
			if status == Dropped {
				tokens = 0
			}
			synthetic = append(synthetic, SourceReport{
				Index: e.SourceIndex, Kind: e.Kind, Name: e.Name, Ref: e.Ref,
				Status: status, Reason: reason, Tokens: tokens,
			})
			continue
		}
		r := &reports[byIdx[e.SourceIndex]]
		switch {
		case st.summarizedInto[e.SourceIndex] != "":
			r.Status = Summarized
			r.Reason = "summarized into " + st.summarizedInto[e.SourceIndex]
			r.Tokens = 0
		case st.dropped[e.SourceIndex]:
			r.Status = Dropped
			r.Reason = "dropped by compaction"
			r.Tokens = 0
		case st.truncated[e.SourceIndex]:
			r.Status = Truncated
			r.Reason = fmt.Sprintf("truncated by compaction to %d tokens", e.Tokens)
			r.Tokens = e.Tokens
		default:
			r.Tokens = e.Tokens
		}
	}
	return append(reports, synthetic...)
}
