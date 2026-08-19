// A small JSON-Schema (draft 2020-12) subset walker (ADR-019 §"Validation
// parity" point 1: "shape is checked against the same published JSON Schema at
// runtime, so shape errors match the contract"). It covers exactly the
// vocabulary the invopop reflector emits for the dag step-config structs:
// object/properties/`additionalProperties:false`/`$ref`/`items`/enum and the
// `true` (any) schema. It is deliberately NOT a full validator (no ajv): the
// config schemas use only this subset, it needs no eval (CSP-safe), and its
// messages mirror the backend's strict codec (internal/dag/decode.go) so a
// client shape finding and a server decode error name the same problem.
//
// Enum values are deliberately NOT checked here — enum semantics differ per
// field (join `mode` is a codeless decode error, `output_format.type` is a
// coded validate error), so the config validator (validate/config.ts) owns
// them. This walker reports only: wrong JSON type, not-an-object, and unknown
// fields (additionalProperties:false), which are the structural, code-free
// decode findings.

import { err, type Issue } from "../validate/issue.js";
import { isPlainObject } from "../util.js";

/** A JSON Schema node (the subset used). `true` means the any-schema. */
export type SchemaNode = boolean | JsonSchema;

/** The object form of a JSON Schema node. */
export interface JsonSchema {
  $ref?: string;
  $defs?: Record<string, SchemaNode>;
  type?: string;
  properties?: Record<string, SchemaNode>;
  required?: string[];
  additionalProperties?: SchemaNode;
  enum?: string[];
  const?: string;
  items?: SchemaNode;
  oneOf?: JsonSchema[];
  title?: string;
  description?: string;
}

/** A `$defs` map for resolving `$ref`. */
export type Defs = Record<string, SchemaNode>;

/** Resolve a local `#/$defs/Name` reference against a defs map. */
export function resolveRef(node: SchemaNode, defs: Defs): SchemaNode {
  let cur = node;
  // Follow chains of $ref (defensive; the emitter produces at most one hop).
  for (let i = 0; i < 16 && cur !== true && cur !== false && cur.$ref; i += 1) {
    const name = cur.$ref.replace(/^#\/\$defs\//, "");
    const target = defs[name];
    if (target === undefined) return cur; // dangling ref: treat as any
    cur = target;
  }
  return cur;
}

// The JSON "type" name a value carries, matching the backend's jsonTypeOf.
function jsonTypeOf(v: unknown): string {
  if (v === null) return "null";
  if (Array.isArray(v)) return "array";
  switch (typeof v) {
    case "object":
      return "object";
    case "string":
      return "string";
    case "boolean":
      return "boolean";
    case "number":
      // The backend distinguishes integer vs number by the target Go type, not
      // the value; jsonTypeOf itself returns "number". Callers that expect an
      // integer compare against the schema's declared type below.
      return "number";
    default:
      return "unknown";
  }
}

// Does a value satisfy a schema's declared primitive `type`? Mirrors the
// backend decode messages: a non-integer where an integer is wanted, etc. The
// backend reports type errors from json.Unmarshal, which names the wanted JSON
// type (string/integer/number/boolean/object/array).
function typeMatches(schemaType: string, v: unknown): boolean {
  switch (schemaType) {
    case "integer":
      return typeof v === "number" && Number.isInteger(v);
    case "number":
      return typeof v === "number" && Number.isFinite(v);
    case "string":
      return typeof v === "string";
    case "boolean":
      return typeof v === "boolean";
    case "object":
      return isPlainObject(v);
    case "array":
      return Array.isArray(v);
    default:
      return true; // unknown declared type: don't judge
  }
}

/**
 * Check a value against a schema node, appending shape issues to `out`. Codeless
 * (severity error), with messages mirroring the backend's strict codec. Enum and
 * semantic rules are not checked here (see the module comment).
 */
export function checkShape(value: unknown, node: SchemaNode, defs: Defs, path: string, out: Issue[]): void {
  const schema = resolveRef(node, defs);
  if (schema === true || schema === false) return; // any-schema (json.RawMessage) or never
  if (schema.oneOf) return; // handled by the discriminated Step walk, not here

  const t = schema.type;
  if (t !== undefined && !typeMatches(t, value)) {
    out.push(err("", path, `expected ${t}, got ${jsonTypeOf(value)}`));
    return; // shape is wrong; don't descend
  }

  if (t === "object" || (schema.properties && isPlainObject(value))) {
    if (!isPlainObject(value)) return;
    const props = schema.properties ?? {};
    const additional = schema.additionalProperties;
    // Report unknown fields when additionalProperties is false — the backend's
    // DisallowUnknownFields. Keys are walked in sorted order (matching the
    // backend's checkUnknownFields), so multi-error output ordering agrees.
    for (const k of Object.keys(value).sort()) {
      const child = props[k];
      if (child === undefined) {
        if (additional === false || additional === undefined) {
          out.push(err("", `${path}.${k}`, "unknown field"));
        } else if (additional !== true) {
          // additionalProperties is a schema: children validate against it.
          checkShape(value[k], additional, defs, `${path}.${k}`, out);
        }
        continue;
      }
      checkShape(value[k], child, defs, `${path}.${k}`, out);
    }
    return;
  }

  if (t === "array" && Array.isArray(value) && schema.items !== undefined) {
    value.forEach((item, i) => checkShape(item, schema.items as SchemaNode, defs, `${path}[${i}]`, out));
  }
}
