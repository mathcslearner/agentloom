/**
 * Sticky, incremental layout for the live DAG (ticket 18.2). The guarantee the
 * DoD wants — "layout remains stable, no full reshuffle" when a planner/map/loop
 * expansion injects nodes — is met *by construction* here: a node that already
 * has a position (from an authored `ui` hint or a prior layout pass) never
 * moves. Only the newly-injected nodes are laid out, as a block anchored next to
 * the step that injected them, then pushed clear of anything already placed.
 *
 * The actual node placement within a block is delegated to an injected
 * `Layouter` (elkjs in the app; a deterministic fake in tests), so this module
 * stays pure and synchronous apart from awaiting that one call.
 *
 * No React/UI imports — under the app's `pure/` eslint boundary.
 */
import type { GraphTopology } from "./graph-topology";

export interface Point {
  x: number;
  y: number;
}

export interface LayoutState {
  positions: Map<string, Point>;
}

/** A layout engine over one block: relative coordinates for `nodes`, given the
 * block-internal `edges`. Injected so the module has no elkjs dependency. */
export type Layouter = (
  nodes: { id: string; width: number; height: number }[],
  edges: { from: string; to: string }[],
) => Promise<Map<string, Point>>;

export const NODE_W = 208; // matches the node's `w-52`
export const NODE_H = 68;
const GAP_X = 80;
const GAP_Y = 32;

export interface PlacementBlock {
  /** "base" (unhinted authored graph) or the injecting expansion's seq. */
  key: string;
  nodes: { id: string; type: string }[];
  /** Edges internal to the block (both endpoints in the block). */
  edges: { from: string; to: string }[];
  /** The injecting step id for an expansion block; absent for the base block. */
  origin?: string;
}

export function emptyLayout(): LayoutState {
  return { positions: new Map() };
}

/**
 * Plan the placement of a topology given the previous layout: the fixed set
 * (prior positions ∪ authored `ui` hints for nodes not yet placed) and the
 * ordered blocks of still-unplaced nodes. Pure — the actual coordinates come
 * from `applyPlacement`.
 */
export function planPlacement(
  topo: GraphTopology,
  prev: LayoutState,
): { fixed: Map<string, Point>; blocks: PlacementBlock[] } {
  const fixed = new Map(prev.positions);
  // Honor authored `ui` hints verbatim for any node not already placed.
  for (const node of topo.nodes.values()) {
    if (!fixed.has(node.id) && node.position) fixed.set(node.id, { ...node.position });
  }

  // Partition the unplaced nodes into blocks: the unhinted authored graph is
  // one "base" block; each expansion (identified by its addedSeq) is a block.
  const byKey = new Map<string, PlacementBlock>();
  for (const node of topo.nodes.values()) {
    if (fixed.has(node.id)) continue;
    const key = node.addedSeq != null ? String(node.addedSeq) : "base";
    let block = byKey.get(key);
    if (!block) {
      block = { key, nodes: [], edges: [], origin: node.origin.step };
      byKey.set(key, block);
    }
    block.nodes.push({ id: node.id, type: node.type });
  }
  // Block-internal edges (both endpoints unplaced and in the same block).
  const blockOf = new Map<string, string>();
  for (const block of byKey.values()) for (const n of block.nodes) blockOf.set(n.id, block.key);
  for (const edge of topo.edges.values()) {
    const bf = blockOf.get(edge.from);
    const bt = blockOf.get(edge.to);
    if (bf && bf === bt) byKey.get(bf)!.edges.push({ from: edge.from, to: edge.to });
  }

  // Order: base first, then expansions by ascending seq (so earlier expansions
  // anchor against the base, later ones against earlier ones).
  const blocks = [...byKey.values()].sort((a, b) => blockOrder(a.key) - blockOrder(b.key));
  return { fixed, blocks };
}

function blockOrder(key: string): number {
  return key === "base" ? -1 : Number(key);
}

