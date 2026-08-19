"use client";

// The problems context (ticket 17.5) — computed once from the whole-graph
// validator (useProblems) and shared by every consumer: StepNode / edge
// renderers read their per-element counts and highlight state, the Problems
// panel reads the full list, and click-to-focus selects the offending element
// and centres it. Hoisted to BuilderShell (inside ReactFlowProvider) so O(n)
// validation runs once per change and focusIssue can drive the viewport.

import { createContext, useCallback, useContext, useMemo, useState, type ReactNode } from "react";
import { useReactFlow } from "@xyflow/react";
import type { Issue } from "@agentloom/graphdef";
import { issueTargets, primaryTarget, useProblems, type Problems } from "./problems";
import { useBuilderStore } from "./store";

interface Highlight {
  nodes: Set<string>;
  edges: Set<string>;
}

interface ProblemsContextValue {
  problems: Problems;
  nodeErrorCount: (id: string) => number;
  edgeErrorCount: (id: string) => number;
  isNodeHighlighted: (id: string) => boolean;
  isEdgeHighlighted: (id: string) => boolean;
  /** Select the offending element, centre it, and highlight the issue's shape. */
  focusIssue: (issue: Issue) => void;
  /** Preview an issue's highlight on hover (null clears the preview). */
  previewIssue: (issue: Issue | null) => void;
}

const EMPTY: Highlight = { nodes: new Set(), edges: new Set() };

const ProblemsContext = createContext<ProblemsContextValue | null>(null);

function errorCount(list: Issue[] | undefined): number {
  return (list ?? []).filter((i) => i.severity === "error").length;
}

export function ProblemsProvider({ children }: { children: ReactNode }) {
  const problems = useProblems();
  const [pinned, setPinned] = useState<Highlight>(EMPTY);
  const [preview, setPreview] = useState<Highlight | null>(null);
  const rf = useReactFlow();
  const selectOnly = useBuilderStore((s) => s.selectOnly);

  const focusIssue = useCallback(
    (issue: Issue) => {
      const { nodes, edges } = useBuilderStore.getState();
      const nodeIds = nodes.map((n) => n.id);
      const edgeIds = edges.map((e) => e.id);
      const targets = issueTargets(issue, nodeIds, edgeIds);
      // Select the element the issue's OWN path points at (a cycle's edges[i]
      // selects the edge, opening the edge inspector where it is fixed), so the
      // inspector follows; fall back to any implicated element.
      const primary = primaryTarget(issue, nodeIds, edgeIds);
      if (primary) selectOnly(primary.kind, primary.id);
      else if (targets.nodes.length > 0) selectOnly("node", targets.nodes[0]!);
      else if (targets.edges.length > 0) selectOnly("edge", targets.edges[0]!);
      setPinned({ nodes: new Set(targets.nodes), edges: new Set(targets.edges) });

      // Centre the viewport on the implicated nodes (edges via their endpoints).
      const focusNodeIds = new Set(targets.nodes);
      for (const eid of targets.edges) {
        const e = edges.find((x) => x.id === eid);
        if (e) {
          focusNodeIds.add(e.source);
          focusNodeIds.add(e.target);
        }
      }
      const rfNodes = [...focusNodeIds].map((id) => ({ id }));
      if (rfNodes.length > 0) void rf.fitView({ nodes: rfNodes, duration: 400, maxZoom: 1.25, padding: 0.4 });
    },
    [rf, selectOnly],
  );

  const previewIssue = useCallback((issue: Issue | null) => {
    if (!issue) {
      setPreview(null);
      return;
    }
    const { nodes, edges } = useBuilderStore.getState();
    const targets = issueTargets(
      issue,
      nodes.map((n) => n.id),
      edges.map((e) => e.id),
    );
    setPreview({ nodes: new Set(targets.nodes), edges: new Set(targets.edges) });
  }, []);

  const value = useMemo<ProblemsContextValue>(() => {
    const active = preview ?? pinned;
    return {
      problems,
      nodeErrorCount: (id) => errorCount(problems.byNode.get(id)),
      edgeErrorCount: (id) => errorCount(problems.byEdge.get(id)),
      isNodeHighlighted: (id) => active.nodes.has(id),
      isEdgeHighlighted: (id) => active.edges.has(id),
      focusIssue,
      previewIssue,
    };
  }, [problems, pinned, preview, focusIssue, previewIssue]);

  return <ProblemsContext.Provider value={value}>{children}</ProblemsContext.Provider>;
}

function useProblemsContext(): ProblemsContextValue {
  const ctx = useContext(ProblemsContext);
  if (!ctx) throw new Error("useProblemsContext must be used within a ProblemsProvider");
  return ctx;
}

/** The full problems context (panel, toolbar). */
export function useProblemsCtx(): ProblemsContextValue {
  return useProblemsContext();
}

/** The count of error-severity problems on a node, or 0 (StepNode). */
export function useNodeProblemCount(id: string): number {
  return useProblemsContext().nodeErrorCount(id);
}

/** The count of error-severity problems on an edge, or 0 (edge renderers). */
export function useEdgeProblemCount(id: string): number {
  return useProblemsContext().edgeErrorCount(id);
}

/** Whether a node is currently highlighted by a focused/hovered issue. */
export function useNodeHighlighted(id: string): boolean {
  return useProblemsContext().isNodeHighlighted(id);
}

/** Whether an edge is currently highlighted by a focused/hovered issue. */
export function useEdgeHighlighted(id: string): boolean {
  return useProblemsContext().isEdgeHighlighted(id);
}
