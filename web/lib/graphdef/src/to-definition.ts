// toDefinition — canvas state → definition value (ADR-019).
//
// The exact inverse of toFlow at the JSON-value level: it re-imposes each node's
// id/type onto its step, each edge's source/target onto its edge, rebuilds the
// `ui` block (viewport + unknowns + orphan node entries + the per-step positions
// of positioned nodes), and copies the document meta whole. Everything the
// mapping does not own rides through by object spread, so unknown fields survive.

import type { Definition } from "./generated/definition.js";
import type { Flow } from "./types.js";
import { isPlainObject } from "./util.js";

/** Reassemble a definition value from canvas state. Pure and deterministic. */
export function toDefinition(flow: Flow): Definition {
  const steps = flow.nodes.map((n) => ({
    ...(n.data.step as unknown as Record<string, unknown>),
    id: n.id,
    type: n.type,
  }));

  const edges = flow.edges.map((e) => ({
    ...(e.data.edge as unknown as Record<string, unknown>),
    from: e.source,
    to: e.target,
  }));

  // Per-step ui entries: rebuilt for every node that had an entry (data.ui
  // defined) or a lifted position (positioned). Position is written back only
  // for positioned nodes, so a synthesized layout never leaks into the document.
  const perStep: Record<string, unknown> = {};
  let anyPerStep = false;
  for (const n of flow.nodes) {
    if (n.data.ui === undefined && !n.data.positioned) continue;
    const entry: Record<string, unknown> = n.data.ui !== undefined ? { ...n.data.ui } : {};
    if (n.data.positioned) entry["position"] = { x: n.position.x, y: n.position.y };
    perStep[n.id] = entry;
    anyPerStep = true;
  }

  const uiOut: Record<string, unknown> = { ...flow.ui };
  const orphanNodes = uiOut["nodes"];
  if (isPlainObject(orphanNodes)) {
    uiOut["nodes"] = { ...orphanNodes, ...perStep };
  } else if (anyPerStep) {
    // Defensive: per-step entries imply the source had a plain-object ui.nodes
    // (so `flow.ui.nodes` is one). This branch cannot normally be reached.
    uiOut["nodes"] = perStep;
  }

  const emitUI = flow.uiPresent || Object.keys(uiOut).length > 0;

  const out: Record<string, unknown> = { ...flow.doc, steps, edges };
  if (emitUI) out["ui"] = uiOut;
  return out as unknown as Definition;
}
