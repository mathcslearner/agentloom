"use client";

// A React context carrying the per-node problem counts (ticket 17.4), computed
// once from the whole-graph validation (useProblems) and read by each StepNode.
// This keeps validation O(n) per render instead of O(n²) (every node calling
// the whole-graph validator), and keeps the problem marks out of the undoable
// node `data` (they are derived, not canvas state).

import { createContext, useContext, type ReactNode } from "react";
import { useProblems } from "./problems";

const NodeProblemsContext = createContext<Map<string, number>>(new Map());

export function ProblemsProvider({ children }: { children: ReactNode }) {
  const { byNode } = useProblems();
  const counts = new Map<string, number>();
  for (const [id, issues] of byNode) counts.set(id, issues.filter((i) => i.severity === "error").length);
  return <NodeProblemsContext.Provider value={counts}>{children}</NodeProblemsContext.Provider>;
}

/** The count of error-severity problems on a node, or 0. */
export function useNodeProblemCount(id: string): number {
  return useContext(NodeProblemsContext).get(id) ?? 0;
}
