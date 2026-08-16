// Package contextmgr assembles a step's declarative `context` spec (ADR-014,
// ticket 12.3) into the text the engine prepends to an llm request before the
// call. It is a leaf: it imports only other leaves (internal/dag for the spec,
// internal/blackboard and internal/retrieval for the source backends,
// internal/tokens for counting, internal/llm indirectly via tokens) plus
// stdlib, so exec and the engine depend on it without a cycle.
//
// Assembly is deterministic given store state: the same blackboard/step-output
// state and retriever corpus produce a byte-identical preamble and identical
// token counts, so 12.3's golden tests and 12.4's before/after audits rest on
// a stable, recomputable number. Sources are assembled in declaration order,
// which is precedence and message order (ADR-014). Per-source token caps
// truncate deterministically; pinned sources are never truncated. A source
// that resolves to nothing is either a permanent failure (on_missing: error,
// the default) or an omission recorded in the audit event (on_missing: skip).
//
// The compaction strategies that shrink an over-budget assembly (12.4) and the
// provider-window guardrail (12.6) build on this package's counted output;
// 12.3 only assembles and reports.
package contextmgr

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/mathcslearner/agentloom/internal/blackboard"
	"github.com/mathcslearner/agentloom/internal/dag"
	"github.com/mathcslearner/agentloom/internal/retrieval"
	"github.com/mathcslearner/agentloom/internal/tokens"
)

// defaultRetrievalTopK mirrors the retrieve executor's default so a context
// retrieval source and a `retrieve` step agree on an unset top_k.
const defaultRetrievalTopK = 5

// truncationMarker is appended to a source's text when a per-source token cap
// truncates it — a fixed string (no embedded count) so truncation stays a
// pure, deterministic function of the input and the cap.
const truncationMarker = "\n…[truncated]…"

// Disposition is how one source resolved.
type Disposition string

const (
	// Included is a source rendered whole into the assembly.
	Included Disposition = "included"
	// Truncated is a source rendered but truncated — either to its per-source
	// cap (12.3) or by the truncate_oldest compaction strategy (12.4).
	Truncated Disposition = "truncated"
	// Skipped is a source that resolved to nothing under an on_missing: skip.
	Skipped Disposition = "skipped"
	// Dropped is a source evicted whole by a compaction strategy (12.4).
	Dropped Disposition = "dropped"
)

// SourceReport is one source's disposition in an assembly — the audit record
// the engine writes to the context_assembled event.
type SourceReport struct {
	Index  int
	Kind   dag.ContextSourceKind
	Name   string
	Ref    string
	Status Disposition
	Reason string
	Tokens int
	Pinned bool
}

// Entry is one rendered (non-skipped) source in an assembly — the unit the
// compaction strategies (12.4) drop or truncate. A source maps to exactly one
// entry (a multi-head blackboard tag source or a multi-doc retrieval source
// renders as a single block containing several inner elements), so entry order
// is source declaration order (precedence and message order) and Render over
// the entries reproduces the assembled preamble byte-for-byte.
type Entry struct {
	// SourceIndex is the source's position in the declared spec (the report key).
	SourceIndex int
	// Kind, Name, Ref mirror the source's identity for rendering and the audit.
	Kind dag.ContextSourceKind
	Name string
	Ref  string
	// Pinned exempts the entry from every compaction strategy (never dropped or
	// truncated).
	Pinned bool
	// Priority orders drop_lowest_priority (lower drops first); 0 by default.
	Priority int
	// Content is the rendered inner text (after any per-source cap truncation).
	Content string
	// Tokens is Content's count under the assembly counter.
	Tokens int
	// truncatedByCap records that the per-source cap (12.3) already truncated
	// this entry at assembly time, so its report stays Truncated even if no
	// compaction touches it.
	truncatedByCap bool
}

// Assembly is the product of Assemble: the preamble text to prepend to the
// request, the per-source dispositions, the rendered entries (the compaction
// unit), and the counted context size.
type Assembly struct {
	// Preamble is the assembled context text, ready to prepend to the request.
	// Empty when every source skipped.
	Preamble string
	// Sources is the per-source disposition in declaration (precedence) order.
	Sources []SourceReport
	// Entries is the rendered (non-skipped) sources in precedence order — the
	// unit 12.4's compaction operates on. Render(Entries) == Preamble.
	Entries []Entry
	// ContextTokens is the counted size of Preamble under CounterID.
	ContextTokens int
	// CounterID is the fingerprint of the counter used throughout.
	CounterID string
}

