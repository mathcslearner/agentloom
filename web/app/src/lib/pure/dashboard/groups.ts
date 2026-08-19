/**
 * Iteration / instance grouping for the live DAG (ticket 18.2). Loop bodies are
 * unrolled into `<step>#k` instances (ADR-015/14.3) and map fan-outs into
 * `<step>#k` instances; both are grouped so the graph stays legible — a loop's
 * iterations and a map's instances collapse into one container.
 *
 * The grouping is derived purely from provenance (origin.kind + origin.step)
 * and the instance suffix, so it needs no extra backend data:
 *   - loop nodes group per iteration: `loop:<origin>:<k>` (label "iteration k");
 *   - map nodes group per map step:   `map:<origin>` (label "N instances");
 *   - planner injections are not grouped (badge only).
 *
 * No React/UI imports — under the app's `pure/` eslint boundary.
 */
import type { GraphTopology, TopoNode } from "./graph-topology";
import type { DisplayStepStatus, StepState } from "./run-state";

export interface NodeGroup {
  id: string;
  kind: "loop" | "map";
  /** The step whose expansion produced these members (the loop source / map). */
  origin: string;
  /** The iteration number (loops), when applicable. */
  iteration?: string;
  label: string;
  /** Member step ids, in a stable order. */
  members: string[];
}

/** Split an instance id into its base and the ordered `#` suffix segments. */
export function parseInstance(id: string): { base: string; suffixes: string[] } {
  const parts = id.split("#");
  return { base: parts[0]!, suffixes: parts.slice(1) };
}

/**
 * The group an injected node belongs to, or null when it is ungrouped (an
 * authored node or a planner injection). Loops group by (origin, last suffix);
 * maps group by origin.
 */
export function groupIdFor(node: TopoNode): { id: string; kind: "loop" | "map"; iteration?: string } | null {
  const origin = node.origin.step;
  if (!origin) return null;
  const { suffixes } = parseInstance(node.id);
  if (node.origin.kind === "loop") {
    const iter = suffixes[suffixes.length - 1] ?? "1";
    return { id: `loop:${origin}:${iter}`, kind: "loop", iteration: iter };
  }
  if (node.origin.kind === "map") {
    return { id: `map:${origin}`, kind: "map" };
  }
  return null;
}

/**
 * Compute the node groups for a topology, keeping member order stable (topology
 * insertion order). Groups with a single member are still returned — a
 * one-instance map or a first loop iteration is a legitimate (if small) group.
 */
export function computeGroups(topo: GraphTopology): NodeGroup[] {
  const byId = new Map<string, NodeGroup>();
  for (const node of topo.nodes.values()) {
    const g = groupIdFor(node);
    if (!g) continue;
    let group = byId.get(g.id);
    if (!group) {
      group = {
        id: g.id,
        kind: g.kind,
        origin: node.origin.step!,
        iteration: g.iteration,
        label: "",
        members: [],
      };
      byId.set(g.id, group);
    }
    group.members.push(node.id);
  }
  const groups = [...byId.values()];
  for (const g of groups) {
    g.label =
      g.kind === "loop"
        ? `iteration ${g.iteration}`
        : `${g.members.length} instance${g.members.length === 1 ? "" : "s"}`;
  }
  return groups;
}

/**
 * A rolled-up status for a collapsed group: the "most active / most severe"
 * member status wins, so a collapsed pill reflects what is happening inside.
 */
export function groupStatus(members: string[], steps: Map<string, StepState>): DisplayStepStatus {
  let best: DisplayStepStatus = "pending";
  let bestRank = -1;
  for (const id of members) {
    const s = steps.get(id);
    if (!s) continue;
    const r = STATUS_RANK[s.status];
    if (r > bestRank) {
      bestRank = r;
      best = s.status;
    }
  }
  return best;
}

// Higher = more worth surfacing on a collapsed pill: a failure or an active
// step should dominate a group of otherwise-idle ones.
const STATUS_RANK: Record<DisplayStepStatus, number> = {
  dead_lettered: 9,
  failed: 8,
  awaiting_human: 7,
  running: 6,
  throttled: 5,
  retrying: 4,
  ready: 3,
  succeeded: 2,
  collected: 1,
  cancelled: 1,
  skipped: 1,
  pending: 0,
};
