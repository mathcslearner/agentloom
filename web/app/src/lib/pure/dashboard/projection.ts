/**
 * Project the live topology + status + layout into render-ready DAG nodes and
 * edges (ticket 18.2). Kept React-Flow-free (like graphdef's `Flow`): the
 * component adapts these plain shapes to React Flow `Node`/`Edge`, so this
 * module stays under the `pure/` eslint boundary and is unit-testable without a
 * canvas.
 *
 * Grouping (loop iterations / map instances) is rendered as a background
 * container node bounding its members. Collapsing a group hides its members and
 * their internal edges and re-targets any edge crossing the group boundary onto
 * the container node — so a collapsed loop reads as a single box with its
 * in/out edges intact.
 */
import type { GraphTopology, TopoEdge } from "./graph-topology";
import type { StepState } from "./run-state";
import { type ApprovalMap, pendingForStep } from "./approvals";
import type { LayoutState, Point } from "./layout";
import { NODE_H, NODE_W } from "./layout";
import { computeGroups, groupStatus, type NodeGroup } from "./groups";
import { edgeVisual, skinFor, type EdgeVisual, type SkinSpec } from "./skins";
import { edgeKey } from "./graph-topology";
import { deriveHandles } from "@/lib/pure/builder/ports";
import type { Edge as GraphdefEdge } from "@agentloom/graphdef";

const GROUP_PAD = 24;
const GROUP_HEADER = 28;

export interface RunStepData {
  stepId: string;
  stepType: string;
  state?: StepState;
  skin: SkinSpec;
  origin: { kind: string; step?: string };
  /** Whether the node was just injected (drives the enter animation). */
  entered: boolean;
  /** The id of the pending approval on this step, if it is a `human_approval`
   * gate awaiting a decision (ticket 18.5) — drives the "Decide" affordance. */
  pendingApprovalId?: string;
}

export interface RunGroupData {
  group: NodeGroup;
  collapsed: boolean;
  memberCount: number;
  status: string;
}

export interface DagNode {
  id: string;
  kind: "runStep" | "runGroup";
  position: Point;
  data: RunStepData | RunGroupData;
  hidden?: boolean;
  width?: number;
  height?: number;
  /** z-order: groups render behind steps. */
  z: number;
}

export interface RunEdgeData {
  edge: TopoEdge;
  visual: EdgeVisual;
}

export interface DagEdge {
  id: string;
  source: string;
  target: string;
  sourceHandle: string;
  targetHandle: string;
  kind: "runStep" | "runLoop";
  hidden?: boolean;
  data: RunEdgeData;
}

export interface ProjectInput {
  topology: GraphTopology;
  steps: Map<string, StepState>;
  layout: LayoutState;
  collapsed: Set<string>;
  /** Event seqs recently applied — a node whose addedSeq is here animates in. */
  recent?: Set<number>;
  /** Pending/decided approvals (18.5) — a gate awaiting a decision gets a
   * "Decide" affordance carrying its approval id. */
  approvals?: ApprovalMap;
}

export interface Projection {
  nodes: DagNode[];
  edges: DagEdge[];
  groups: NodeGroup[];
}

