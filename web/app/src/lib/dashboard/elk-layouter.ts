/**
 * The elkjs-backed block layouter for the live DAG (ticket 18.2). Implements the
 * pure `Layouter` contract: given a block's nodes and internal edges, it returns
 * each node's coordinates. It is injected into `computeLayout`, so the pure
 * layout module has no elkjs dependency and stays testable with a fake.
 *
 * elkjs is loaded lazily (dynamic import) so the builder route — which never
 * lays out a run — does not pull ~1 MB of layout engine into its bundle. A
 * single ELK instance is memoized across blocks.
 */
import type { Layouter, Point } from "@/lib/pure/dashboard/layout";

type ElkNode = { id: string; width?: number; height?: number; x?: number; y?: number };
type ElkGraph = {
  id: string;
  layoutOptions?: Record<string, string>;
  children: ElkNode[];
  edges: { id: string; sources: string[]; targets: string[] }[];
};
interface ElkInstance {
  layout(graph: ElkGraph): Promise<ElkGraph>;
}

let elkPromise: Promise<ElkInstance> | null = null;

async function getElk(): Promise<ElkInstance> {
  if (!elkPromise) {
    elkPromise = import("elkjs/lib/elk.bundled.js").then((m) => {
      const ELK = (m.default ?? m) as new () => ElkInstance;
      return new ELK();
    });
  }
  return elkPromise;
}

const LAYOUT_OPTIONS: Record<string, string> = {
  "elk.algorithm": "layered",
  "elk.direction": "RIGHT",
  "elk.layered.spacing.nodeNodeBetweenLayers": "80",
  "elk.spacing.nodeNode": "32",
};

export const layoutWithElk: Layouter = async (nodes, edges) => {
  const out = new Map<string, Point>();
  if (nodes.length === 0) return out;
  const elk = await getElk();
  const graph: ElkGraph = {
    id: "root",
    layoutOptions: LAYOUT_OPTIONS,
    children: nodes.map((n) => ({ id: n.id, width: n.width, height: n.height })),
    edges: edges.map((e, i) => ({ id: `e${i}`, sources: [e.from], targets: [e.to] })),
  };
  const laid = await elk.layout(graph);
  for (const c of laid.children) out.set(c.id, { x: c.x ?? 0, y: c.y ?? 0 });
  return out;
};
