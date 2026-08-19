// A pure normal-edge graph over a definition's steps/edges, ported from
// internal/dag/graph.go: the adjacency the graph-semantic checks need
// (findCycles, reaches, Ancestors, loopBody). Loop edges (`type: "loop"`) are
// excluded from readiness adjacency exactly as on the backend. Operates on the
// raw step/edge shapes (after the decode stage has confirmed unique ids and
// resolved endpoints), so it never depends on the typed Definition.

/** A step as the graph reads it: an id and whether it is a loop edge target. */
export interface GraphStep {
  id: string;
}

/** An edge as the graph reads it. `loop` marks an M1 loop edge. */
export interface GraphEdge {
  from: string;
  to: string;
  loop: boolean;
}

/** One cycle finding: the declaration index of the closing edge and the path. */
export interface CycleFinding {
  edgeIdx: number;
  path: string[];
}

export function pathString(c: CycleFinding): string {
  return c.path.join(" -> ");
}

export class Graph {
  readonly steps: GraphStep[];
  readonly edges: GraphEdge[];
  readonly index = new Map<string, number>(); // step id → declaration index
  readonly outNormal: number[][]; // per step: outgoing normal-edge indices
  readonly inNormal: number[][]; // per step: incoming normal-edge indices
  readonly loopEdges: number[] = []; // indices of marked loop edges

  constructor(steps: GraphStep[], edges: GraphEdge[]) {
    this.steps = steps;
    this.edges = edges;
    this.outNormal = steps.map(() => []);
    this.inNormal = steps.map(() => []);
    steps.forEach((s, i) => this.index.set(s.id, i));
    edges.forEach((e, i) => {
      if (e.loop) {
        this.loopEdges.push(i);
        return;
      }
      const from = this.index.get(e.from);
      const to = this.index.get(e.to);
      if (from !== undefined) this.outNormal[from]!.push(i);
      if (to !== undefined) this.inNormal[to]!.push(i);
    });
  }

  private toIdx(edge: number): number {
    return this.index.get(this.edges[edge]!.to)!;
  }

  /**
   * Locate cycles in the normal-edge graph via iterative three-colour DFS in
   * declaration order (dag.Graph.findCycles), one finding per back edge.
   */
  findCycles(): CycleFinding[] {
    const WHITE = 0;
    const GRAY = 1;
    const BLACK = 2;
    const color = new Array<number>(this.steps.length).fill(WHITE);
    const findings: CycleFinding[] = [];

    for (let root = 0; root < this.steps.length; root += 1) {
      if (color[root] !== WHITE) continue;
      color[root] = GRAY;
      const stack: Array<{ step: number; next: number }> = [{ step: root, next: 0 }];
      while (stack.length > 0) {
        const f = stack[stack.length - 1]!;
        const outs = this.outNormal[f.step]!;
        if (f.next === outs.length) {
          color[f.step] = BLACK;
          stack.pop();
          continue;
        }
        const e = outs[f.next]!;
        f.next += 1;
        const t = this.toIdx(e);
        if (color[t] === WHITE) {
          color[t] = GRAY;
          stack.push({ step: t, next: 0 });
        } else if (color[t] === GRAY) {
          const path: string[] = [];
          for (let i = 0; i < stack.length; i += 1) {
            if (stack[i]!.step === t) {
              for (const fr of stack.slice(i)) path.push(this.steps[fr.step]!.id);
              break;
            }
          }
          path.push(this.steps[t]!.id);
          findings.push({ edgeIdx: e, path });
        }
      }
    }
    return findings;
  }

  /** BFS reachability over normal edges (dag.Graph.reaches), by index. */
  reaches(from: number, to: number): boolean {
    const visited = new Array<boolean>(this.steps.length).fill(false);
    const queue = [from];
    while (queue.length > 0) {
      const s = queue.shift()!;
      for (const e of this.outNormal[s]!) {
        const t = this.toIdx(e);
        if (t === to) return true;
        if (!visited[t]) {
          visited[t] = true;
          queue.push(t);
        }
      }
    }
    return false;
  }

  /** The set of step ids strictly upstream of stepID via normal edges (dag.Graph.Ancestors). */
  ancestors(stepID: string): Set<string> {
    const start = this.index.get(stepID);
    const out = new Set<string>();
    if (start === undefined) return out;
    const visited = new Array<boolean>(this.steps.length).fill(false);
    const queue = [start];
    while (queue.length > 0) {
      const s = queue.shift()!;
      for (const e of this.inNormal[s]!) {
        const p = this.index.get(this.edges[e]!.from)!;
        if (!visited[p]) {
          visited[p] = true;
          out.add(this.steps[p]!.id);
          queue.push(p);
        }
      }
    }
    return out;
  }
}

/** Whether the normal-edge graph (all loop edges removed) is well-formed enough to build a Graph. */
export function hasUniqueIds(steps: readonly GraphStep[]): boolean {
  const seen = new Set<string>();
  for (const s of steps) {
    if (seen.has(s.id)) return false;
    seen.add(s.id);
  }
  return true;
}