export function projectGraph(input: ProjectInput): Projection {
  const { topology, steps, layout, collapsed, recent, approvals } = input;
  const groups = computeGroups(topology);
  const groupOf = memberGroupIndex(groups);

  const nodes: DagNode[] = [];

  // Group container nodes (rendered behind steps). A collapsed container is a
  // compact pill; an expanded one bounds its members.
  const collapsedGroups = new Set<string>();
  for (const g of groups) {
    const isCollapsed = collapsed.has(g.id);
    if (isCollapsed) collapsedGroups.add(g.id);
    const box = groupBox(g, layout, isCollapsed);
    if (!box) continue;
    nodes.push({
      id: g.id,
      kind: "runGroup",
      position: box.position,
      width: box.width,
      height: box.height,
      z: 0,
      data: {
        group: g,
        collapsed: isCollapsed,
        memberCount: g.members.length,
        status: groupStatus(g.members, steps),
      },
    });
  }

  // Step nodes.
  for (const node of topology.nodes.values()) {
    const gid = groupOf.get(node.id);
    const hidden = gid != null && collapsedGroups.has(gid);
    const pos = layout.positions.get(node.id) ?? { x: 0, y: 0 };
    const state = steps.get(node.id);
    nodes.push({
      id: node.id,
      kind: "runStep",
      position: pos,
      z: 1,
      hidden,
      data: {
        stepId: node.id,
        stepType: node.type,
        state,
        skin: state ? skinFor(state) : neutralSkin(),
        origin: node.origin,
        entered: node.addedSeq != null && recent?.has(node.addedSeq) === true,
        pendingApprovalId:
          state?.status === "awaiting_human" && approvals
            ? pendingForStep(approvals, node.id)?.id
            : undefined,
      },
    });
  }

  // Edges, with collapse re-targeting and de-duplication.
  const edges: DagEdge[] = [];
  const seen = new Set<string>();
  for (const edge of topology.edges.values()) {
    const srcGroup = collapsedGroups.has(groupOf.get(edge.from) ?? "") ? groupOf.get(edge.from) : undefined;
    const dstGroup = collapsedGroups.has(groupOf.get(edge.to) ?? "") ? groupOf.get(edge.to) : undefined;
    // Both endpoints inside the same collapsed group → internal, hide.
    if (srcGroup && dstGroup && srcGroup === dstGroup) continue;

    const source = srcGroup ?? edge.from;
    const target = dstGroup ?? edge.to;
    const id = `e:${edgeKey(source, target, edge.type)}`;
    if (seen.has(id)) continue; // multiple member edges collapse to one
    seen.add(id);

    const handles = deriveHandles({ from: edge.from, to: edge.to, type: edge.type, decision: edge.decision } as GraphdefEdge);
    const visual = edgeVisual(edge, steps.get(edge.from), steps.get(edge.to));
    edges.push({
      id,
      source,
      target,
      sourceHandle: handles.sourceHandle,
      targetHandle: handles.targetHandle,
      kind: edge.type === "loop" ? "runLoop" : "runStep",
      data: { edge, visual },
    });
  }

  return { nodes, edges, groups };
}

/** Map each grouped member step id to its group id. */
function memberGroupIndex(groups: NodeGroup[]): Map<string, string> {
  const out = new Map<string, string>();
  for (const g of groups) for (const m of g.members) out.set(m, g.id);
  return out;
}

/** The container node's box: bounding the members (expanded) or a compact pill
 * anchored at the members' top-left (collapsed). Null when no member is placed
 * yet (a group whose members haven't been laid out). */
function groupBox(
  g: NodeGroup,
  layout: LayoutState,
  collapsed: boolean,
): { position: Point; width: number; height: number } | null {
  let minX = Infinity;
  let minY = Infinity;
  let maxX = -Infinity;
  let maxY = -Infinity;
  let any = false;
  for (const id of g.members) {
    const p = layout.positions.get(id);
    if (!p) continue;
    any = true;
    minX = Math.min(minX, p.x);
    minY = Math.min(minY, p.y);
    maxX = Math.max(maxX, p.x + NODE_W);
    maxY = Math.max(maxY, p.y + NODE_H);
  }
  if (!any) return null;
  if (collapsed) {
    return { position: { x: minX, y: minY }, width: NODE_W, height: NODE_H };
  }
  return {
    position: { x: minX - GROUP_PAD, y: minY - GROUP_PAD - GROUP_HEADER },
    width: maxX - minX + GROUP_PAD * 2,
    height: maxY - minY + GROUP_PAD * 2 + GROUP_HEADER,
  };
}

function neutralSkin(): SkinSpec {
  return { tone: "neutral", badge: "pending", summary: "waiting on dependencies", pulse: false };
}