// Sources supplies the store-backed data Assemble reads. The engine builds
// these closures/handles over its store, the step's blackboard, and the
// retriever registry; contextmgr never imports internal/store, so it stays a
// leaf.
type Sources struct {
	// StepOutput returns a succeeded upstream step's output. ok is false (with
	// a nil error) when the step has no recorded output (not succeeded) — a
	// missing source subject to the source's on_missing policy.
	StepOutput func(ctx context.Context, stepID string) (output json.RawMessage, ok bool, err error)
	// Board is the run/step-scoped blackboard for blackboard sources; nil means
	// no board is wired (a blackboard source is then a config error).
	Board blackboard.Board
	// Retriever resolves a retriever by name for retrieval sources; nil means
	// no registry is wired (a retrieval source is then a config error).
	Retriever func(name string) (retrieval.Retriever, error)
}

// MissingSourceError reports a source that resolved to nothing under the
// default (error) missing-policy. Deterministic (the same store state resolves
// the same way), so the engine records a permanent step failure before any
// provider call.
type MissingSourceError struct {
	Index int
	Kind  dag.ContextSourceKind
	Name  string
	Ref   string
	Msg   string
}

func (e *MissingSourceError) Error() string {
	return fmt.Sprintf("context source %d (%s %q): %s", e.Index, e.Kind, e.Name, e.Msg)
}

// ConfigError reports a source that cannot be assembled because the required
// backend is not wired (no blackboard, no retriever registry) or names an
// unknown retriever. Deterministic and permanent regardless of the source's
// missing-policy.
type ConfigError struct {
	Index int
	Kind  dag.ContextSourceKind
	Name  string
	Msg   string
}

func (e *ConfigError) Error() string {
	return fmt.Sprintf("context source %d (%s %q): %s", e.Index, e.Kind, e.Name, e.Msg)
}

// Assemble resolves each source in spec against src, applies per-source caps,
// and renders the assembled preamble, using counter for every token count.
// Sources are processed in declaration order (precedence and message order).
// A source that resolves to nothing is a *MissingSourceError (on_missing:
// error, the default) or a Skipped report (on_missing: skip); a source whose
// backend is unwired or unknown is a *ConfigError. Any other error (a store or
// retriever transport failure) is returned wrapped, for the engine to treat as
// transport and redeliver.
func Assemble(ctx context.Context, spec dag.ContextSpec, counter tokens.Counter, src Sources) (Assembly, error) {
	asm := Assembly{CounterID: counter.ID()}
	for i, s := range spec.Sources {
		name := sourceName(s, i)
		content, ref, missing, err := resolveSource(ctx, i, s, name, src)
		if err != nil {
			return Assembly{}, err
		}
		if missing {
			if s.EffectiveMissingPolicy() == dag.MissingError {
				return Assembly{}, &MissingSourceError{Index: i, Kind: s.Kind, Name: name, Ref: ref, Msg: "resolved to nothing"}
			}
			asm.Sources = append(asm.Sources, SourceReport{
				Index: i, Kind: s.Kind, Name: name, Ref: ref,
				Status: Skipped, Reason: "resolved to nothing", Pinned: s.Pinned,
			})
			continue
		}
		status := Included
		reason := ""
		truncatedByCap := false
		if s.MaxTokens != nil && !s.Pinned {
			kept, removed, cut := truncateToTokens(content, *s.MaxTokens, counter)
			if cut {
				content = kept
				status = Truncated
				truncatedByCap = true
				reason = fmt.Sprintf("truncated %d tokens to cap %d", removed, *s.MaxTokens)
			}
		}
		asm.Entries = append(asm.Entries, Entry{
			SourceIndex: i, Kind: s.Kind, Name: name, Ref: ref,
			Pinned: s.Pinned, Priority: s.EffectivePriority(),
			Content: content, Tokens: counter.Count(content), truncatedByCap: truncatedByCap,
		})
		asm.Sources = append(asm.Sources, SourceReport{
			Index: i, Kind: s.Kind, Name: name, Ref: ref,
			Status: status, Reason: reason, Tokens: counter.Count(content), Pinned: s.Pinned,
		})
	}
	asm.Preamble = Render(asm.Entries)
	asm.ContextTokens = counter.Count(asm.Preamble)
	return asm, nil
}

