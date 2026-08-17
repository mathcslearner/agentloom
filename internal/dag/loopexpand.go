package dag

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// This file is the loop unroller (ADR-015/ADR-016, ticket 14.3): the pure
// function that turns a marked loop edge plus the completing iteration into a
// PlanOutput delta for the next iteration. The engine calls
// GenerateLoopExpansion inside the loop source's completion transaction, then
// applies the delta through store.ExpandRun — the same primitive a map uses,
// with an engine-generated delta. It is IO-free and deterministic.
//
// A loop is executed by cloning its body — the normal-edge span from the loop
// edge's `to` (the body entry) to its `from` (the loop source) — as fresh
// instances `<id>#<iter>`, chaining one iteration onto the last:
//
//   - after-splice: <current loop-source instance> -> <entry>#<iter>, so the
//     completing iteration readies the next one's entry.
//   - internal body edges cloned with the `#<iter>` suffix.
//   - before-splice: <loop-source>#<iter> -> <exit target>, one per non-loop
//     out-edge of the loop source, carrying its `when`. These widen the pending
//     exit target's remaining_deps; the completing iteration's own exit edge
//     resolves skipped (its `when` is false while the loop continues), so the
//     exit target always waits on exactly the newest iteration until one fires.
//
// The next iteration's loop-back is not materialized: the engine detects it
// lazily when <loop-source>#<iter> completes with the condition still true.

// GenerateLoopExpansion builds the PlanOutput one loop iteration applies.
// def is the immutable definition snapshot; loopEdge is the marked loop edge
// whose source just completed; currentInstanceID is that completing instance
// (the authored id for iteration 0, else `<id>#k`); nextIter is the iteration
// number the new body instances carry.
//
// Agent body steps are resolved (ResolveAgentStep) before cloning, so an
// injected agent instance carries the same merged, self-describing config an
// authored agent step gets at instantiation — expansion does not re-read the
// agents section.
func GenerateLoopExpansion(def *Definition, loopEdge Edge, currentInstanceID string, nextIter int) (PlanOutput, error) {
	if def == nil {
		return PlanOutput{}, fmt.Errorf("dag: GenerateLoopExpansion: nil definition")
	}
	g, err := NewGraph(def)
	if err != nil {
		return PlanOutput{}, err
	}
	from, to := loopEdge.From, loopEdge.To
	body, err := loopBody(g, from, to)
	if err != nil {
		return PlanOutput{}, err
	}
	stepByID := make(map[string]Step, len(def.Steps))
	for _, s := range def.Steps {
		stepByID[s.ID] = s
	}

	plan := PlanOutput{SchemaVersion: PlanSchemaVersion}

	// Clone body steps, sorted for a deterministic delta.
	members := make([]string, 0, len(body))
	for id := range body {
		members = append(members, id)
	}
	sort.Strings(members)
	for _, id := range members {
		s := stepByID[id]
		if s.Type == StepAgent {
			resolved, rerr := ResolveAgentStep(def, s)
			if rerr != nil {
				return PlanOutput{}, fmt.Errorf("dag: resolving loop body agent %q: %w", id, rerr)
			}
			s = resolved
		}
		inst, cerr := rewriteLoopBodyStep(s, body, nextIter)
		if cerr != nil {
			return PlanOutput{}, fmt.Errorf("dag: loop instance step %q: %w", id, cerr)
		}
		plan.Steps = append(plan.Steps, inst)
	}

	// Internal body edges (both endpoints in the body), cloned with the suffix.
	// The loop edge is never cloned — the next loop-back is lazy.
	for _, e := range def.Edges {
		if e.IsLoop() || !body[e.From] || !body[e.To] {
			continue
		}
		plan.Edges = append(plan.Edges, Edge{
			From: instanceID(e.From, nextIter),
			To:   instanceID(e.To, nextIter),
			When: e.When,
		})
	}

	// After-splice: the completing instance readies each body entry (a member
	// with no normal in-edge from another body member — for a chain, the loop
	// edge's `to`).
	entries := loopEntries(def, body)
	for _, en := range entries {
		plan.Edges = append(plan.Edges, Edge{From: currentInstanceID, To: instanceID(en, nextIter)})
	}

	// Before-splice: clone the loop source's non-loop out-edges onto this
	// iteration's loop-source instance, pointing at the same (outside-body)
	// exit targets. A non-source body member leaving the body is unsupported
	// (it would re-fire an outside step every iteration) — reject defensively.
	for _, e := range def.Edges {
		if e.IsLoop() {
			continue
		}
		if e.From == from && !body[e.To] {
			plan.Edges = append(plan.Edges, Edge{
				From: instanceID(from, nextIter),
				To:   e.To,
				When: e.When,
			})
			continue
		}
		if e.From != from && body[e.From] && !body[e.To] {
			return PlanOutput{}, fmt.Errorf("dag: loop body step %q has an edge leaving the body to %q (only the loop source may exit the body)", e.From, e.To)
		}
	}

	return plan, nil
}

