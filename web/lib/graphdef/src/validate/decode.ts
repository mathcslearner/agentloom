// The decode stage — the client mirror of internal/dag/decode.go. It reproduces
// the strict codec's accept/reject decision: a non-object root, a bad
// schema_version, missing structural-required fields (schema_version/name/steps/
// edges, a step's id/type, an edge's from/to, a param's type, a template's
// steps), unknown fields anywhere (additionalProperties:false), wrong JSON
// types, and the decode-enforced closed enums. Findings are codeless (severity
// error), matching the backend's *dag.DecodeError. When this stage finds any
// error the validate stage does not run (Go returns decode errors alone), so
// the two stages never both fire on one fixture.
//
// It is precise, not exhaustive-by-order: the goal is that every decode-failed
// fixture rejects here and no decode-clean fixture does — the parity test
// asserts accept/reject over the whole corpus and exact code+path over the
// validate stage.

import { DEFINITION_SCHEMA } from "../generated/definition.schema.js";
import { isPlainObject } from "../util.js";
import { resolveRef, type Defs, type JsonSchema, type SchemaNode } from "../schema/schema.js";
import { err, type Issue } from "./issue.js";
import { SCHEMA_VERSION } from "./limits.js";

const ROOT = DEFINITION_SCHEMA as unknown as JsonSchema;
const DEFS = (ROOT.$defs ?? {}) as Defs;

// Enums the codec does NOT enforce — they carry a ValidationCode at the validate
// stage instead (output_format.type/mode, loop on_exhausted / no_progress.policy).
const VALIDATE_OWNED_ENUM_DEFS = new Set(["OutputFormatType", "OutputFormatMode", "ExhaustPolicy"]);

// Step-envelope field names (dag.Step) — the Step JSON Schema is the loose
// {type, config} oneOf, so the envelope's known-field set is hardcoded here.
const KNOWN_STEP_FIELDS = new Set([
  "id",
  "type",
  "config",
  "retry",
  "timeout",
  "cache",
  "budget",
  "validation",
  "blackboard",
  "context",
]);
// Step-envelope block → its $def, walked by the schema checker.
const ENVELOPE_DEFS: Record<string, string> = {
  retry: "RetryPolicy",
  cache: "CachePolicy",
  budget: "StepBudget",
  validation: "ValidationPolicy",
  blackboard: "BlackboardPolicy",
  context: "ContextSpec",
};

function jsonTypeOf(v: unknown): string {
  if (v === null) return "null";
  if (Array.isArray(v)) return "array";
  return typeof v;
}

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
      return true;
  }
}

// checkNode walks a value against a schema node (resolving $ref), reporting type
// errors, unknown fields (additionalProperties:false), array-item errors, and
// decode-enforced enum misses. It intentionally does NOT enforce `required`
// (that split — some schema `required` are validate rules — is handled by the
// callers) and skips validate-owned enums.
function checkNode(value: unknown, node: SchemaNode, path: string, out: Issue[], refName?: string): void {
  const schema = resolveRef(node, DEFS);
  if (schema === true || schema === false) return; // any / never
  if (schema.oneOf) return; // discriminated Step — walked by checkStep

  if (schema.enum && !VALIDATE_OWNED_ENUM_DEFS.has(refName ?? "")) {
    if (typeof value === "string" && !schema.enum.includes(value)) {
      out.push(err("", path, `unknown value ${JSON.stringify(value)} (expected one of: ${schema.enum.join(", ")})`));
      return;
    }
  }

  const t = schema.type;
  if (t !== undefined && !typeMatches(t, value)) {
    out.push(err("", path, `expected ${t}, got ${jsonTypeOf(value)}`));
    return;
  }

  if (t === "object" || (schema.properties && isPlainObject(value))) {
    if (!isPlainObject(value)) return;
    const props = schema.properties ?? {};
    const additional = schema.additionalProperties;
    for (const k of Object.keys(value).sort()) {
      const child = props[k];
      if (child === undefined) {
        if (additional === false || additional === undefined) {
          out.push(err("", `${path}.${k}`, "unknown field"));
        } else if (additional !== true) {
          checkNode(value[k], additional, `${path}.${k}`, out, refNameOf(additional));
        }
        continue;
      }
      checkNode(value[k], child, `${path}.${k}`, out, refNameOf(child));
    }
    return;
  }

  if (t === "array" && Array.isArray(value) && schema.items !== undefined) {
    value.forEach((item, i) => checkNode(item, schema.items as SchemaNode, `${path}[${i}]`, out, refNameOf(schema.items!)));
  }
}

