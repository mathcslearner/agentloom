package dag

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// This file is the map fan-out generator (ADR-015, ticket 13.4): the pure
// function that turns a validated sub-template plus a runtime list into a
// PlanOutput delta, and the per-instance reference rewriter it depends on. The
// engine calls GenerateMapExpansion inside the map step's completion
// transaction, then applies the delta through store.ExpandRun — the same
// primitive a planner uses, with an engine-generated delta instead of an
// LLM-authored one. It is IO-free and deterministic given its inputs.

// GatherIDSuffix names a map's generated gather join in the reserved
// instance-id space: `<map_id>#gather`.
const GatherIDSuffix = "#gather"

// MapGatherID returns the id of the gather join a map step generates.
func MapGatherID(mapStepID string) string { return mapStepID + GatherIDSuffix }

// GenerateMapExpansion builds the PlanOutput a map step's completion applies
// (ADR-015): one instance of the body sub-template per item (ids `<step>#<k>`,
// item and internal step references rewritten per instance), spliced between
// the map step (an `after` edge `map -> entry#k`) and a generated gather join
// (`sink#k -> gather`), plus the gather step itself — a barrier whose config
// collects `${{ steps.<sink>#k.output }}` into the ordered result array.
//
// tmpl is assumed validated by checkTemplateSection (unique local ids, acyclic,
// exactly one sink), so entry/sink detection and rewriting cannot fail on shape.
// An empty items list yields just the gather (an empty ordered array) — a valid
// no-op-shaped expansion. items carries the raw JSON of each element, spliced
// into instance configs through the map step's own output at runtime.
func GenerateMapExpansion(tmpl *Template, items []json.RawMessage, mapStepID string) (PlanOutput, error) {
	if tmpl == nil || len(tmpl.Steps) == 0 {
		return PlanOutput{}, fmt.Errorf("dag: map body is empty")
	}
	entries, sink, err := templateFrontier(tmpl)
	if err != nil {
		return PlanOutput{}, err
	}
	gatherID := MapGatherID(mapStepID)

	plan := PlanOutput{SchemaVersion: PlanSchemaVersion}
	sinkRefs := make([]string, len(items))
	for k := range items {
		for _, s := range tmpl.Steps {
			inst, err := rewriteInstanceStep(s, mapStepID, k)
			if err != nil {
				return PlanOutput{}, fmt.Errorf("dag: map instance %d step %q: %w", k, s.ID, err)
			}
			plan.Steps = append(plan.Steps, inst)
		}
		for _, e := range tmpl.Edges {
			plan.Edges = append(plan.Edges, Edge{
				From: instanceID(e.From, k), To: instanceID(e.To, k),
				When: e.When, Type: e.Type, Condition: e.Condition, MaxIterations: e.MaxIterations,
				Decision: e.Decision,
			})
		}
		// Splice: the map step readies each entry instance (after-splice), and
		// each sink instance fans into the gather.
		for _, en := range entries {
			plan.Edges = append(plan.Edges, Edge{From: mapStepID, To: instanceID(en, k)})
		}
		plan.Edges = append(plan.Edges, Edge{From: instanceID(sink, k), To: gatherID})
		sinkRefs[k] = "${{ steps." + instanceID(sink, k) + ".output }}"
	}

	gatherCfg, err := gatherConfigJSON(sinkRefs)
	if err != nil {
		return PlanOutput{}, err
	}
	gc, err := DecodeStepConfig(StepGather, gatherCfg)
	if err != nil {
		return PlanOutput{}, fmt.Errorf("dag: building gather config: %w", err)
	}
	plan.Steps = append(plan.Steps, Step{ID: gatherID, Type: StepGather, Config: gc})
	return plan, nil
}

// instanceID mints the reserved instance id `<base>#<k>` for instance k.
func instanceID(base string, k int) string { return base + "#" + strconv.Itoa(k) }

