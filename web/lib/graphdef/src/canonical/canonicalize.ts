// Canonical export (ADR-019 §"Canonical export", ticket 17.6).
//
// `canonicalize(def)` renders a definition value as the exact bytes the backend
// stores — a byte-for-byte mirror of `dag.Encode` (internal/dag/encode.go):
//   - fixed field order (Go struct declaration order, which the invopop
//     reflector preserves in DEFINITION_SCHEMA's `properties` order);
//   - optional fields omitted per Go's `omitempty` (empty string/0/false, empty
//     slice/map, nil pointer/interface — but a non-nil pointer-to-0 numeric and
//     a present-but-empty struct/opaque payload are kept);
//   - the two value-level edge drops from Edge.MarshalJSON (`type:"normal"` and
//     `on_exhausted:"proceed"`);
//   - map keys sorted (params/agents/templates);
//   - the `ui` block emitted with recursively-sorted keys;
//   - no HTML escaping, compact, no trailing newline.
//
// It is proven byte-equal against a Go-produced golden over the whole fixture
// corpus (internal/dag/testdata/canonical.golden.json; test/canonical.test.ts).
// The one modelled asymmetry: Go splices opaque `json.RawMessage` payloads
// (tool `input`, output-format `schema`, approval `payload`/`edit_schema`, map
// `items`, blackboard `value`, validator `config`) verbatim — preserving the
// authored byte spelling — whereas this canonicalizer re-serializes them from
// the parsed value. The two agree for canonically-authored payloads (the corpus
// and any round-tripped canonical document), which is the byte guarantee
// ADR-019 states; a payload authored with, e.g., `1.0` or a `<` escape
// would re-serialize to `1` / `<`. Object key order inside a payload is
// preserved either way (both keep insertion order).

import { DEFINITION_SCHEMA } from "../generated/definition.schema.js";
import { resolveRef, type Defs, type JsonSchema, type SchemaNode } from "../schema/schema.js";
import { isPlainObject } from "../util.js";
import { goNumber, goString } from "./strings.js";

const ROOT = DEFINITION_SCHEMA as unknown as { $ref?: string; $defs: Defs };
const DEFS: Defs = ROOT.$defs;

// Pointer-numeric fields (`*int` / `*float64` in the Go structs), keyed
// `StructName.field`. For these a present value of 0 is a non-nil pointer and is
// KEPT; for a plain (non-pointer) numeric field, 0 is the zero value and is
// omitted. This is the one distinction the JSON Schema does not encode, so it is
// pinned here with the Go source (grep `\*(int|float64)` in internal/dag). A
// drift test (test/canonical.test.ts) re-derives coverage from the schema.
const POINTER_NUMERIC = new Set<string>([
  "Definition.budget_usd",
  "StepBudget.max_usd",
  "LLMConfig.temperature",
  "PlannerConfig.temperature",
  "AgentConfig.temperature",
  "AgentDef.temperature",
  "ModelFallback.at_budget_fraction",
  "BlackboardWriteConfig.expected_version",
  "CompactionStrategy.n",
  "CompactionStrategy.min_tokens",
  "CompactionStrategy.max_tokens",
  "ContextSource.top_k",
  "ContextSource.max_tokens",
  "ContextSource.priority",
  "ContextSpec.budget_tokens",
  "ExpansionPolicy.max_added_steps",
  "ExpansionPolicy.max_total_steps",
  "ExpansionPolicy.max_expansions",
  "ExpansionPolicy.max_depth",
]);

function objSchema(node: SchemaNode): JsonSchema | null {
  const s = resolveRef(node, DEFS);
  return s === true || s === false ? null : s;
}

