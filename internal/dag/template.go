package dag

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"text/template"
	"unicode/utf8"
)

// This file is the ticket 8.2 seam: step input templating and data flow.
// String values inside a step's config may carry `${{ ... }}` expressions
// referencing upstream step outputs (`steps.<id>.output.<path>`) and run
// parameters (`run.params.<key>`). ParseConfigTemplates compiles them —
// at definition-validation time for the static lint (referenced step
// exists and is upstream, referenced param is declared), and again in the
// worker just before execution — and Render resolves them against the
// run's recorded state, producing the config the executor decodes.
//
// The engine of record is Go text/template with `${{`/`}}` delimiters and
// a restricted FuncMap (get, default, toJson, truncate — no arbitrary
// code). Two deliberate hardenings on top of it:
//
//   - Expressions only. Control structures (if/range/with/...), variable
//     declarations, and nested template invocations are rejected at parse
//     time; a template action is a single pipeline.
//   - Rendering happens exactly once, on the authored definition. Values
//     flowing in from upstream outputs or params are data: a `${{ ... }}`
//     inside an upstream output is never re-parsed, so injection attempts
//     through step outputs are inert.
//
// Reference semantics: bare references are strict — a missing step,
// output, path element, or param is a typed *MissingRefError, never an
// empty string (ADR-006 classifies the resulting step failure permanent).
// The sanctioned opt-out is the lenient `get` function, which resolves a
// quoted path to nil when absent so `default` can substitute:
// `${{ get "steps.a.output.x" | default "fallback" }}`.
//
// Type preservation: a string that is exactly one `${{ ... }}` expression
// (whitespace aside) is replaced by the resolved value itself — objects,
// arrays, numbers, and booleans survive as JSON, which is how structured
// data flows between steps. A string mixing literals and expressions
// renders to a string: scalars interpolate naturally (numbers verbatim,
// bare strings, true/false, nil as ""), composites as compact JSON.
const (
	tmplLeftDelim  = "${{"
	tmplRightDelim = "}}"
)

// HasTemplate reports whether s contains a template expression opener.
// It is the cheap gate both the validator and the engine use before any
// parsing work — and the marker per-field config validation (e.g. sleep's
// duration parseability) uses to defer literal checks to runtime.
func HasTemplate(s string) bool { return strings.Contains(s, tmplLeftDelim) }

// TemplateRef is one reference extracted from a config's template
// expressions — the unit the static lint checks and the engine's
// output-fetch planning consumes.
type TemplateRef struct {
	// ConfigPath is the JSON path of the templated string within the
	// config (e.g. "input.greeting", "messages[0].content").
	ConfigPath string

	// Raw is the reference as authored, e.g. "steps.a.output.text".
	Raw string

	// StepID is the referenced step for steps.* references, "" otherwise.
	StepID string

	// ParamKey is the referenced parameter key for run.params.*
	// references, "" otherwise (also "" for a whole-map `run.params`
	// reference).
	ParamKey string

	// Lenient marks a reference recovered from a quoted string literal
	// inside an expression (a `get` path). Lenient references resolve to
	// nil when absent by design, so the lint skips them; they still count
	// toward the set of upstream outputs the engine fetches.
	Lenient bool
}

// TemplateError is one template that cannot be parsed or executed,
// qualified by the JSON path of the templated string within the config.
type TemplateError struct {
	Path string
	Msg  string
}

func (e *TemplateError) Error() string {
	if e.Path == "" {
		return e.Msg
	}
	return e.Path + ": " + e.Msg
}

// TemplateRefError is one malformed reference inside a template
// expression — a `steps.` or `run.` token that does not follow the
// reference grammar (steps.<id>.output[.<path>] / run.params[.<key>]).
type TemplateRefError struct {
	Path string // JSON path of the templated string within the config
	Ref  string // the offending token as authored
	Msg  string
}