// loopBody returns the members of the loop body: the normal-edge span from the
// loop edge's `to` (entry) to its `from` (loop source), inclusive. Validation
// guarantees `to` reaches `from`; the intersection of `to`'s descendants and
// `from`'s ancestors (both plus the endpoints) is exactly that span.
func loopBody(g *Graph, from, to string) (map[string]bool, error) {
	desc, err := g.Descendants(to)
	if err != nil {
		return nil, err
	}
	anc, err := g.Ancestors(from)
	if err != nil {
		return nil, err
	}
	body := map[string]bool{from: true, to: true}
	for id := range desc {
		if id == from || anc[id] {
			body[id] = true
		}
	}
	return body, nil
}

// loopEntries returns the body members with no normal in-edge from another body
// member, in definition order. These are the steps the completing iteration
// after-splices to (for a chain body, the single loop-edge `to`).
func loopEntries(def *Definition, body map[string]bool) []string {
	hasBodyParent := make(map[string]bool, len(body))
	for _, e := range def.Edges {
		if e.IsLoop() || !body[e.From] || !body[e.To] {
			continue
		}
		hasBodyParent[e.To] = true
	}
	var entries []string
	for _, s := range def.Steps {
		if body[s.ID] && !hasBodyParent[s.ID] {
			entries = append(entries, s.ID)
		}
	}
	return entries
}

// rewriteLoopBodyStep clones one body step into iteration `iter`: the id gains
// the `#iter` suffix, config and context step references to other body members
// are re-pointed at their instances, and refs to steps outside the body (and
// run.params) are left untouched. Envelope policies without step references are
// carried verbatim.
func rewriteLoopBodyStep(s Step, body map[string]bool, iter int) (Step, error) {
	rw := loopRefRewriter(body, iter)
	inst := Step{
		ID:         instanceID(s.ID, iter),
		Type:       s.Type,
		Retry:      s.Retry,
		Timeout:    s.Timeout,
		Cache:      s.Cache,
		Budget:     s.Budget,
		Validation: s.Validation,
		Blackboard: s.Blackboard,
		Context:    rewriteLoopContext(s.Context, body, iter),
	}
	if s.Config != nil {
		raw, err := marshalNoEscape(s.Config)
		if err != nil {
			return Step{}, err
		}
		rewritten, err := rewriteLoopConfigRefs(raw, rw)
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

// loopRefRewriter returns the per-token rewrite for iteration `iter`: a
// `steps.<x>…` reference whose step is a body member gains the `#iter` suffix;
// references to steps outside the body, run.params, idents, and literals are
// unchanged.
func loopRefRewriter(body map[string]bool, iter int) func(string) string {
	is := strconv.Itoa(iter)
	return func(tok string) string {
		if !strings.HasPrefix(tok, "steps.") {
			return tok
		}
		rest := tok[len("steps."):]
		id, tail, hasTail := strings.Cut(rest, ".")
		if !body[id] {
			return tok
		}
		out := "steps." + id + "#" + is
		if hasTail {
			out += "." + tail
		}
		return out
	}
}

// rewriteLoopConfigRefs rewrites the body-member references inside one config's
// JSON string values via rw. A config with no templates returns unchanged.
func rewriteLoopConfigRefs(config json.RawMessage, rw func(string) string) (json.RawMessage, error) {
	if len(config) == 0 || !HasTemplate(string(config)) {
		return config, nil
	}
	dec := json.NewDecoder(bytes.NewReader(config))
	dec.UseNumber()
	var tree any
	if err := dec.Decode(&tree); err != nil {
		return nil, fmt.Errorf("config is not valid JSON: %w", err)
	}
	rewritten, err := rewriteTreeStrings(tree, func(s string) (string, error) {
		return rewriteTemplateString(s, rw)
	})
	if err != nil {
		return nil, err
	}
	return marshalNoEscape(rewritten)
}

// rewriteLoopContext re-points a body step's step_output context sources at
// their instances (body members only), leaving every other source kind and
// out-of-body reference alone. A nil spec returns nil.
func rewriteLoopContext(cs *ContextSpec, body map[string]bool, iter int) *ContextSpec {
	if cs == nil {
		return nil
	}
	out := &ContextSpec{
		BudgetTokens: cs.BudgetTokens,
		Compaction:   cs.Compaction,
		Sources:      make([]ContextSource, len(cs.Sources)),
	}
	for i, src := range cs.Sources {
		if src.Kind == SourceStepOutput && body[src.Step] {
			src.Step = instanceID(src.Step, iter)
		}
		out.Sources[i] = src
	}
	return out
}
