"use client";

// Problems derivation (ticket 17.4): run the client config validator over the
// current canvas definition and map each issue onto the node it belongs to, so
// the canvas can mark invalid nodes and the (17.6) submit can block on errors.
// steps[i] maps to nodes[i] — toDefinition emits steps in node order, so the
// index in a `steps[i]…` path is the node's position.

import { useMemo } from "react";
import { hasErrors, validateStepConfigs, type Issue } from "@agentloom/graphdef";
import { useBuilderStore } from "./store";
import { useCatalogStore } from "./catalog-store";

/** The leading `steps[<i>]` index of a backend path, or null. */
export function stepIndexOfPath(path: string): number | null {
  const m = /^steps\[(\d+)\]/.exec(path);
  return m ? Number(m[1]) : null;
}

export interface Problems {
  issues: Issue[];
  /** node id → its issues. */
  byNode: Map<string, Issue[]>;
  hasErrors: boolean;
}

/** Map issues onto node ids by their `steps[i]` index, given the node order. */
export function mapIssuesToNodes(issues: readonly Issue[], nodeIds: readonly string[]): Map<string, Issue[]> {
  const byNode = new Map<string, Issue[]>();
  for (const issue of issues) {
    const idx = stepIndexOfPath(issue.path);
    if (idx === null || idx < 0 || idx >= nodeIds.length) continue;
    const id = nodeIds[idx]!;
    const list = byNode.get(id) ?? [];
    list.push(issue);
    byNode.set(id, list);
  }
  return byNode;
}

/** Derive the live problems for the current canvas. */
export function useProblems(): Problems {
  const nodes = useBuilderStore((s) => s.nodes);
  const edges = useBuilderStore((s) => s.edges);
  const toDefinitionValue = useBuilderStore((s) => s.toDefinitionValue);
  const schemas = useCatalogStore((s) => s.schemas);

  return useMemo(() => {
    const def = toDefinitionValue();
    const issues = validateStepConfigs(def, schemas);
    const nodeIds = nodes.map((n) => n.id);
    return { issues, byNode: mapIssuesToNodes(issues, nodeIds), hasErrors: hasErrors(issues) };
    // edges is a dependency so the memo recomputes when the graph changes (the
    // definition depends on both nodes and edges).
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [nodes, edges, schemas, toDefinitionValue]);
}

/** Blocking-errors selector for the (17.6) submit flow. */
export function useHasBlockingErrors(): boolean {
  return useProblems().hasErrors;
}