func (e *TemplateRefError) Error() string {
	return fmt.Sprintf("%s: invalid reference %q: %s", e.Path, e.Ref, e.Msg)
}

// MissingRefError is a strict reference that did not resolve at render
// time: the referenced step has no output available, or the path names a
// key or index that does not exist. Per ADR-006 the engine records the
// resulting step failure as permanent — the run's recorded state is
// immutable, so retrying cannot help.
type MissingRefError struct {
	Ref    string
	Detail string
}

func (e *MissingRefError) Error() string {
	return fmt.Sprintf("template reference %q did not resolve: %s", e.Ref, e.Detail)
}

// RenderData is the run state a Render call resolves references against.
type RenderData struct {
	// Outputs maps upstream step IDs to their recorded output JSON. The
	// engine populates exactly the referenced steps that have a recorded
	// output (i.e. succeeded); a strict reference to an absent entry is a
	// *MissingRefError.
	Outputs map[string]json.RawMessage

	// Params is the run's submitted parameters as a JSON object; nil or
	// empty means no parameters were submitted.
	Params json.RawMessage
}

// ConfigTemplate is one step config's parsed template set, ready to lint
// (Refs) or render. Not safe for concurrent use: the engine parses one
// per execution.
type ConfigTemplate struct {
	raw       json.RawMessage
	tree      any
	templates []*strTemplate
	refs      []TemplateRef
	state     *renderState
}

// strTemplate is one templated string value within the config.
type strTemplate struct {
	path  string // config-relative JSON path, for error messages
	src   string // the authored string
	tmpl  *template.Template
	whole bool // single expression, whitespace-only literals → type-preserving
}

// renderState is the shared closure target of the template FuncMap: the
// resolution root for the current Render call, the value captured by a
// whole-expression pipeline, and the first typed reference error (kept
// aside because text/template wraps function errors in its own types).
type renderState struct {
	root     map[string]any
	captured any
	refErr   error
}

// ParseConfigTemplates walks a step config's JSON and compiles every
// string value carrying a `${{ ... }}` expression. It reports every
// problem it can find in one pass: the returned error joins
// *TemplateError and *TemplateRefError values, each qualified by the
// config-relative JSON path (errors.As-reachable through errors.Join).
// A config with no template expressions parses to a ConfigTemplate whose
// HasTemplates is false and whose Render returns the config verbatim.
func ParseConfigTemplates(config json.RawMessage) (*ConfigTemplate, error) {
	ct := &ConfigTemplate{raw: config, state: &renderState{}}
	if len(config) == 0 || !HasTemplate(string(config)) {
		return ct, nil
	}
	dec := json.NewDecoder(bytes.NewReader(config))
	dec.UseNumber()
	var tree any
	if err := dec.Decode(&tree); err != nil {
		return nil, &TemplateError{Msg: fmt.Sprintf("config is not valid JSON: %v", err)}
	}
	p := &tmplParser{ct: ct}
	ct.tree = p.walk(tree, "")
	if len(p.errs) > 0 {
		return nil, errors.Join(p.errs...)
	}
	return ct, nil
}

// HasTemplates reports whether the config carries any template
// expressions (false means Render is a verbatim pass-through).
func (ct *ConfigTemplate) HasTemplates() bool { return len(ct.templates) > 0 }

// Refs returns every reference extracted from the config's expressions,
// in config-walk order (map keys sorted, so the order is deterministic).
func (ct *ConfigTemplate) Refs() []TemplateRef { return ct.refs }

// StepIDs returns the sorted, de-duplicated set of step IDs the config
// references — strict and lenient alike — which is exactly the set of
// upstream outputs the engine must fetch before rendering.
func (ct *ConfigTemplate) StepIDs() []string {
	seen := make(map[string]bool)
	var ids []string
	for _, r := range ct.refs {
		if r.StepID != "" && !seen[r.StepID] {
			seen[r.StepID] = true
			ids = append(ids, r.StepID)
		}
	}
	sort.Strings(ids)
	return ids
}

