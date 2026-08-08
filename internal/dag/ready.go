package dag

import (
	"fmt"
	"slices"
	"strings"
)

// stepState classifies a step during one ReadySteps computation.
type stepState int8

const (
	statePending stepState = iota
	stateCompleted
	stateSkipped
	stateFailed
)

// edgeResolution is the state of one normal edge (ADR-003 "Edge
// resolution"): unresolved until its source reaches a terminal state,
// then fired or skipped.
type edgeResolution int8

const (
	edgeUnresolved edgeResolution = iota
	edgeFired
	edgeSkipped
)

// ReadySteps computes which steps are eligible to run given the current
// run state, implementing ADR-003's readiness, skip-propagation, and join
// semantics over the normal-edge graph (loop edges are ignored; they
// belong to expansion, M14).
//
// Inputs are disjoint sets of step IDs: completed (succeeded), skipped
// (resolved skipped — at run time the engine seeds this from `when`-false
// and branch pass-over outcomes), and failed. Every step in none of the
// sets is pending. Edges resolve from step state alone: out-edges of
// completed steps fired, of skipped steps skipped, of failed or pending
// steps unresolved; per-edge CEL outcomes refine this at the engine layer
// (1.5/M4) by seeding the skipped set.
//
// ready holds the pending steps whose dependencies are satisfied: entry
// steps; non-join steps and `join all` steps with every incoming edge
// resolved and at least one fired; `join any` steps with at least one
// incoming edge fired. A failed parent therefore permanently blocks its
// non-`join any` dependents — never ready, never skipped — until ADR-006's
// failure policy (M5) decides the run's fate. Callers must exclude steps
// they have already dispatched: a ready step stays ready until it enters
// one of the input sets.
//
// newlySkipped holds the pending steps skip propagation resolved this
// call (every incoming edge skipped); the engine persists them, feeding
// them back through the skipped set on subsequent calls.
//
// Both results are in step declaration order. ReadySteps is pure and
// deterministic; it fails on unknown or overlapping input IDs and on a
// cyclic normal-edge graph (Validate rejects those definitions).
func (g *Graph) ReadySteps(completed, skipped, failed map[string]bool) (ready, newlySkipped []string, err error) {
	states, err := g.inputStates(completed, skipped, failed)
	if err != nil {
		return nil, nil, err
	}
	order, ok := g.topoOrder()
	if !ok {
		return nil, nil, fmt.Errorf("dag: ReadySteps: the normal-edge graph has a cycle (run Validate for the offending edges)")
	}

	// resolve computes a normal edge's resolution from its source's state.
	resolve := func(edge int) edgeResolution {
		switch states[g.index[g.def.Edges[edge].From]] {
		case stateCompleted:
			return edgeFired
		case stateSkipped:
			return edgeSkipped
		default:
			return edgeUnresolved
		}
	}

	// Skip propagation in topological order: each step sees its parents'
	// final states, so one pass reaches the fixed point and terminates
	// (the normal-edge graph is a DAG).
	for _, s := range order {
		if states[s] != statePending || len(g.inNormal[s]) == 0 {
			continue
		}
		allSkipped := true
		for _, e := range g.inNormal[s] {
			if resolve(e) != edgeSkipped {
				allSkipped = false
				break
			}
		}
		if allSkipped {
			states[s] = stateSkipped
			newlySkipped = append(newlySkipped, g.def.Steps[s].ID)
		}
	}
	// Topological collection order back to canonical declaration order.
	slices.SortFunc(newlySkipped, func(a, b string) int { return g.index[a] - g.index[b] })

	// Readiness, in declaration order. Entry steps are ready at run
	// creation; `join any` fires on the first fired edge; everything else
	// (non-join and `join all` share the rule) needs all edges resolved
	// with at least one fired.
	for s, step := range g.def.Steps {
		if states[s] != statePending {
			continue
		}
		if len(g.inNormal[s]) == 0 {
			ready = append(ready, step.ID)
			continue
		}
		anyFired, allResolved := false, true
		for _, e := range g.inNormal[s] {
			switch resolve(e) {
			case edgeFired:
				anyFired = true
			case edgeUnresolved:
				allResolved = false
			}
		}
		if joinMode(step) == JoinAny {
			if anyFired {
				ready = append(ready, step.ID)
			}
			continue
		}
		if allResolved && anyFired {
			ready = append(ready, step.ID)
		}
	}
	return ready, newlySkipped, nil
}

// joinMode returns the join mode of a join step, or "" for any other step
// (including a join with missing config, which then gets the stricter
// `all`-shaped readiness rule).
func joinMode(s Step) JoinMode {
	if s.Type != StepJoin {
		return ""
	}
	if c, ok := s.Config.(*JoinConfig); ok && c != nil {
		return c.Mode
	}
	return ""
}

// inputStates folds the three input sets into per-step states, rejecting
// unknown and overlapping IDs (an engine bug worth failing loudly on).
func (g *Graph) inputStates(completed, skipped, failed map[string]bool) ([]stepState, error) {
	states := make([]stepState, len(g.def.Steps))
	var unknown, overlap []string
	assign := func(set map[string]bool, state stepState) {
		for id, in := range set {
			if !in {
				continue
			}
			idx, ok := g.index[id]
			if !ok {
				unknown = append(unknown, id)
				continue
			}
			if states[idx] != statePending {
				overlap = append(overlap, id)
				continue
			}
			states[idx] = state
		}
	}
	assign(completed, stateCompleted)
	assign(skipped, stateSkipped)
	assign(failed, stateFailed)
	if len(unknown) == 0 && len(overlap) == 0 {
		return states, nil
	}
	var probs []string
	if len(unknown) > 0 {
		slices.Sort(unknown)
		probs = append(probs, fmt.Sprintf("unknown steps %q", unknown))
	}
	if len(overlap) > 0 {
		slices.Sort(overlap)
		probs = append(probs, fmt.Sprintf("steps in more than one set %q", overlap))
	}
	return nil, fmt.Errorf("dag: ReadySteps: %s", strings.Join(probs, "; "))
}
