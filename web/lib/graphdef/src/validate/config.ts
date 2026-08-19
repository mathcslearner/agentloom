// Backward-compatible façade (17.4 API) over the full definition validator
// (17.5, definition.ts). `validateStepConfigs` now delegates to
// `validateDefinition` and returns only the single-step-local config findings
// (the `steps[i].config…` subset), so 17.4 callers and its parity test keep
// working while the app moves to `validateDefinition` for whole-graph problems.
//
// The ConfigSchemas types stay here because the app's schema-driven config
// panel (SchemaForm) still consumes per-step-type config schemas from the
// plugin catalog; validation itself no longer needs them (the full validator
// uses the embedded definition schema for shape).

import type { Definition } from "../generated/definition.js";
import type { Defs, SchemaNode } from "../schema/schema.js";
import type { Issue } from "./issue.js";
import { validateDefinition } from "./definition.js";

/** The config schema for a step type: the root schema plus its `$defs`. */
export interface StepConfigSchema {
  schema: SchemaNode;
  defs: Defs;
}

/** Per-step-type config schemas (from the plugin catalog or the fallback). */
export type ConfigSchemas = Partial<Record<string, StepConfigSchema>>;

const CONFIG_PATH = /^steps\[\d+\]\.config(\.|\[|$)/;

/**
 * Validate every step's config in a definition (17.4 compatibility). `schemas`
 * is accepted for signature compatibility but unused — the full validator
 * (validateDefinition) carries the shape rules internally. Returns the
 * config-scoped subset of the full verdict.
 */
export function validateStepConfigs(def: unknown, _schemas?: ConfigSchemas): Issue[] {
  return validateDefinition(def).filter((i) => CONFIG_PATH.test(i.path));
}

export type { Definition };