// Render resolves every template expression against data and returns the
// rendered config JSON. Configs without templates return the original
// bytes unchanged; rendered configs are a canonical re-encoding (map keys
// sorted, no HTML escaping). Failures return a *MissingRefError (strict
// reference did not resolve) or *TemplateError (execution failure),
// errors.As-reachable.
func (ct *ConfigTemplate) Render(data RenderData) (json.RawMessage, error) {
	if !ct.HasTemplates() {
		return ct.raw, nil
	}
	steps := make(map[string]any, len(data.Outputs))
	for id, out := range data.Outputs {
		v, err := decodeJSONValue(out)
		if err != nil {
			return nil, &TemplateError{Msg: fmt.Sprintf("output of step %q is not valid JSON: %v", id, err)}
		}
		steps[id] = map[string]any{"output": v}
	}
	params, err := decodeJSONValue(data.Params)
	if err != nil {
		return nil, &TemplateError{Msg: fmt.Sprintf("run params are not valid JSON: %v", err)}
	}
	ct.state.root = map[string]any{
		"steps": steps,
		"run":   map[string]any{"params": params},
	}
	rendered, err := ct.renderNode(ct.tree)
	if err != nil {
		return nil, err
	}
	return marshalNoEscape(rendered)
}

// decodeJSONValue decodes raw JSON preserving number fidelity
// (json.Number, so a large integer round-trips unmangled); nil or empty
// input decodes to nil.
func decodeJSONValue(raw json.RawMessage) (any, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	return v, nil
}

// renderNode renders one node of the parsed config tree: templated
// strings execute, containers recurse, everything else passes through.
func (ct *ConfigTemplate) renderNode(node any) (any, error) {
	switch n := node.(type) {
	case map[string]any:
		out := make(map[string]any, len(n))
		for k, v := range n {
			rv, err := ct.renderNode(v)
			if err != nil {
				return nil, err
			}
			out[k] = rv
		}
		return out, nil
	case []any:
		out := make([]any, len(n))
		for i, v := range n {
			rv, err := ct.renderNode(v)
			if err != nil {
				return nil, err
			}
			out[i] = rv
		}
		return out, nil
	case *strTemplate:
		return ct.execute(n)
	default:
		return node, nil
	}
}

// execute runs one templated string. Whole-expression templates return
// the captured resolved value (type-preserving); mixed templates return
// the interpolated string.
func (ct *ConfigTemplate) execute(st *strTemplate) (any, error) {
	ct.state.captured = nil
	ct.state.refErr = nil
	var buf strings.Builder
	var out io.Writer = &buf
	if st.whole {
		out = io.Discard
	}
	if err := st.tmpl.Execute(out, nil); err != nil {
		// text/template wraps function errors in its own types; the typed
		// reference error was kept aside so callers can errors.As it.
		if ct.state.refErr != nil {
			return nil, ct.state.refErr
		}
		return nil, &TemplateError{Path: st.path, Msg: fmt.Sprintf("executing template %q: %v", st.src, err)}
	}
	if st.whole {
		return ct.state.captured, nil
	}
	return buf.String(), nil
}

// tmplParser accumulates parsed templates and problems across one config
// walk.
type tmplParser struct {
	ct   *ConfigTemplate
	errs []error
}

// walk descends the decoded config tree, replacing each templated string
// with its parsed *strTemplate node. Map keys walk in sorted order so
// extracted refs and reported problems are deterministic.
func (p *tmplParser) walk(node any, path string) any {
	switch n := node.(type) {
	case map[string]any:
		keys := make([]string, 0, len(n))
		for k := range n {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			n[k] = p.walk(n[k], joinPath(path, k))
		}
		return n
	case []any:
		for i := range n {
			n[i] = p.walk(n[i], fmt.Sprintf("%s[%d]", path, i))
		}
		return n
	case string:
		if !HasTemplate(n) {
			return n
		}
		st := p.parseString(n, path)
		if st == nil {
			return n // problems recorded; keep the original string
		}
		p.ct.templates = append(p.ct.templates, st)
		return st
	default:
		return node
	}
}