// Render assembles the entries' rendered blocks into the preamble text, in the
// order given (declaration/precedence order), joined by blank lines — the exact
// byte layout Assemble produces. Compaction (12.4) re-renders the surviving
// entries with this same function, so a compacted preamble differs from the
// original only in the dropped/truncated entries.
func Render(entries []Entry) string {
	if len(entries) == 0 {
		return ""
	}
	blocks := make([]string, len(entries))
	for i, e := range entries {
		blocks[i] = renderBlock(e.Kind, e.Name, e.Content)
	}
	return strings.Join(blocks, "\n\n")
}

// resolveSource resolves one source to its rendered content and a
// human-readable ref. missing is true when the source resolved to nothing
// (the caller applies the on_missing policy). A returned error is a
// *ConfigError (permanent) or a transport error (redeliver).
func resolveSource(ctx context.Context, i int, s dag.ContextSource, name string, src Sources) (content, ref string, missing bool, err error) {
	switch s.Kind {
	case dag.SourceLiteral:
		return s.Text, "literal", false, nil

	case dag.SourceStepOutput:
		ref = "step:" + s.Step
		if src.StepOutput == nil {
			return "", ref, false, &ConfigError{Index: i, Kind: s.Kind, Name: name, Msg: "no step-output reader wired"}
		}
		out, ok, oerr := src.StepOutput(ctx, s.Step)
		if oerr != nil {
			return "", ref, false, fmt.Errorf("reading step %q output: %w", s.Step, oerr)
		}
		if !ok {
			return "", ref, true, nil
		}
		val, perr := blackboard.ResolvePointer(out, s.Path)
		if perr != nil {
			// A pointer that does not resolve against a present output is a
			// missing value (ADR-014), subject to the source's on_missing.
			return "", ref + pointerRef(s.Path), true, nil
		}
		return renderValue(val), ref + pointerRef(s.Path), false, nil

	case dag.SourceBlackboard:
		if src.Board == nil {
			return "", blackboardRef(s), false, &ConfigError{Index: i, Kind: s.Kind, Name: name, Msg: "no blackboard wired"}
		}
		return resolveBlackboard(ctx, s, src.Board)

	case dag.SourceRetrieval:
		ref = fmt.Sprintf("retriever:%s query:%q", s.Retriever, s.Query)
		if src.Retriever == nil {
			return "", ref, false, &ConfigError{Index: i, Kind: s.Kind, Name: name, Msg: "no retriever registry wired"}
		}
		r, rerr := src.Retriever(s.Retriever)
		if rerr != nil {
			// An unknown retriever is a permanent config error (the retriever
			// name is static); any other resolve error is transport.
			var unknown *retrieval.UnknownRetrieverError
			if errors.As(rerr, &unknown) {
				return "", ref, false, &ConfigError{Index: i, Kind: s.Kind, Name: name, Msg: rerr.Error()}
			}
			return "", ref, false, fmt.Errorf("resolving retriever %q: %w", s.Retriever, rerr)
		}
		topK := defaultRetrievalTopK
		if s.TopK != nil {
			topK = *s.TopK
		}
		docs, qerr := r.Query(ctx, s.Query, topK)
		if qerr != nil {
			return "", ref, false, fmt.Errorf("querying retriever %q: %w", s.Retriever, qerr)
		}
		if len(docs) == 0 {
			return "", ref, true, nil
		}
		return renderDocs(docs), ref, false, nil
	}
	// Unknown kind is unreachable — Validate rejects it — but be explicit.
	return "", "", false, &ConfigError{Index: i, Kind: s.Kind, Name: name, Msg: "unknown source kind"}
}

