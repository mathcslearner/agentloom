"use client";

// Problems derivation (ticket 17.5): run the full client graph validator
// (validateDefinition) over the current canvas definition and map each issue
// onto the node or edge it belongs to. steps[i] ↔ nodes[i] and edges[i] ↔
// edges[i] by construction (toDefinition emits both in canvas order), so a
// path's index is the array position. Issues carry an optional `related[]` of
// extra paths (e.g. every node/edge on a cycle) for highlight.

import { useMemo } from "react";
import { hasErrors, validateDefinition, type Issue } from "@agentloom/graphdef";
import { useBuilderStore } from "./store";

/** The leading `steps[<i>]` index of a backend path, or null. */
export function stepIndexOfPath(path: string): number | null {
  const m = /^steps\[(\d+)\]/.exec(path);
  return m ? Number(m[1]) : null;
}

/** The leading `edges[<i>]` index of a backend path, or null. */
export function edgeIndexOfPath(path: string): number | null {
  const m = /^edges\[(\d+)\]/.exec(path);
  return m ? Number(m[1]) : null;
}

export interface Problems {
  issues: Issue[];
  errors: Issue[];
  warnings: Issue[];
  /** node id → its issues. */
  byNode: Map<string, Issue[]>;
  /** edge id → its issues. */
  byEdge: Map<string, Issue[]>;
  /** issues that belong to neither a node nor an edge (document-level). */
  document: Issue[];
  hasErrors: boolean;
}

function push(map: Map<string, Issue[]>, key: string, issue: Issue): void {
  const list = map.get(key) ?? [];
  list.push(issue);
  map.set(key, list);
}

/** Map issues onto node ids by their `steps[i]` index, given the node order. */
export function mapIssuesToNodes(issues: readonly Issue[], nodeIds: readonly string[]): Map<string, Issue[]> {
  const byNode = new Map<string, Issue[]>();
  for (const issue of issues) {
    const idx = stepIndexOfPath(issue.path);
    if (idx === null || idx < 0 || idx >= nodeIds.length) continue;
    push(byNode, nodeIds[idx]!, issue);
  }
  return byNode;
}

/** Map issues onto edge ids by their `edges[i]` index, given the edge order. */
export function mapIssuesToEdges(issues: readonly Issue[], edgeIds: readonly string[]): Map<string, Issue[]> {
  const byEdge = new Map<string, Issue[]>();
  for (const issue of issues) {
    if (stepIndexOfPath(issue.path) !== null) continue; // a steps[i] path is a node's
    const idx = edgeIndexOfPath(issue.path);
    if (idx === null || idx < 0 || idx >= edgeIds.length) continue;
    push(byEdge, edgeIds[idx]!, issue);
  }
  return byEdge;
}

/** The element an issue's own path points at (a cycle's `edges[i]` → that edge). */
export function primaryTarget(
  issue: Issue,
  nodeIds: readonly string[],
  edgeIds: readonly string[],
): { kind: "node" | "edge"; id: string } | null {
  const si = stepIndexOfPath(issue.path);
  if (si !== null && si >= 0 && si < nodeIds.length) return { kind: "node", id: nodeIds[si]! };
  const ei = edgeIndexOfPath(issue.path);
  if (ei !== null && ei >= 0 && ei < edgeIds.length) return { kind: "edge", id: edgeIds[ei]! };
  return null;
}

/** The set of node ids and edge ids an issue implicates (its path + related). */
export function issueTargets(
  issue: Issue,
  nodeIds: readonly string[],
  edgeIds: readonly string[],
): { nodes: string[]; edges: string[] } {
  const nodes = new Set<string>();
  const edges = new Set<string>();
  for (const p of [issue.path, ...(issue.related ?? [])]) {
    const si = stepIndexOfPath(p);
    if (si !== null && si >= 0 && si < nodeIds.length) {
      nodes.add(nodeIds[si]!);
      continue;
    }
    const ei = edgeIndexOfPath(p);
    if (ei !== null && ei >= 0 && ei < edgeIds.length) edges.add(edgeIds[ei]!);
  }
  return { nodes: [...nodes], edges: [...edges] };
}

/** Derive the live problems for the current canvas. */
export function useProblems(): Problems {
  const nodes = useBuilderStore((s) => s.nodes);
  const edges = useBuilderStore((s) => s.edges);
  const toDefinitionValue = useBuilderStore((s) => s.toDefinitionValue);

  return useMemo(() => {
    const def = toDefinitionValue();
    const issues = validateDefinition(def);
    const errors = issues.filter((i) => i.severity === "error");
    const warnings = issues.filter((i) => i.severity === "warning");
    const nodeIds = nodes.map((n) => n.id);
    const edgeIds = edges.map((e) => e.id);
    const byNode = mapIssuesToNodes(issues, nodeIds);
    const byEdge = mapIssuesToEdges(issues, edgeIds);
    const claimed = new Set([...byNode.values(), ...byEdge.values()].flat());
    const document = issues.filter((i) => !claimed.has(i));
    return { issues, errors, warnings, byNode, byEdge, document, hasErrors: hasErrors(issues) };
  }, [nodes, edges, toDefinitionValue]);
}

/** Blocking-errors selector for the (17.6) submit flow. */
export function useHasBlockingErrors(): boolean {
  return useProblems().hasErrors;
}