// joinPath appends one object key to a config-relative JSON path.
func joinPath(path, key string) string {
	if path == "" {
		return key
	}
	return path + "." + key
}

// parseString compiles one templated string: split into literal and
// expression segments, rewrite each expression (bare references become
// strict lookups), and parse the reconstructed source as a text/template.
// Returns nil after recording problems when the string cannot compile.
func (p *tmplParser) parseString(src, path string) *strTemplate {
	segs, err := splitTemplate(src)
	if err != nil {
		p.errs = append(p.errs, &TemplateError{Path: path, Msg: err.Error()})
		return nil
	}
	nActions := 0
	wholeCandidate := true
	for _, s := range segs {
		if s.isAction {
			nActions++
		} else if strings.TrimSpace(s.text) != "" {
			wholeCandidate = false
		}
	}
	whole := wholeCandidate && nActions == 1

	var b strings.Builder
	ok := true
	for _, s := range segs {
		if !s.isAction {
			if !whole { // a whole-expression template drops its whitespace shell
				b.WriteString(s.text)
			}
			continue
		}
		rewritten, refs, err := rewriteExpr(s.text, path)
		if err != nil {
			p.errs = append(p.errs, err)
			ok = false
			continue
		}
		p.ct.refs = append(p.ct.refs, refs...)
		sink := "__fmt"
		if whole {
			sink = "__cap"
		}
		b.WriteString(tmplLeftDelim + " " + rewritten + " | " + sink + " " + tmplRightDelim)
	}
	if !ok {
		return nil
	}
	tmpl, err := template.New(path).
		Delims(tmplLeftDelim, tmplRightDelim).
		Funcs(p.ct.funcMap()).
		Parse(b.String())
	if err != nil {
		p.errs = append(p.errs, &TemplateError{Path: path, Msg: fmt.Sprintf("parsing template %q: %v", src, err)})
		return nil
	}
	return &strTemplate{path: path, src: src, tmpl: tmpl, whole: whole}
}

// tmplSegment is one piece of a templated string: literal text or the
// inner expression of one `${{ ... }}` action.
type tmplSegment struct {
	text     string
	isAction bool
}

// splitTemplate splits src into literal and expression segments. The
// closing delimiter is scanned quote-aware, so a `}}` inside a quoted
// string does not close the action; an unterminated action is an error.
func splitTemplate(src string) ([]tmplSegment, error) {
	var segs []tmplSegment
	rest := src
	for {
		open := strings.Index(rest, tmplLeftDelim)
		if open < 0 {
			if rest != "" {
				segs = append(segs, tmplSegment{text: rest})
			}
			return segs, nil
		}
		if open > 0 {
			segs = append(segs, tmplSegment{text: rest[:open]})
		}
		body := rest[open+len(tmplLeftDelim):]
		end, err := findActionEnd(body)
		if err != nil {
			return nil, err
		}
		segs = append(segs, tmplSegment{text: body[:end], isAction: true})
		rest = body[end+len(tmplRightDelim):]
	}
}

// findActionEnd returns the index of the closing `}}` in body, skipping
// quoted and raw string literals.
func findActionEnd(body string) (int, error) {
	for i := 0; i < len(body); i++ {
		switch body[i] {
		case '}':
			if strings.HasPrefix(body[i:], tmplRightDelim) {
				return i, nil
			}
		case '"', '\'':
			j, err := skipQuoted(body, i)
			if err != nil {
				return 0, err
			}
			i = j
		case '`':
			j := strings.IndexByte(body[i+1:], '`')
			if j < 0 {
				return 0, errors.New("unterminated raw string literal in template expression")
			}
			i += j + 1
		}
	}
	return 0, fmt.Errorf("unterminated template expression (missing %q)", tmplRightDelim)
}

