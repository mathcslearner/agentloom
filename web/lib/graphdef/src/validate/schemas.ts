// Build per-step-type config schemas from the published definition schema
// (the offline fallback for the app when GET /v1/plugins is unreachable, and
// the source the graphdef parity test uses). The live plugin catalog's
// root-inlined config schemas are the primary source in the app; this derives
// the same shapes from the definition schema's `Step` oneOf + `$defs`.

import { DEFINITION_SCHEMA } from "../generated/definition.schema.js";
import type { ConfigSchemas, StepConfigSchema } from "./config.js";
import type { Defs, JsonSchema, SchemaNode } from "../schema/schema.js";

function isObj(v: unknown): v is Record<string, unknown> {
  return typeof v === "object" && v !== null && !Array.isArray(v);
}

/**
 * Map each step `type` to its config schema by reading `$defs.Step.oneOf`:
 * each variant binds a `type` const to a `config` `$ref`. Returns schemas whose
 * `defs` is the whole `$defs` map, so `$ref` resolution works for nested types
 * (LLMMessage, OutputFormat, …).
 */
export function fallbackConfigSchemas(): ConfigSchemas {
  const schema = DEFINITION_SCHEMA as Record<string, unknown>;
  const defs = (isObj(schema["$defs"]) ? schema["$defs"] : {}) as Defs;
  const step = defs["Step"];
  const out: ConfigSchemas = {};
  if (typeof step !== "object" || step === null) return out;
  const oneOf = (step as JsonSchema).oneOf;
  if (!Array.isArray(oneOf)) return out;

  for (const variant of oneOf) {
    const props = variant.properties;
    if (!props) continue;
    const typeConst = extractConst(props["type"]);
    const cfg = props["config"];
    if (typeConst === undefined || cfg === undefined) continue;
    const entry: StepConfigSchema = { schema: cfg as SchemaNode, defs };
    out[typeConst] = entry;
  }
  return out;
}

function extractConst(node: SchemaNode | undefined): string | undefined {
  if (node === undefined || node === true || node === false) return undefined;
  return typeof node.const === "string" ? node.const : undefined;
}

/**
 * Normalize a plugin's config_schema (from GET /v1/plugins) into a
 * StepConfigSchema. The plugin form is root-inlined (its properties are the
 * root, its `$defs` self-contained), so its own `$defs` is the resolver.
 */
export function pluginConfigSchema(configSchema: Record<string, unknown>): StepConfigSchema {
  const defs = (isObj(configSchema["$defs"]) ? configSchema["$defs"] : {}) as Defs;
  return { schema: configSchema as SchemaNode, defs };
}