function refNameOf(node: SchemaNode): string | undefined {
  if (node !== true && node !== false && node.$ref) {
    return node.$ref.replace(/^#\/\$defs\//, "");
  }
  return undefined;
}

function stepConfigRef(type: string): SchemaNode | undefined {
  const step = DEFS["Step"];
  if (step === true || step === false || step === undefined || !step.oneOf) return undefined;
  for (const variant of step.oneOf) {
    const props = variant.properties;
    if (!props) continue;
    const typeConst = props["type"];
    const c = typeConst !== undefined && typeConst !== true && typeConst !== false ? typeConst.const : undefined;
    if (c === type) return props["config"];
  }
  return undefined;
}

const KNOWN_STEP_TYPES = new Set(
  (() => {
    const step = DEFS["Step"];
    if (step === true || step === false || step === undefined || !step.oneOf) return [];
    return step.oneOf
      .map((v) => {
        const tc = v.properties?.["type"];
        return tc !== undefined && tc !== true && tc !== false ? tc.const : undefined;
      })
      .filter((c): c is string => typeof c === "string");
  })(),
);

// checkStep walks one step's identity, type, config, and envelope blocks
// (decode.go decodeStep): id/type required strings; type known; config walked
// only when the type is known; every envelope block walked against its $def;
// unknown envelope fields flagged.
function checkStep(step: unknown, path: string, out: Issue[]): void {
  if (!isPlainObject(step)) {
    out.push(err("", path, `expected object, got ${jsonTypeOf(step)}`));
    return;
  }
  const id = step["id"];
  if (id === undefined) out.push(err("", `${path}.id`, "required field is missing"));
  else if (typeof id !== "string") out.push(err("", `${path}.id`, `expected string, got ${jsonTypeOf(id)}`));

  let typeKnown = false;
  const type = step["type"];
  if (type === undefined) {
    out.push(err("", `${path}.type`, "required field is missing"));
  } else if (typeof type !== "string") {
    out.push(err("", `${path}.type`, `expected string, got ${jsonTypeOf(type)}`));
  } else if (!KNOWN_STEP_TYPES.has(type)) {
    out.push(err("", `${path}.type`, `unknown step type ${JSON.stringify(type)}`));
  } else {
    typeKnown = true;
  }

  if (step["config"] !== undefined && typeKnown) {
    const ref = stepConfigRef(type as string);
    if (ref !== undefined) checkNode(step["config"], ref, `${path}.config`, out, refNameOf(ref));
  }
  for (const [field, defName] of Object.entries(ENVELOPE_DEFS)) {
    if (step[field] !== undefined) {
      checkNode(step[field], { $ref: `#/$defs/${defName}` }, `${path}.${field}`, out, defName);
    }
  }
  if (step["timeout"] !== undefined && typeof step["timeout"] !== "string") {
    out.push(err("", `${path}.timeout`, `expected string, got ${jsonTypeOf(step["timeout"])}`));
  }
  for (const k of Object.keys(step).sort()) {
    if (!KNOWN_STEP_FIELDS.has(k)) out.push(err("", `${path}.${k}`, "unknown field"));
  }
}

function checkEdge(edge: unknown, path: string, out: Issue[]): void {
  if (!isPlainObject(edge)) {
    out.push(err("", path, `expected object, got ${jsonTypeOf(edge)}`));
    return;
  }
  for (const key of ["from", "to"]) {
    if (edge[key] === undefined) out.push(err("", `${path}.${key}`, "required field is missing"));
  }
  checkNode(edge, { $ref: "#/$defs/Edge" }, path, out, "Edge");
}

function checkParams(params: unknown, out: Issue[]): void {
  if (!isPlainObject(params)) {
    out.push(err("", "params", `expected object, got ${jsonTypeOf(params)}`));
    return;
  }
  for (const key of Object.keys(params).sort()) {
    const p = `params.${key}`;
    const spec = params[key];
    if (!isPlainObject(spec)) {
      out.push(err("", p, `expected object, got ${jsonTypeOf(spec)}`));
      out.push(err("", `${p}.type`, "required field is missing"));
      continue;
    }
    checkNode(spec, { $ref: "#/$defs/ParamSpec" }, p, out, "ParamSpec");
    if (spec["type"] === undefined) out.push(err("", `${p}.type`, "required field is missing"));
  }
}

function checkStepsArray(steps: unknown, base: string, out: Issue[]): boolean {
  if (!Array.isArray(steps)) {
    out.push(err("", base, `expected array, got ${jsonTypeOf(steps)}`));
    return false;
  }
  steps.forEach((s, i) => checkStep(s, `${base}[${i}]`, out));
  return true;
}

function checkEdgesArray(edges: unknown, base: string, out: Issue[]): boolean {
  if (!Array.isArray(edges)) {
    out.push(err("", base, `expected array, got ${jsonTypeOf(edges)}`));
    return false;
  }
  edges.forEach((e, i) => checkEdge(e, `${base}[${i}]`, out));
  return true;
}

function checkNamedGraphMap(value: unknown, base: string, kind: "template" | "agent", out: Issue[]): void {
  if (!isPlainObject(value)) {
    out.push(err("", base, `expected object, got ${jsonTypeOf(value)}`));
    return;
  }
  for (const name of Object.keys(value).sort()) {
    const p = `${base}.${name}`;
    const entry = value[name];
    if (kind === "template") {
      if (!isPlainObject(entry)) {
        out.push(err("", p, `expected object, got ${jsonTypeOf(entry)}`));
        continue;
      }
      if (entry["steps"] === undefined) out.push(err("", `${p}.steps`, "required field is missing"));
      else checkStepsArray(entry["steps"], `${p}.steps`, out);
      if (entry["edges"] !== undefined) checkEdgesArray(entry["edges"], `${p}.edges`, out);
      for (const k of Object.keys(entry).sort()) {
        if (k !== "steps" && k !== "edges") out.push(err("", `${p}.${k}`, "unknown field"));
      }
    } else {
      checkNode(entry, { $ref: "#/$defs/AgentDef" }, p, out, "AgentDef");
    }
  }
}

const KNOWN_TOP_FIELDS = new Set([
  "schema_version",
  "name",
  "description",
  "on_failure",
  "max_wall_clock",
  "budget_usd",
  "on_budget_exceeded",
  "expansion",
  "templates",
  "agents",
  "params",
  "steps",
  "edges",
  "ui",
]);

/** Run the decode stage; returns codeless shape/enum/required findings. */
export function runDecodeStage(value: unknown): Issue[] {
  const out: Issue[] = [];
  if (!isPlainObject(value)) {
    out.push(err("", "", `expected object, got ${jsonTypeOf(value)}`));
    return out;
  }

  // schema_version short-circuits, exactly like decode.go.
  const ver = value["schema_version"];
  if (ver === undefined) {
    out.push(err("", "schema_version", "required field is missing"));
    return out;
  }
  if (typeof ver !== "number" || !Number.isInteger(ver)) {
    out.push(err("", "schema_version", `expected integer, got ${typeof ver === "number" ? ver : jsonTypeOf(ver)}`));
    return out;
  }
  if (ver !== SCHEMA_VERSION) {
    out.push(err("", "schema_version", `unsupported schema_version ${ver} (this engine supports ${SCHEMA_VERSION})`));
    return out;
  }

  if (value["name"] === undefined) out.push(err("", "name", "required field is missing"));
  else if (typeof value["name"] !== "string") out.push(err("", "name", `expected string, got ${jsonTypeOf(value["name"])}`));

  if (value["description"] !== undefined && typeof value["description"] !== "string") {
    out.push(err("", "description", `expected string, got ${jsonTypeOf(value["description"])}`));
  }
  if (value["on_failure"] !== undefined) checkNode(value["on_failure"], { $ref: "#/$defs/FailurePolicy" }, "on_failure", out, "FailurePolicy");
  if (value["max_wall_clock"] !== undefined && typeof value["max_wall_clock"] !== "string") {
    out.push(err("", "max_wall_clock", `expected string, got ${jsonTypeOf(value["max_wall_clock"])}`));
  }
  if (value["budget_usd"] !== undefined && typeof value["budget_usd"] !== "number") {
    out.push(err("", "budget_usd", `expected number, got ${jsonTypeOf(value["budget_usd"])}`));
  }
  if (value["on_budget_exceeded"] !== undefined) checkNode(value["on_budget_exceeded"], { $ref: "#/$defs/BudgetPolicy" }, "on_budget_exceeded", out, "BudgetPolicy");
  if (value["expansion"] !== undefined) checkNode(value["expansion"], { $ref: "#/$defs/ExpansionPolicy" }, "expansion", out, "ExpansionPolicy");
  if (value["templates"] !== undefined) checkNamedGraphMap(value["templates"], "templates", "template", out);
  if (value["agents"] !== undefined) checkNamedGraphMap(value["agents"], "agents", "agent", out);
  if (value["params"] !== undefined) checkParams(value["params"], out);

  if (value["steps"] === undefined) out.push(err("", "steps", "required field is missing"));
  else checkStepsArray(value["steps"], "steps", out);
  if (value["edges"] === undefined) out.push(err("", "edges", "required field is missing"));
  else checkEdgesArray(value["edges"], "edges", out);

  if (value["ui"] !== undefined && !isPlainObject(value["ui"])) {
    out.push(err("", "ui", `expected object, got ${jsonTypeOf(value["ui"])}`));
  }

  for (const k of Object.keys(value).sort()) {
    if (!KNOWN_TOP_FIELDS.has(k)) out.push(err("", k, "unknown field"));
  }
  return out;
}