// skipQuoted returns the index of the closing quote of the string literal
// opening at body[start] (double- or single-quoted, per the opener),
// honoring backslash escapes.
func skipQuoted(body string, start int) (int, error) {
	quote := body[start]
	for i := start + 1; i < len(body); i++ {
		switch body[i] {
		case '\\':
			i++
		case quote:
			return i, nil
		}
	}
	return 0, errors.New("unterminated string literal in template expression")
}

// unquoteSingle decodes the body of a single-quoted string literal: `\'`
// and `\\` unescape, every other byte is literal. Single quotes exist so
// expressions embedded in JSON documents avoid `\"` escaping noise; they
// are re-emitted as Go double-quoted strings for text/template.
func unquoteSingle(lit string) string {
	var b strings.Builder
	for i := 0; i < len(lit); i++ {
		if lit[i] == '\\' && i+1 < len(lit) && (lit[i+1] == '\'' || lit[i+1] == '\\') {
			i++
		}
		b.WriteByte(lit[i])
	}
	return b.String()
}

// tmplKeywords are the text/template control keywords rejected in
// expressions: rendering is a single pipeline per action by design.
var tmplKeywords = map[string]bool{
	"if": true, "else": true, "end": true, "range": true, "with": true,
	"template": true, "block": true, "define": true, "break": true, "continue": true,
}

// tmplAllowedIdents are the bare identifiers an expression may use: the
// four public functions plus the boolean/nil literals. Everything else —
// text/template's builtins (printf, index, call, ...) included — is
// rejected at parse time, which is what makes the FuncMap genuinely
// restricted (builtins cannot be removed from text/template itself).
var tmplAllowedIdents = map[string]bool{
	"get": true, "default": true, "toJson": true, "truncate": true,
	"true": true, "false": true, "nil": true,
}

