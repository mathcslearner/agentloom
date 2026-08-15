// Package jsonrepair applies cheap, deterministic fixes to an LLM's text
// output before the engine's validate stage declares it invalid (ticket
// 11.3, ADR-013). Models routinely wrap JSON in Markdown code fences, add a
// trailing comma, quote nothing, or bracket the JSON with a sentence of
// prose — none of which a JSON parser tolerates, yet all of which are
// mechanically recoverable without another model call.
//
// The package is a stdlib-only leaf (it imports nothing from agentloom): the
// llm executor calls Repair on a completion's text, and the engine never sees
// it directly. Repairs are conservative and cumulative — each pass is applied
// in turn, string contents are never rewritten (only structural punctuation
// outside strings), and a value that parses at any point short-circuits the
// remaining passes. A value that still does not parse after every pass is
// reported unrepairable, never guessed at: the semantic-retry loop (11.4) is
// the correct next move for an output no deterministic fix reaches.
package jsonrepair

import (
	"bytes"
	"encoding/json"
	"strings"
)

// Status is a repair outcome, recorded on the attempt as output provenance
// (ADR-013).
type Status string

const (
	// StatusRaw means the input already parsed as JSON with no repair (the
	// common, happy case — a well-behaved structured response).
	StatusRaw Status = "raw"
	// StatusRepaired means one or more deterministic passes turned an
	// unparseable input into valid JSON.
	StatusRepaired Status = "repaired"
	// StatusUnrepairable means no pass produced valid JSON — the output is
	// left to the semantic-retry loop.
	StatusUnrepairable Status = "unrepairable"
)

// The repair step names recorded in Result.Steps, in the order they are
// attempted. Only steps that actually changed the working text are recorded,
// so an empty Steps slice on a StatusRaw result means "already valid".
const (
	StepStripCodeFence   = "strip_code_fence"
	StepExtractFirstJSON = "extract_first_json"
	StepTrailingCommas   = "trailing_commas"
	StepUnquotedKeys     = "unquoted_keys"
)

// Result is the outcome of a repair attempt.
type Result struct {
	// Value is the repaired, compacted JSON on StatusRaw/StatusRepaired, and
	// nil on StatusUnrepairable. It is always valid JSON when non-nil.
	Value json.RawMessage
	// Status is the repair outcome.
	Status Status
	// Steps names the passes that changed the text, in application order.
	// Empty on StatusRaw; the attempted passes on StatusUnrepairable.
	Steps []string
}

// Repair attempts to turn text into valid JSON through a fixed sequence of
// conservative, cumulative passes. It compacts the result (stable
// whitespace, numbers preserved byte-for-byte) so the returned value is
// canonical. The input's string contents are never altered — only structural
// punctuation outside string literals.
func Repair(text string) Result {
	// Fast path: already valid JSON. Compact it so the value is canonical
	// like the repaired path, but record no steps — nothing was repaired.
	if compact, ok := compactValid(text); ok {
		return Result{Value: compact, Status: StatusRaw}
	}

	work := text
	var steps []string
	// Each pass is applied in turn to the cumulative working text; after any
	// pass that changes the text we re-check validity and short-circuit. This
	// keeps the repair minimal — the first pass that makes the value parse
	// wins, and no later pass touches an already-valid value.
	for _, pass := range []struct {
		name string
		fn   func(string) string
	}{
		{StepStripCodeFence, stripCodeFence},
		{StepExtractFirstJSON, extractFirstJSON},
		{StepTrailingCommas, removeTrailingCommas},
		{StepUnquotedKeys, quoteBareKeys},
	} {
		next := pass.fn(work)
		if next == work {
			continue // pass made no change — do not record it
		}
		work = next
		steps = append(steps, pass.name)
		if compact, ok := compactValid(work); ok {
			return Result{Value: compact, Status: StatusRepaired, Steps: steps}
		}
	}

	// Nothing produced valid JSON. Report the passes we attempted (those that
	// changed the text) so the provenance shows what was tried.
	return Result{Status: StatusUnrepairable, Steps: steps}
}

// compactValid reports whether s (trimmed) is valid JSON and, if so, returns
// its compacted form. A copy is returned so the caller never aliases the
// input.
func compactValid(s string) (json.RawMessage, bool) {
	trimmed := strings.TrimSpace(s)
	if trimmed == "" || !json.Valid([]byte(trimmed)) {
		return nil, false
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, []byte(trimmed)); err != nil {
		return nil, false
	}
	return json.RawMessage(buf.Bytes()), true
}

// stripCodeFence removes a single Markdown code fence wrapping the text —
// ```json ... ``` or ``` ... ``` — the single most common way a model
// packages a JSON answer. It only fires when the trimmed text opens with a
// fence; a stray backtick inside otherwise-valid JSON is left alone.
func stripCodeFence(text string) string {
	t := strings.TrimSpace(text)
	if !strings.HasPrefix(t, "```") {
		return text
	}
	// Drop the opening fence line (```lang or ```) up to and including its
	// newline; a fence with no newline is not a real block, so leave it.
	nl := strings.IndexByte(t, '\n')
	if nl < 0 {
		return text
	}
	body := t[nl+1:]
	// Drop the closing fence: the last ``` and anything after it.
	if end := strings.LastIndex(body, "```"); end >= 0 {
		body = body[:end]
	}
	return strings.TrimSpace(body)
}

