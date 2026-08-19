// toFlow — definition value → canvas state (ADR-019).
//
// Shape-checks the minimum a canvas needs, lifts per-step positions out of the
// `ui` block, and carries everything else — every config value, envelope block,
// unknown field, and residual `ui` key — verbatim into the Flow. It does NOT
// validate configs, edge endpoints, cycles, or CEL (that is 17.5); a dangling
// edge or an invalid config rides faithfully into the Flow.

import type { Edge, Step, StepType } from "./generated/definition.js";
import { assignEdgeIds } from "./edge-id.js";
import { GraphdefError, type Flow, type FlowEdge, type FlowNode, type Position, type ToFlowOptions } from "./types.js";
import { isFiniteNumber, isPlainObject, omit } from "./util.js";

/** A deterministic grid used when a step carries no `ui.nodes[id].position`. */
function defaultGridPosition(_step: Step, index: number): Position {
  const COLS = 4;
  const DX = 220;
  const DY = 140;
  return { x: (index % COLS) * DX, y: Math.floor(index / COLS) * DY };
}

/** Read a valid `{x, y}` position from a `ui.nodes[id]` entry, or null. */
function readPosition(entry: Record<string, unknown>): Position | null {
  const p = entry["position"];
  if (isPlainObject(p) && isFiniteNumber(p["x"]) && isFiniteNumber(p["y"])) {
    return { x: p["x"], y: p["y"] };
  }
  return null;
}

/**
 * Map a definition value (typed or freshly parsed `unknown`) to canvas state.
 * Throws {@link GraphdefError} on a shape violation or a duplicate step id.
 */
export function toFlow(input: unknown, opts: ToFlowOptions = {}): Flow {
  if (!isPlainObject(input)) {
    throw new GraphdefError("not_object", "", "definition must be a JSON object");
  }
  const defaultPosition = opts.defaultPosition ?? defaultGridPosition;

  // --- steps ---
  const rawSteps = input["steps"];
  if (!Array.isArray(rawSteps)) {
    throw new GraphdefError("steps_not_array", "steps", "`steps` must be an array");
  }

  // --- ui: split the per-step node map from the rest ---
  const uiPresent = "ui" in input;
  const rawUI = input["ui"];
  if (uiPresent && !isPlainObject(rawUI)) {
    throw new GraphdefError("ui_not_object", "ui", "`ui` must be a JSON object");
  }
  const uiObj: Record<string, unknown> = isPlainObject(rawUI) ? rawUI : {};
  const rawNodes = uiObj["nodes"];
  // Only treat `ui.nodes` as a per-step map when it is a plain object; anything
  // else is left in `ui` untouched (no lifting), so it round-trips verbatim.
  const nodesMap: Record<string, unknown> | null = isPlainObject(rawNodes) ? rawNodes : null;

  const stepIds = new Set<string>();
  const nodes: FlowNode[] = rawSteps.map((raw, i) => {
    if (!isPlainObject(raw)) {
      throw new GraphdefError("step_invalid", `steps[${i}]`, "step must be a JSON object");
    }
    const id = raw["id"];
    if (typeof id !== "string") {
      throw new GraphdefError("step_invalid", `steps[${i}].id`, "step `id` must be a string");
    }
    const type = raw["type"];
    if (typeof type !== "string") {
      throw new GraphdefError("step_invalid", `steps[${i}].type`, "step `type` must be a string");
    }
    if (stepIds.has(id)) {
      throw new GraphdefError("duplicate_step_id", `steps[${i}].id`, `duplicate step id ${JSON.stringify(id)}`);
    }
    stepIds.add(id);

    // Lift this step's ui.nodes entry, if any.
    let positioned = false;
    let position: Position;
    let nodeUI: Record<string, unknown> | undefined;
    const entry = nodesMap ? nodesMap[id] : undefined;
    if (isPlainObject(entry)) {
      const lifted = readPosition(entry);
      if (lifted) {
        positioned = true;
        position = lifted;
        nodeUI = omit(entry, ["position"]); // rest may be {}; presence is meaningful
      } else {
        // Entry present but no valid position: keep it whole, synthesize a spot.
        position = defaultPosition(raw as unknown as Step, i);
        nodeUI = { ...entry };
      }
    } else {
      position = defaultPosition(raw as unknown as Step, i);
      nodeUI = undefined; // no entry at all
    }

    const data =
      nodeUI === undefined
        ? { step: raw as unknown as Step, positioned }
        : { step: raw as unknown as Step, positioned, ui: nodeUI };
    return { id, type: type as StepType, position, data };
  });

  // Residual ui: everything except the per-step entries we lifted. Orphan
  // entries (ids that name no step) and an empty `nodes` container are retained.
  let ui: Record<string, unknown>;
  if (nodesMap) {
    const orphans: Record<string, unknown> = {};
    for (const k of Object.keys(nodesMap)) {
      if (!stepIds.has(k)) orphans[k] = nodesMap[k];
    }
    ui = { ...omit(uiObj, ["nodes"]), nodes: orphans };
  } else {
    ui = { ...uiObj }; // `nodes` (if any) was not a plain object → left in place
  }

  // --- edges ---
  const rawEdges = input["edges"];
  if (!Array.isArray(rawEdges)) {
    throw new GraphdefError("edges_not_array", "edges", "`edges` must be an array");
  }
  const parsedEdges = rawEdges.map((raw, i) => {
    if (!isPlainObject(raw)) {
      throw new GraphdefError("edge_invalid", `edges[${i}]`, "edge must be a JSON object");
    }
    const from = raw["from"];
    const to = raw["to"];
    if (typeof from !== "string") {
      throw new GraphdefError("edge_invalid", `edges[${i}].from`, "edge `from` must be a string");
    }
    if (typeof to !== "string") {
      throw new GraphdefError("edge_invalid", `edges[${i}].to`, "edge `to` must be a string");
    }
    return { from, to, raw };
  });
  const ids = assignEdgeIds(parsedEdges);
  const edges: FlowEdge[] = parsedEdges.map((e, i) => ({
    id: ids[i]!,
    source: e.from,
    target: e.to,
    data: { edge: e.raw as unknown as Edge },
  }));

  // --- doc meta: everything except steps/edges/ui ---
  const doc = omit(input, ["steps", "edges", "ui"]);

  return { doc, nodes, edges, ui, uiPresent };
}