// isRefChar reports whether c may appear in a bare reference token
// (dotted path of step IDs, JSON keys, and array indices).
func isRefChar(c byte) bool {
	return c == '.' || c == '_' || c == '-' ||
		(c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
}

// rewriteExpr validates and rewrites one action's expression: bare
// `steps.` / `run.` references become strict `__ref` lookups, reference-
// shaped string literals (lenient `get` paths) are recorded for output
// fetching, control structures and variable declarations are rejected.
func rewriteExpr(expr, path string) (string, []TemplateRef, error) {
	var b strings.Builder
	var refs []TemplateRef
	first := true
	for i := 0; i < len(expr); {
		c := expr[i]
		switch {
		case c == '"':
			j, err := skipQuoted(expr, i)
			if err != nil {
				return "", nil, &TemplateError{Path: path, Msg: err.Error()}
			}
			lit := expr[i+1 : j]
			if r, ok := literalRef(lit, path); ok {
				refs = append(refs, r)
			}
			b.WriteString(expr[i : j+1])
			i = j + 1
			first = false
		case c == '\'':
			j, err := skipQuoted(expr, i)
			if err != nil {
				return "", nil, &TemplateError{Path: path, Msg: err.Error()}
			}
			lit := unquoteSingle(expr[i+1 : j])
			if r, ok := literalRef(lit, path); ok {
				refs = append(refs, r)
			}
			b.WriteString(strconv.Quote(lit))
			i = j + 1
			first = false
		case c == '`':
			j := strings.IndexByte(expr[i+1:], '`')
			if j < 0 {
				return "", nil, &TemplateError{Path: path, Msg: "unterminated raw string literal in template expression"}
			}
			if r, ok := literalRef(expr[i+1:i+1+j], path); ok {
				refs = append(refs, r)
			}
			b.WriteString(expr[i : i+j+2])
			i += j + 2
			first = false
		case c == '$':
			return "", nil, &TemplateError{Path: path, Msg: "template variables are not supported in expressions"}
		case c == ':' && i+1 < len(expr) && expr[i+1] == '=':
			return "", nil, &TemplateError{Path: path, Msg: "variable declarations are not supported in template expressions"}
		case isRefChar(c):
			j := i
			for j < len(expr) && isRefChar(expr[j]) {
				j++
			}
			tok := expr[i:j]
			if first && tmplKeywords[tok] {
				return "", nil, &TemplateError{Path: path, Msg: fmt.Sprintf("control structures (%q) are not supported in template expressions", tok)}
			}
			first = false
			switch {
			case isRefToken(tok):
				ref, err := parseRef(tok, path)
				if err != nil {
					return "", nil, err
				}
				refs = append(refs, ref)
				b.WriteString(`(__ref ` + strconv.Quote(tok) + `)`)
			case isIdent(tok) && !tmplAllowedIdents[tok]:
				return "", nil, &TemplateError{
					Path: path,
					Msg:  fmt.Sprintf("unknown function %q (available: get, default, toJson, truncate)", tok),
				}
			default:
				b.WriteString(tok)
			}
			i = j
		default:
			if !isSpace(c) {
				first = false
			}
			b.WriteByte(c)
			i++
		}
	}
	return b.String(), refs, nil
}

// isSpace reports ASCII whitespace (the only whitespace the expression
// grammar cares about).
func isSpace(c byte) bool { return c == ' ' || c == '\t' || c == '\n' || c == '\r' }

// isIdent reports whether a token is a bare identifier (a would-be
// function name or literal): dotless and starting with a letter or
// underscore. Numbers and dotted reference tokens are handled elsewhere.
func isIdent(tok string) bool {
	if strings.Contains(tok, ".") {
		return false
	}
	c := tok[0]
	return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

// isRefToken reports whether a bare token is a reference attempt: the
// known roots (with or without a path), or any other dotted identifier
// that is not a number literal — so `secrets.key` or a `step.a` typo
// gets a typed unknown-root error instead of text/template's opaque
// "function not defined".
func isRefToken(tok string) bool {
	if tok == "steps" || tok == "run" ||
		strings.HasPrefix(tok, "steps.") || strings.HasPrefix(tok, "run.") {
		return true
	}
	if !strings.Contains(tok, ".") {
		return false
	}
	_, err := strconv.ParseFloat(tok, 64)
	return err != nil // dotted and not a number → a reference attempt
}

// parseRef validates one bare reference token against the reference
// grammar: steps.<id>.output[.<path>] or run.params[.<key>[.<path>]].
func parseRef(tok, path string) (TemplateRef, error) {
	segs := strings.Split(tok, ".")
	for _, s := range segs {
		if s == "" {
			return TemplateRef{}, &TemplateRefError{Path: path, Ref: tok, Msg: "empty path segment"}
		}
	}
	switch segs[0] {
	case "steps":
		if len(segs) < 3 || segs[2] != "output" {
			return TemplateRef{}, &TemplateRefError{
				Path: path, Ref: tok,
				Msg: "step references must have the form steps.<id>.output[.<path>] (only a step's output is addressable)",
			}
		}
		if !stepIDRe.MatchString(segs[1]) {
			return TemplateRef{}, &TemplateRefError{
				Path: path, Ref: tok,
				Msg: fmt.Sprintf("step ID %q does not match %s", segs[1], stepIDRe),
			}
		}
		return TemplateRef{ConfigPath: path, Raw: tok, StepID: segs[1]}, nil
	case "run":
		if len(segs) < 2 || segs[1] != "params" {
			return TemplateRef{}, &TemplateRefError{
				Path: path, Ref: tok,
				Msg: "run references must have the form run.params[.<key>]",
			}
		}
		r := TemplateRef{ConfigPath: path, Raw: tok}
		if len(segs) > 2 {
			r.ParamKey = segs[2]
		}
		return r, nil
	default:
		return TemplateRef{}, &TemplateRefError{Path: path, Ref: tok, Msg: "unknown reference root"}
	}
}

// literalRef recognizes a reference-shaped string literal — a lenient
// `get` path. Only well-formed step references matter (they feed the
// engine's output fetch); anything else resolves to nil at runtime by
// `get`'s contract and needs no static handling.
func literalRef(lit, path string) (TemplateRef, bool) {
	if !strings.HasPrefix(lit, "steps.") {
		return TemplateRef{}, false
	}
	for i := 0; i < len(lit); i++ {
		if !isRefChar(lit[i]) {
			return TemplateRef{}, false
		}
	}
	r, err := parseRef(lit, path)
	if err != nil {
		return TemplateRef{}, false
	}
	r.Lenient = true
	return r, true
}

// funcMap builds the restricted template function set, closing over the
// ConfigTemplate's render state. __ref, __cap, and __fmt are internal
// plumbing (strict lookup, whole-expression capture, interpolation
// formatting); get, default, toJson, and truncate are the public four.
func (ct *ConfigTemplate) funcMap() template.FuncMap {
	st := ct.state
	return template.FuncMap{
		"__ref": func(path string) (any, error) {
			v, err := resolvePath(st.root, path)
			if err != nil {
				st.refErr = err
				return nil, err
			}
			return v, nil
		},
		"__cap": func(v any) string {
			st.captured = v
			return ""
		},
		"__fmt": fmtValue,
		"get": func(path string) any {
			v, err := resolvePath(st.root, path)
			if err != nil {
				return nil
			}
			return v
		},
		"default": func(fallback, v any) any {
			if v == nil || v == "" {
				return fallback
			}
			return v
		},
		"toJson": func(v any) (string, error) {
			b, err := marshalNoEscape(v)
			if err != nil {
				return "", err
			}
			return string(b), nil
		},
		"truncate": func(n int, v any) (string, error) {
			s, err := fmtValue(v)
			if err != nil {
				return "", err
			}
			if n <= 0 {
				return "", nil
			}
			if utf8.RuneCountInString(s) <= n {
				return s, nil
			}
			runes := []rune(s)
			return string(runes[:n]), nil
		},
	}
}

// fmtValue renders a resolved value for string interpolation: strings
// verbatim, numbers and booleans in their JSON spelling, nil as the empty
// string, and composites as compact JSON.
func fmtValue(v any) (string, error) {
	switch x := v.(type) {
	case nil:
		return "", nil
	case string:
		return x, nil
	case json.Number:
		return x.String(), nil
	case bool:
		return strconv.FormatBool(x), nil
	default:
		b, err := marshalNoEscape(v)
		if err != nil {
			return "", err
		}
		return string(b), nil
	}
}

// resolvePath walks a dotted reference path through the resolution root:
// maps by key, arrays by integer index. Any miss — absent key, bad or
// out-of-range index, descending into a scalar — is a *MissingRefError
// naming the failing segment.
func resolvePath(root map[string]any, path string) (any, error) {
	var cur any = root
	for _, seg := range strings.Split(path, ".") {
		switch node := cur.(type) {
		case map[string]any:
			v, ok := node[seg]
			if !ok {
				return nil, &MissingRefError{Ref: path, Detail: fmt.Sprintf("nothing at segment %q", seg)}
			}
			cur = v
		case []any:
			idx, err := strconv.Atoi(seg)
			if err != nil {
				return nil, &MissingRefError{Ref: path, Detail: fmt.Sprintf("segment %q indexes an array but is not an integer", seg)}
			}
			if idx < 0 || idx >= len(node) {
				return nil, &MissingRefError{Ref: path, Detail: fmt.Sprintf("index %d out of range (array has %d elements)", idx, len(node))}
			}
			cur = node[idx]
		default:
			return nil, &MissingRefError{Ref: path, Detail: fmt.Sprintf("segment %q descends into a non-container value", seg)}
		}
	}
	return cur, nil
}
