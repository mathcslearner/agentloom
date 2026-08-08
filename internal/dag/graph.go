package dag

import (
	"fmt"
	"strings"
)

// Graph is the adjacency view of a Definition used by the graph algorithms
// (ticket 1.4): cycle detection, topological ordering, reachability, and
// readiness. Loop edges are indexed separately and excluded from adjacency —
// per ADR-003 they participate in expansion (M14), not in readiness or skip
// propagation, and are the only sanctioned cycles.
//
// A Graph is immutable after construction and safe for concurrent use.
type Graph struct {
	def   *Definition
	index map[string]int // step ID → declaration index

	// outNormal / inNormal hold, per step (by declaration index), the
	// indices into def.Edges of its normal out- and in-edges, in edge
	// declaration order.
	outNormal [][]int
	inNormal  [][]int

	// loopEdges holds the indices of the marked loop edges, in declaration
	// order.
	loopEdges []int
}

// NewGraph builds the adjacency view of def. It fails only on definitions
// no graph can be built from — nil, duplicate step IDs, or edges naming
// unknown steps; everything else (including cycles, which Validate rejects)
// is left to the algorithms so their errors carry precise context.
func NewGraph(def *Definition) (*Graph, error) {
	if def == nil {
		return nil, fmt.Errorf("dag: NewGraph called with nil definition")
	}
	g := &Graph{
		def:       def,
		index:     make(map[string]int, len(def.Steps)),
		outNormal: make([][]int, len(def.Steps)),
		inNormal:  make([][]int, len(def.Steps)),
	}
	for i, s := range def.Steps {
		if first, dup := g.index[s.ID]; dup {
			return nil, fmt.Errorf("dag: NewGraph: duplicate step ID %q (steps[%d] and steps[%d])", s.ID, first, i)
		}
		g.index[s.ID] = i
	}
	for i, e := range def.Edges {
		from, ok := g.index[e.From]
		if !ok {
			return nil, fmt.Errorf("dag: NewGraph: edges[%d].from names unknown step %q", i, e.From)
		}
		to, ok := g.index[e.To]
		if !ok {
			return nil, fmt.Errorf("dag: NewGraph: edges[%d].to names unknown step %q", i, e.To)
		}
		if e.IsLoop() {
			g.loopEdges = append(g.loopEdges, i)
			continue
		}
		g.outNormal[from] = append(g.outNormal[from], i)
		g.inNormal[to] = append(g.inNormal[to], i)
	}
	return g, nil
}

// toIdx returns the declaration index of edge i's target step.
func (g *Graph) toIdx(edge int) int { return g.index[g.def.Edges[edge].To] }

// TopoOrder returns the step IDs in a topological order of the normal-edge
// graph (Kahn's algorithm, deterministic: ties break in declaration order).
// It fails if the normal-edge graph has a cycle; Validate is the layer that
// reports cycles with per-edge paths.
func (g *Graph) TopoOrder() ([]string, error) {
	order, ok := g.topoOrder()
	if !ok {
		return nil, fmt.Errorf("dag: TopoOrder: the normal-edge graph has a cycle (run Validate for the offending edges)")
	}
	ids := make([]string, len(order))
	for i, idx := range order {
		ids[i] = g.def.Steps[idx].ID
	}
	return ids, nil
}

// topoOrder is TopoOrder on declaration indices; ok is false when a cycle
// prevents a complete order.
func (g *Graph) topoOrder() (order []int, ok bool) {
	remaining := make([]int, len(g.def.Steps))
	queue := make([]int, 0, len(g.def.Steps))
	for i := range g.def.Steps {
		remaining[i] = len(g.inNormal[i])
		if remaining[i] == 0 {
			queue = append(queue, i)
		}
	}
	order = make([]int, 0, len(g.def.Steps))
	for len(queue) > 0 {
		s := queue[0]
		queue = queue[1:]
		order = append(order, s)
		for _, e := range g.outNormal[s] {
			t := g.toIdx(e)
			if remaining[t]--; remaining[t] == 0 {
				queue = append(queue, t)
			}
		}
	}
	return order, len(order) == len(g.def.Steps)
}

// Ancestors returns the set of step IDs with a non-empty normal-edge path
// to stepID. A step is never its own ancestor (normal-edge cycles are
// rejected by Validate before ancestry is meaningful).
func (g *Graph) Ancestors(stepID string) (map[string]bool, error) {
	start, ok := g.index[stepID]
	if !ok {
		return nil, fmt.Errorf("dag: Ancestors: unknown step %q", stepID)
	}
	ancestors := make(map[string]bool)
	visited := make([]bool, len(g.def.Steps))
	queue := []int{start}
	for len(queue) > 0 {
		s := queue[0]
		queue = queue[1:]
		for _, e := range g.inNormal[s] {
			p := g.index[g.def.Edges[e].From]
			if !visited[p] {
				visited[p] = true
				ancestors[g.def.Steps[p].ID] = true
				queue = append(queue, p)
			}
		}
	}
	return ancestors, nil
}

// reaches reports whether a non-empty normal-edge path leads from step
// index `from` to step index `to`. Starting and ending at the same step
// requires an actual cycle through it, not the trivial empty path.
func (g *Graph) reaches(from, to int) bool {
	visited := make([]bool, len(g.def.Steps))
	queue := []int{from}
	for len(queue) > 0 {
		s := queue[0]
		queue = queue[1:]
		for _, e := range g.outNormal[s] {
			t := g.toIdx(e)
			if t == to {
				return true
			}
			if !visited[t] {
				visited[t] = true
				queue = append(queue, t)
			}
		}
	}
	return false
}

// cycleFinding is one cycle in the normal-edge graph: the edge that closes
// it and the full step path, e.g. ["a", "b", "a"].
type cycleFinding struct {
	edgeIdx int
	path    []string
}

// pathString renders the cycle path for error messages.
func (c cycleFinding) pathString() string { return strings.Join(c.path, " -> ") }

// findCycles locates cycles in the normal-edge graph via iterative
// three-color DFS (iterative so a 10k-step chain cannot overflow the
// stack). It reports one finding per back edge encountered — distinct
// cycles each produce their own finding — deterministically: DFS roots and
// out-edges are visited in declaration order.
func (g *Graph) findCycles() []cycleFinding {
	const (
		white = iota // unvisited
		gray         // on the current DFS path
		black        // fully explored
	)
	color := make([]int8, len(g.def.Steps))
	type frame struct {
		step int
		next int // position in outNormal[step]
	}
	var findings []cycleFinding
	var stack []frame

	for root := range g.def.Steps {
		if color[root] != white {
			continue
		}
		color[root] = gray
		stack = append(stack[:0], frame{step: root})
		for len(stack) > 0 {
			f := &stack[len(stack)-1]
			if f.next == len(g.outNormal[f.step]) {
				color[f.step] = black
				stack = stack[:len(stack)-1]
				continue
			}
			e := g.outNormal[f.step][f.next]
			f.next++
			t := g.toIdx(e)
			switch color[t] {
			case white:
				color[t] = gray
				stack = append(stack, frame{step: t})
			case gray:
				// Back edge: the cycle is the DFS path from t to the top of
				// the stack, closed by e back to t.
				var path []string
				for i := range stack {
					if stack[i].step == t {
						for _, fr := range stack[i:] {
							path = append(path, g.def.Steps[fr.step].ID)
						}
						break
					}
				}
				path = append(path, g.def.Steps[t].ID)
				findings = append(findings, cycleFinding{edgeIdx: e, path: path})
			}
		}
	}
	return findings
}