// templateFrontier returns the body's entry steps (no incoming normal edge) and
// its single sink step (no outgoing normal edge). Validation guarantees exactly
// one sink; a body with none or several is a validation failure, defensively
// re-reported here so the engine never generates a malformed delta.
func templateFrontier(tmpl *Template) (entries []string, sink string, err error) {
	indeg := make(map[string]int, len(tmpl.Steps))
	outdeg := make(map[string]int, len(tmpl.Steps))
	for _, s := range tmpl.Steps {
		indeg[s.ID] = 0
		outdeg[s.ID] = 0
	}
	for _, e := range tmpl.Edges {
		if e.IsLoop() {
			continue // loop edges never gate readiness
		}
		outdeg[e.From]++
		indeg[e.To]++
	}
	var sinks []string
	for _, s := range tmpl.Steps {
		if indeg[s.ID] == 0 {
			entries = append(entries, s.ID)
		}
		if outdeg[s.ID] == 0 {
			sinks = append(sinks, s.ID)
		}
	}
	if len(sinks) != 1 {
		return nil, "", fmt.Errorf("dag: map body must have exactly one sink, found %d %v", len(sinks), sinks)
	}
	return entries, sinks[0], nil
}

// rewriteInstanceStep clones one template step into instance k: the id gains
// the `#k` suffix, the config's item/step references are rewritten, and the
// context spec's step_output sources (if any) are re-pointed at the instance's
// steps. Envelope policies with no step references (retry, timeout, cache,
// budget, validation, blackboard) are carried verbatim.
func rewriteInstanceStep(s Step, mapStepID string, k int) (Step, error) {
	inst := Step{
		ID:         instanceID(s.ID, k),
		Type:       s.Type,
		Retry:      s.Retry,
		Timeout:    s.Timeout,
		Cache:      s.Cache,
		Budget:     s.Budget,
		Validation: s.Validation,
		Blackboard: s.Blackboard,
		Context:    rewriteInstanceContext(s.Context, k),
	}
	if s.Config != nil {
		raw, err := marshalNoEscape(s.Config)
		if err != nil {
			return Step{}, err
		}
		rewritten, err := rewriteConfigRefs(raw, mapStepID, k)
		if err != nil {
			return Step{}, err
		}
		cfg, err := DecodeStepConfig(s.Type, rewritten)
		if err != nil {
			return Step{}, fmt.Errorf("re-decoding rewritten config: %w", err)
		}
		inst.Config = cfg
	}
	return inst, nil
}

// rewriteInstanceContext re-points a body step's step_output context sources at
// the instance's steps (`x` → `x#k`), leaving every other source kind alone. A
// nil spec (the common case) returns nil.
func rewriteInstanceContext(cs *ContextSpec, k int) *ContextSpec {
	if cs == nil {
		return nil
	}
	out := &ContextSpec{
		BudgetTokens: cs.BudgetTokens,
		Compaction:   cs.Compaction,
		Sources:      make([]ContextSource, len(cs.Sources)),
	}
	for i, src := range cs.Sources {
		if src.Kind == SourceStepOutput && src.Step != "" {
			src.Step = instanceID(src.Step, k)
		}
		out.Sources[i] = src
	}
	return out
}

// gatherConfigJSON builds a gather step's config: the ordered list of
// per-instance output references the gather resolves into its result array.
func gatherConfigJSON(sinkRefs []string) (json.RawMessage, error) {
	items, err := marshalNoEscape(sinkRefs)
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	buf.WriteString(`{"items":`)
	buf.Write(items)
	buf.WriteByte('}')
	return buf.Bytes(), nil
}

// rewriteConfigRefs rewrites the template references inside one config's JSON
// string values for instance k of a map over mapStepID:
//
//	${{ item }}        -> ${{ steps.<map>.output.items.<k> }}
//	${{ item.<p> }}    -> ${{ steps.<map>.output.items.<k>.<p> }}
//	${{ item_index }}  -> ${{ steps.<map>.output.indices.<k> }}
//	${{ steps.<x>… }}  -> ${{ steps.<x>#<k>… }}
//
// run.params references and non-reference text are left unchanged. The result
// is a config the runtime renderer resolves like any other — the item flows
// through the map step's own output, and internal refs point at instance
// siblings. A config with no templates returns unchanged.
func rewriteConfigRefs(config json.RawMessage, mapStepID string, k int) (json.RawMessage, error) {
	if len(config) == 0 || !HasTemplate(string(config)) {
		return config, nil
	}
	dec := json.NewDecoder(bytes.NewReader(config))
	dec.UseNumber()
	var tree any
	if err := dec.Decode(&tree); err != nil {
		return nil, fmt.Errorf("config is not valid JSON: %w", err)
	}
	rw := instanceRefRewriter(mapStepID, k)
	rewritten, err := rewriteTreeStrings(tree, func(s string) (string, error) {
		return rewriteTemplateString(s, rw)
	})
	if err != nil {
		return nil, err
	}
	return marshalNoEscape(rewritten)
}