/** The `$defs` name a node references, if it is a direct `$ref`. */
function refName(node: SchemaNode): string | undefined {
  if (node === true || node === false) return undefined;
  const m = node.$ref?.match(/^#\/\$defs\/(.+)$/);
  return m ? m[1] : undefined;
}

/** Generic canonical JSON for opaque (`json.RawMessage`) subtrees and unknown
 *  passthrough keys: objects keep insertion order (matching Go's `json.Compact`
 *  of the authored bytes); strings/numbers use the Go scalar rules. */
function goValue(v: unknown): string {
  if (v === null || v === undefined) return "null";
  if (typeof v === "string") return goString(v);
  if (typeof v === "number") return goNumber(v);
  if (typeof v === "boolean") return v ? "true" : "false";
  if (Array.isArray(v)) return "[" + v.map(goValue).join(",") + "]";
  if (isPlainObject(v)) {
    return "{" + Object.keys(v).map((k) => goString(k) + ":" + goValue(v[k])).join(",") + "}";
  }
  return "null";
}

/** The `ui` block: identical to {@link goValue} but with recursively-sorted
 *  object keys (ADR-019: canonical `ui` is emitted sorted). */
function uiValue(v: unknown): string {
  if (Array.isArray(v)) return "[" + v.map(uiValue).join(",") + "]";
  if (isPlainObject(v)) {
    return "{" + Object.keys(v).sort().map((k) => goString(k) + ":" + uiValue(v[k])).join(",") + "}";
  }
  return goValue(v);
}

/** Does Go's `omitempty` drop this present value for the given field? */
function omit(structName: string, field: string, child: SchemaNode, v: unknown): boolean {
  const s = resolveRef(child, DEFS);
  if (s === true || s === false) return false; // opaque / any: present ⇒ kept
  const t = s.type;
  if (t === "object") {
    const isMap = s.properties === undefined && s.additionalProperties !== undefined && s.additionalProperties !== false;
    // A map is omitempty-empty when {}; a pointer struct (or the `config`
    // interface) keeps a present, even empty, object.
    return isMap && isPlainObject(v) && Object.keys(v).length === 0;
  }
  if (t === "array") return Array.isArray(v) && v.length === 0;
  if (t === "boolean") return v === false;
  if (t === "integer" || t === "number") {
    return !POINTER_NUMERIC.has(`${structName}.${field}`) && v === 0;
  }
  // string, enum ($ref to a string enum), or anything else scalar
  return v === "";
}

/** Emit an object against a named struct schema, in property (struct-field)
 *  order, applying omitempty and the field-level special cases. */
function emitStruct(value: Record<string, unknown>, structName: string): string {
  const schema = objSchema(DEFS[structName] as SchemaNode);
  const props = schema?.properties ?? {};
  const required = new Set(schema?.required ?? []);
  const parts: string[] = [];
  const seen = new Set<string>();

  for (const k of Object.keys(props)) {
    seen.add(k);

    // Step.config binds to the per-type config schema via the oneOf variant.
    if (structName === "Step" && k === "config") {
      if (!("config" in value)) continue; // interface omitempty: absent ⇒ nil ⇒ dropped
      parts.push(goString("config") + ":" + emitStepConfig(value));
      continue;
    }

    const child = props[k]!;
    const present = k in value;
    if (!present) {
      // A required array (Definition steps/edges, Template steps) is always an
      // array even when absent, so export never yields an invalid document.
      if (required.has(k) && objSchema(child)?.type === "array") parts.push(goString(k) + ":[]");
      continue;
    }
    const v = value[k];
    if (!required.has(k) && omit(structName, k, child, v)) continue;
    // Edge value-level drops (Edge.MarshalJSON).
    if (structName === "Edge" && k === "type" && v === "normal") continue;
    if (structName === "Edge" && k === "on_exhausted" && v === "proceed") continue;

    parts.push(goString(k) + ":" + emitValue(v, child, structName, k));
  }

  // Passthrough: keys the strict struct does not know (only present in
  // imported-but-not-yet-valid documents; the backend rejects them). Appended in
  // sorted order so export stays deterministic; never present in the corpus.
  for (const k of Object.keys(value).sort()) {
    if (seen.has(k)) continue;
    parts.push(goString(k) + ":" + goValue(value[k]));
  }

  return "{" + parts.join(",") + "}";
}

/** Emit a Step's `config` against the schema bound to its `type`. */
function emitStepConfig(step: Record<string, unknown>): string {
  const cfg = step["config"];
  const type = step["type"];
  const variants = (objSchema(DEFS["Step"] as SchemaNode)?.oneOf ?? []) as JsonSchema[];
  for (const variant of variants) {
    const constType = (variant.properties?.["type"] as JsonSchema | undefined)?.const;
    if (constType === type) {
      const name = refName(variant.properties?.["config"] as SchemaNode);
      if (name && isPlainObject(cfg)) return emitStruct(cfg, name);
      break;
    }
  }
  return goValue(cfg); // unknown step type: emit the config verbatim
}

/** Emit a value against its schema node, dispatching by shape. */
function emitValue(value: unknown, node: SchemaNode, ownerStruct: string, field: string): string {
  if (ownerStruct === "Definition" && field === "ui") return uiValue(value);

  const s = resolveRef(node, DEFS);
  if (s === true || s === false) return goValue(value); // opaque payload

  if (s.oneOf && isPlainObject(value)) return emitStruct(value, "Step");

  const t = s.type;
  if (t === "object" && isPlainObject(value)) {
    const isMap = s.properties === undefined && s.additionalProperties !== undefined && s.additionalProperties !== false;
    if (isMap) {
      const addl = s.additionalProperties as SchemaNode;
      const keys = Object.keys(value).sort();
      const parts = keys.map((k) => goString(k) + ":" + emitValue(value[k], addl, "", ""));
      return "{" + parts.join(",") + "}";
    }
    const name = refName(node);
    if (name) return emitStruct(value, name);
    return goValue(value); // an inline object schema with no name: treat as opaque
  }

  if (t === "array" && Array.isArray(value)) {
    const items = s.items;
    if (items === undefined) return goValue(value);
    return "[" + value.map((el) => emitValue(el, items, ownerStruct, field)).join(",") + "]";
  }

  // Scalar leaf (string/enum/integer/number/boolean).
  return goValue(value);
}

/** Render a definition value as canonical JSON bytes, byte-for-byte identical to
 *  the backend's `dag.Encode` for a canonically-authored document. */
export function canonicalize(def: unknown): string {
  if (!isPlainObject(def)) return goValue(def);
  return emitStruct(def, "Definition");
}