/**
 * Lay out a topology, keeping every already-placed node fixed and only running
 * the layouter over each block of new nodes. Returns a new LayoutState with the
 * fixed positions plus the placed blocks. Blocks are anchored to the right of
 * their injecting step (or the fixed bounding box for the base block) and pushed
 * down until they no longer overlap anything already placed.
 */
export async function computeLayout(topo: GraphTopology, prev: LayoutState, layouter: Layouter): Promise<LayoutState> {
  const { fixed, blocks } = planPlacement(topo, prev);
  const placed = new Map(fixed);
  for (const block of blocks) {
    const rel = await layouter(
      block.nodes.map((n) => ({ id: n.id, width: NODE_W, height: NODE_H })),
      block.edges,
    );
    const anchored = anchorBlock(rel, block, placed);
    const resolved = resolveCollisions(placed, anchored);
    for (const [id, p] of resolved) placed.set(id, p);
  }
  return { positions: placed };
}

/**
 * Translate a block's relative coordinates so its top-left sits at the anchor:
 * to the right of the injecting step for an expansion block, or to the right of
 * the fixed graph's bounding box (else the origin) for the base block.
 */
export function anchorBlock(rel: Map<string, Point>, block: PlacementBlock, placed: Map<string, Point>): Map<string, Point> {
  const anchor = anchorPoint(block, placed);
  const min = boundsMin(rel.values());
  const out = new Map<string, Point>();
  for (const [id, p] of rel) out.set(id, { x: anchor.x + (p.x - min.x), y: anchor.y + (p.y - min.y) });
  return out;
}

function anchorPoint(block: PlacementBlock, placed: Map<string, Point>): Point {
  if (block.origin) {
    const origin = placed.get(block.origin);
    if (origin) return { x: origin.x + NODE_W + GAP_X, y: origin.y };
  }
  // Base block (or an expansion whose origin isn't placed yet): to the right of
  // everything placed, or the layout origin when nothing is placed.
  if (placed.size === 0) return { x: 0, y: 0 };
  let maxX = -Infinity;
  let minY = Infinity;
  for (const p of placed.values()) {
    maxX = Math.max(maxX, p.x + NODE_W);
    minY = Math.min(minY, p.y);
  }
  return { x: maxX + GAP_X, y: minY };
}

/**
 * Push a candidate block straight down (in whole rows) until none of its node
 * rects overlaps any already-placed node rect. Moving only in Y keeps the
 * horizontal left→right flow intact.
 */
export function resolveCollisions(placed: Map<string, Point>, block: Map<string, Point>): Map<string, Point> {
  const placedRects = [...placed.values()];
  let dy = 0;
  const step = NODE_H + GAP_Y;
  const guard = 1000; // never loop forever on pathological input
  for (let i = 0; i < guard; i++) {
    let collides = false;
    for (const p of block.values()) {
      const rect = { x: p.x, y: p.y + dy };
      if (placedRects.some((q) => overlaps(rect, q))) {
        collides = true;
        break;
      }
    }
    if (!collides) break;
    dy += step;
  }
  if (dy === 0) return block;
  const out = new Map<string, Point>();
  for (const [id, p] of block) out.set(id, { x: p.x, y: p.y + dy });
  return out;
}

function overlaps(a: Point, b: Point): boolean {
  return a.x < b.x + NODE_W && a.x + NODE_W > b.x && a.y < b.y + NODE_H && a.y + NODE_H > b.y;
}

function boundsMin(points: Iterable<Point>): Point {
  let x = Infinity;
  let y = Infinity;
  for (const p of points) {
    x = Math.min(x, p.x);
    y = Math.min(y, p.y);
  }
  if (!isFinite(x)) return { x: 0, y: 0 };
  return { x, y };
}

/**
 * A trivial deterministic column layouter (a fallback / test default): lays the
 * block's nodes out in a single vertical column in insertion order. Real layout
 * is elkjs; this keeps the module usable and testable without it.
 */
export const columnLayouter: Layouter = async (nodes) => {
  const out = new Map<string, Point>();
  nodes.forEach((n, i) => out.set(n.id, { x: 0, y: i * (NODE_H + GAP_Y) }));
  return out;
};