// resolveBlackboard resolves a blackboard source: a single head by key, or
// every head carrying all listed tags (AND), rendered in key order.
func resolveBlackboard(ctx context.Context, s dag.ContextSource, board blackboard.Board) (content, ref string, missing bool, err error) {
	ref = blackboardRef(s)
	if s.Key != "" {
		entry, ok, gerr := board.Get(ctx, s.Key)
		if gerr != nil {
			return "", ref, false, fmt.Errorf("reading blackboard key %q: %w", s.Key, gerr)
		}
		if !ok {
			return "", ref, true, nil
		}
		return renderValue(entry.Value), ref, false, nil
	}
	entries, lerr := board.List(ctx, blackboard.ListFilter{Tags: s.Tags})
	if lerr != nil {
		return "", ref, false, fmt.Errorf("listing blackboard by tags %v: %w", s.Tags, lerr)
	}
	if len(entries) == 0 {
		return "", ref, true, nil
	}
	// List returns heads ordered by key (deterministic).
	parts := make([]string, 0, len(entries))
	for _, e := range entries {
		parts = append(parts, fmt.Sprintf("<entry key=%q version=%d>\n%s\n</entry>", e.Key, e.Version, renderValue(e.Value)))
	}
	return strings.Join(parts, "\n"), ref, false, nil
}

// renderValue turns a stored JSON value into text: a JSON string becomes its
// unquoted content (the common case — an llm step's `/text`); any other value
// becomes its canonical compact JSON. Deterministic.
func renderValue(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	return string(blackboard.CanonicalValue(raw))
}

// renderDocs renders retrieval results in ranked order, each wrapped with its
// id so a downstream model can cite it.
func renderDocs(docs []retrieval.ScoredDoc) string {
	parts := make([]string, 0, len(docs))
	for _, d := range docs {
		parts = append(parts, fmt.Sprintf("<doc id=%q>\n%s\n</doc>", d.ID, d.Content))
	}
	return strings.Join(parts, "\n")
}

// renderBlock wraps a source's content in a stable, labeled envelope the model
// and the audit trail both see.
func renderBlock(kind dag.ContextSourceKind, name, content string) string {
	return fmt.Sprintf("<context name=%q kind=%q>\n%s\n</context>", name, string(kind), content)
}

// sourceName resolves a source's label: the authored name, else "<kind>#<i>".
func sourceName(s dag.ContextSource, i int) string {
	if s.Name != "" {
		return s.Name
	}
	return fmt.Sprintf("%s#%d", s.Kind, i)
}

func pointerRef(path string) string {
	if path == "" {
		return ""
	}
	return " " + path
}

func blackboardRef(s dag.ContextSource) string {
	if s.Key != "" {
		return "key:" + s.Key
	}
	tags := append([]string(nil), s.Tags...)
	sort.Strings(tags)
	return "tags:" + strings.Join(tags, ",")
}

// truncateToTokens keeps the largest rune-boundary prefix of text whose count
// (plus the truncation marker, when it fits) is at most cap, appending the
// marker. It returns the kept string, the number of tokens removed, and
// whether truncation occurred. Pure and deterministic: a binary search over
// rune boundaries with a fixed marker.
func truncateToTokens(text string, capTokens int, counter tokens.Counter) (kept string, removed int, truncated bool) {
	total := counter.Count(text)
	if total <= capTokens {
		return text, 0, false
	}
	runes := []rune(text)
	// Prefer to append the marker; fall back to a bare prefix only if the
	// marker alone would already exceed the cap. Because BPE counts are not
	// additive (Count(a)+Count(b) can differ from Count(a+b)), the search
	// measures the FINAL string (prefix, or prefix+marker) against the cap, so
	// the returned string is guaranteed <= cap tokens.
	withMarker := counter.Count(truncationMarker) <= capTokens
	measure := func(prefix string) int {
		if withMarker {
			return counter.Count(prefix + truncationMarker)
		}
		return counter.Count(prefix)
	}
	// Largest n in [0, len(runes)] with measure(string(runes[:n])) <= cap;
	// measure is monotonic non-decreasing in n, so binary search is valid.
	lo, hi := 0, len(runes)
	best := 0
	for lo <= hi {
		mid := (lo + hi) / 2
		if measure(string(runes[:mid])) <= capTokens {
			best = mid
			lo = mid + 1
		} else {
			hi = mid - 1
		}
	}
	prefix := string(runes[:best])
	removed = total - counter.Count(prefix)
	if withMarker {
		return prefix + truncationMarker, removed, true
	}
	return prefix, removed, true
}