// instanceRefRewriter returns the per-token rewrite for instance k: item roots
// map to the map step's output, local step refs gain the `#k` suffix, and
// everything else is unchanged.
func instanceRefRewriter(mapStepID string, k int) func(string) string {
	ks := strconv.Itoa(k)
	itemRoot := "steps." + mapStepID + ".output.items." + ks
	indexRef := "steps." + mapStepID + ".output.indices." + ks
	return func(tok string) string {
		switch {
		case tok == "item":
			return itemRoot
		case strings.HasPrefix(tok, "item."):
			return itemRoot + tok[len("item"):] // ".<path>" preserved
		case tok == "item_index":
			return indexRef
		case strings.HasPrefix(tok, "steps."):
			rest := tok[len("steps."):]
			id, tail, hasTail := strings.Cut(rest, ".")
			out := "steps." + id + "#" + ks
			if hasTail {
				out += "." + tail
			}
			return out
		default:
			return tok // run.params, function idents, literals
		}
	}
}

// rewriteTreeStrings applies fn to every string value in a decoded JSON tree,
// recursing through objects and arrays.
func rewriteTreeStrings(node any, fn func(string) (string, error)) (any, error) {
	switch n := node.(type) {
	case map[string]any:
		for key, v := range n {
			rv, err := rewriteTreeStrings(v, fn)
			if err != nil {
				return nil, err
			}
			n[key] = rv
		}
		return n, nil
	case []any:
		for i, v := range n {
			rv, err := rewriteTreeStrings(v, fn)
			if err != nil {
				return nil, err
			}
			n[i] = rv
		}
		return n, nil
	case string:
		return fn(n)
	default:
		return node, nil
	}
}

// rewriteTemplateString rewrites the reference tokens inside one string's
// `${{ ... }}` actions, leaving literal segments untouched. Non-templated
// strings pass through.
func rewriteTemplateString(src string, rw func(string) string) (string, error) {
	if !HasTemplate(src) {
		return src, nil
	}
	segs, err := splitTemplate(src)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	for _, s := range segs {
		if !s.isAction {
			b.WriteString(s.text)
			continue
		}
		b.WriteString(tmplLeftDelim)
		b.WriteString(rewriteExprRefs(s.text, rw))
		b.WriteString(tmplRightDelim)
	}
	return b.String(), nil
}

// rewriteExprRefs rewrites the reference tokens in one `${{ }}` expression body,
// applying rw to each bare reference token and to the contents of quoted string
// literals (the lenient `get` paths). Non-reference text — operators,
// whitespace, function idents — is preserved verbatim.
func rewriteExprRefs(expr string, rw func(string) string) string {
	var b strings.Builder
	for i := 0; i < len(expr); {
		c := expr[i]
		switch {
		case c == '"' || c == '\'':
			j, err := skipQuoted(expr, i)
			if err != nil {
				b.WriteString(expr[i:]) // malformed — leave the remainder as-is
				return b.String()
			}
			b.WriteByte(c)
			b.WriteString(rewriteQuotedRef(expr[i+1:j], rw))
			b.WriteByte(expr[j])
			i = j + 1
		case c == '`':
			j := strings.IndexByte(expr[i+1:], '`')
			if j < 0 {
				b.WriteString(expr[i:])
				return b.String()
			}
			b.WriteByte('`')
			b.WriteString(rewriteQuotedRef(expr[i+1:i+1+j], rw))
			b.WriteByte('`')
			i += j + 1 + 1
		case isRefChar(c):
			j := i
			for j < len(expr) && isRefChar(expr[j]) {
				j++
			}
			b.WriteString(rw(expr[i:j]))
			i = j
		default:
			b.WriteByte(c)
			i++
		}
	}
	return b.String()
}

// rewriteQuotedRef rewrites a quoted string literal's contents only when the
// whole literal is a single reference-shaped token (a lenient `get` path);
// arbitrary literal text (a default value, a message) is left unchanged.
func rewriteQuotedRef(lit string, rw func(string) string) string {
	for i := 0; i < len(lit); i++ {
		if !isRefChar(lit[i]) {
			return lit // contains non-ref characters — not a bare ref path
		}
	}
	if lit == "" {
		return lit
	}
	if lit == "item" || lit == "item_index" ||
		strings.HasPrefix(lit, "item.") || strings.HasPrefix(lit, "steps.") {
		return rw(lit)
	}
	return lit
}