// extractFirstJSON returns the first balanced JSON object or array embedded
// in text — the fix for a model that brackets its JSON with a sentence
// ("Here is the result: { ... }. Hope that helps!"). It scans for the first
// '{' or '[', then walks to the matching close, honoring string literals and
// escapes so a brace inside a string never miscounts. If no balanced span is
// found the text is returned unchanged.
func extractFirstJSON(text string) string {
	start := -1
	var openCh, closeCh byte
	for i := 0; i < len(text); i++ {
		if text[i] == '{' || text[i] == '[' {
			start = i
			openCh = text[i]
			if openCh == '{' {
				closeCh = '}'
			} else {
				closeCh = ']'
			}
			break
		}
	}
	if start < 0 {
		return text
	}
	depth := 0
	inString := false
	escaped := false
	for i := start; i < len(text); i++ {
		c := text[i]
		if inString {
			switch {
			case escaped:
				escaped = false
			case c == '\\':
				escaped = true
			case c == '"':
				inString = false
			}
			continue
		}
		switch c {
		case '"':
			inString = true
		case openCh:
			depth++
		case closeCh:
			depth--
			if depth == 0 {
				return text[start : i+1]
			}
		}
	}
	return text // unbalanced — leave it for the unrepairable verdict
}

// removeTrailingCommas deletes a comma that is immediately followed (ignoring
// whitespace) by a closing '}' or ']' — the trailing-comma mistake JSON
// forbids but almost every other language tolerates. Commas inside string
// literals are skipped by tracking string state.
func removeTrailingCommas(text string) string {
	var b strings.Builder
	b.Grow(len(text))
	inString := false
	escaped := false
	for i := 0; i < len(text); i++ {
		c := text[i]
		if inString {
			b.WriteByte(c)
			switch {
			case escaped:
				escaped = false
			case c == '\\':
				escaped = true
			case c == '"':
				inString = false
			}
			continue
		}
		if c == '"' {
			inString = true
			b.WriteByte(c)
			continue
		}
		if c == ',' {
			// Look ahead past whitespace for a closing bracket.
			j := i + 1
			for j < len(text) && isJSONSpace(text[j]) {
				j++
			}
			if j < len(text) && (text[j] == '}' || text[j] == ']') {
				continue // drop the comma
			}
		}
		b.WriteByte(c)
	}
	return b.String()
}

// quoteBareKeys wraps an unquoted object key in double quotes — the
// JavaScript-object-literal habit ({key: 1} instead of {"key": 1}). It only
// quotes an identifier that sits in key position: immediately after a '{' or
// ',' (ignoring whitespace) and immediately before a ':'. Anything else —
// values, keys already quoted, text inside strings — is left untouched.
func quoteBareKeys(text string) string {
	var b strings.Builder
	b.Grow(len(text) + 8)
	inString := false
	escaped := false
	// afterOpener is true when the last significant (non-space) byte was a
	// '{' or ',', i.e. the parser is at a position where an object key may
	// begin.
	afterOpener := false
	for i := 0; i < len(text); i++ {
		c := text[i]
		if inString {
			b.WriteByte(c)
			switch {
			case escaped:
				escaped = false
			case c == '\\':
				escaped = true
			case c == '"':
				inString = false
			}
			continue
		}
		switch {
		case c == '"':
			inString = true
			afterOpener = false
			b.WriteByte(c)
		case c == '{' || c == ',':
			afterOpener = true
			b.WriteByte(c)
		case isJSONSpace(c):
			b.WriteByte(c) // whitespace does not clear afterOpener
		case afterOpener && isIdentStart(c):
			// A bare identifier in key position: consume it and, if it is
			// immediately followed (past whitespace) by ':', quote it.
			j := i
			for j < len(text) && isIdentPart(text[j]) {
				j++
			}
			ident := text[i:j]
			k := j
			for k < len(text) && isJSONSpace(text[k]) {
				k++
			}
			if k < len(text) && text[k] == ':' {
				b.WriteByte('"')
				b.WriteString(ident)
				b.WriteByte('"')
			} else {
				b.WriteString(ident) // not a key — leave as-is
			}
			i = j - 1
			afterOpener = false
		default:
			afterOpener = false
			b.WriteByte(c)
		}
	}
	return b.String()
}

// isJSONSpace reports whether c is JSON insignificant whitespace.
func isJSONSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r'
}

// isIdentStart reports whether c may begin a bare (unquoted) object key.
func isIdentStart(c byte) bool {
	return c == '_' || c == '$' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

// isIdentPart reports whether c may continue a bare object key.
func isIdentPart(c byte) bool {
	return isIdentStart(c) || (c >= '0' && c <= '9')
}
