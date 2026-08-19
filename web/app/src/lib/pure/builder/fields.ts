// The schema-driven field planner (ticket 17.4): turn a config JSON Schema
// (root-inlined, from a plugin's config_schema or the fallback) into an ordered
// list of FieldPlans the SchemaForm renders. Pure — no React. Widget selection
// is: a per-field hint first (model/tool/retriever/agent/template/prompt/
// duration/hidden), then the schema's own shape (enum → select, primitive →
// input, array → list, nested object → sub-form, `true` → JSON editor).

import type { StepType } from "@agentloom/graphdef";
import { resolveRef, type Defs, type JsonSchema, type SchemaNode } from "@agentloom/graphdef";
import { fieldHint, humanizeField, isPromptField, isRequiredField, type FieldHint } from "./hints.js";

/** The widget a field renders as. */
export type Widget =
  | "text"
  | "textarea"
  | "prompt"
  | "number"
  | "integer"
  | "boolean"
  | "enum"
  | "string-list"
  | "object"
  | "object-list"
  | "json"
  | "model"
  | "picker"
  | "duration";

/** A planned form field. */
export interface FieldPlan {
  name: string;
  label: string;
  required: boolean;
  widget: Widget;
  hint?: FieldHint;
  /** The resolved property schema. */
  schema: JsonSchema | true;
  /** Enum values, when the widget is "enum". */
  enumValues?: string[];
  /** For array widgets, the resolved element schema. */
  itemSchema?: SchemaNode;
  /** For object/object-list widgets, the resolved (or element) object's fields. */
  fields?: FieldPlan[];
}

/** Options controlling how a schema is planned. */
export interface PlanOptions {
  /** The step type, for required-ness and per-field hints. */
  stepType: StepType;
  /** The `$defs` resolver for `$ref`. */
  defs: Defs;
  /** True when planning a nested object (suppresses top-level-only hints). */
  nested?: boolean;
}

function asObject(node: SchemaNode, defs: Defs): JsonSchema | true {
  const r = resolveRef(node, defs);
  return r === false ? true : r;
}

/** Plan the fields of a root (or nested) object schema. */
export function planFields(node: SchemaNode, opts: PlanOptions): FieldPlan[] {
  const schema = asObject(node, opts.defs);
  if (schema === true || schema.properties === undefined) return [];
  const props = schema.properties;
  return Object.keys(props).map((name) => planField(name, props[name]!, opts));
}

function planField(name: string, raw: SchemaNode, opts: PlanOptions): FieldPlan {
  const schema = asObject(raw, opts.defs);
  const required = !opts.nested && isRequiredField(opts.stepType, name);
  const hint = opts.nested ? undefined : fieldHint(opts.stepType, name);
  const base: FieldPlan = { name, label: humanizeField(name), required, widget: "json", hint, schema };

  if (hint === "hidden") return { ...base, widget: "json", hint };
  if (hint === "model") return { ...base, widget: "model" };
  if (hint === "tool" || hint === "retriever" || hint === "agent" || hint === "template") return { ...base, widget: "picker" };
  if (hint === "duration") return { ...base, widget: "text" };
  if (hint === "prompt") return { ...base, widget: "prompt" };

  if (schema === true) return { ...base, widget: "json" };

  if (Array.isArray(schema.enum)) {
    return { ...base, widget: "enum", enumValues: schema.enum };
  }

  switch (schema.type) {
    case "string":
      return { ...base, widget: isPromptField(name) ? "prompt" : "text" };
    case "integer":
      return { ...base, widget: "integer" };
    case "number":
      return { ...base, widget: "number" };
    case "boolean":
      return { ...base, widget: "boolean" };
    case "array": {
      const item = schema.items ?? true;
      const resolved = asObject(item, opts.defs);
      if (resolved !== true && resolved.type === "string" && !resolved.enum) {
        return { ...base, widget: "string-list", itemSchema: item };
      }
      if (resolved !== true && (resolved.type === "object" || resolved.properties)) {
        return {
          ...base,
          widget: "object-list",
          itemSchema: item,
          fields: planFields(resolved, { ...opts, nested: true }),
        };
      }
      return { ...base, widget: "json", itemSchema: item };
    }
    case "object":
      return { ...base, widget: "object", fields: planFields(schema, { ...opts, nested: true }) };
    default:
      // Objects declared only via `properties` (no explicit "object" type).
      if (schema.properties) {
        return { ...base, widget: "object", fields: planFields(schema, { ...opts, nested: true }) };
      }
      return { ...base, widget: "json" };
  }
}
