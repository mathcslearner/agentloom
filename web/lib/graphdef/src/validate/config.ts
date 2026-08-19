// The client-side config validator (ADR-019 §"Validation parity" point 4:
// "config-level rules come from the plugin JSON Schemas served by GET
// /v1/plugins"). It mirrors the backend's single-step-local config rules
// (internal/dag/validate.go checkStepConfig / checkLLMConfig / checkOutputFormat
// / checkHumanApproval), reporting the backend's ValidationCode vocabulary and
// path grammar so a client verdict and a server verdict name the same problem.
//
// Scope (17.4): single-step-local config rules only. Cross-section rules (agent
// role merge, run expansion caps), envelope-block bound checks, graph/CEL rules,
// and full accept/reject parity over the whole corpus are 17.5 (ADR-019). Shape
// (wrong type, unknown field, not-object) comes from the JSON-Schema subset
// walker (schema/schema.ts); enum semantics are owned here per field.

import type { Definition, Step } from "../generated/definition.js";
import { isPlainObject } from "../util.js";
import { checkShape, type Defs, type SchemaNode } from "../schema/schema.js";
import { Code, err, type Issue } from "./issue.js";
import { MAX_APPROVAL_TIMEOUT_SECONDS, parseGoDuration } from "./duration.js";

/** The config schema for a step type: the root schema plus its `$defs`. */
export interface StepConfigSchema {
  schema: SchemaNode;
  defs: Defs;
}

/** Per-step-type config schemas (from the plugin catalog or the fallback). */
export type ConfigSchemas = Partial<Record<string, StepConfigSchema>>;

const REQUIRED_MISSING = "required field is missing";

function hasTemplate(v: unknown): boolean {
  return typeof v === "string" && v.includes("${{");
}

function isNonEmptyString(v: unknown): v is string {
  return typeof v === "string" && v !== "";
}

// The config record of a step, always an object for our purposes (a non-object
// config is a shape error the walker already reported).
function configOf(step: Step): Record<string, unknown> {
  const c = (step as { config?: unknown }).config;
  return isPlainObject(c) ? c : {};
}

/**
 * Validate every step's config in a definition, returning the union of shape
 * findings (from the JSON-Schema subset) and single-step-local semantic
 * findings. `schemas` supplies the per-step-type config schema; a step type with
 * no schema is shape-skipped (semantics still run).
 */
export function validateStepConfigs(def: unknown, schemas: ConfigSchemas): Issue[] {
  const out: Issue[] = [];
  if (!isPlainObject(def)) return out;
  const steps = def["steps"];
  if (!Array.isArray(steps)) return out;

  steps.forEach((raw, i) => {
    if (!isPlainObject(raw)) return;
    const type = raw["type"];
    if (typeof type !== "string") return;
    const path = `steps[${i}].config`;
    const cfg = isPlainObject(raw["config"]) ? (raw["config"] as Record<string, unknown>) : undefined;

    // Shape (structural, code-free) — wrong type, unknown field, not-object.
    const s = schemas[type];
    if (s && raw["config"] !== undefined) {
      checkShape(raw["config"], s.schema, s.defs, path, out);
    }

    // Semantic rules per step type. Run on the (possibly empty) config object.
    const c = cfg ?? {};
    switch (type) {
      case "llm":
        checkModelCall(c, path, out);
        break;
      case "planner":
        checkModelCall(c, path, out);
        break;
      case "agent":
        checkAgent(c, path, def, out);
        break;
      case "retrieve":
        requireField(c, "retriever", path, out);
        requireField(c, "query", path, out);
        break;
      case "map":
        requireField(c, "items", path, out);
        requireField(c, "body", path, out);
        break;
      case "join":
        checkJoin(c, path, out);
        break;
      case "human_approval":
        checkHumanApproval(c, path, out);
        break;
      case "tool":
        requireField(c, "tool", path, out);
        break;
      case "sleep":
        checkSleep(c, path, out);
        break;
      case "fail_n_times":
        checkFailNTimes(c, path, out);
        break;
      case "counter":
        requireField(c, "path", path, out);
        break;
      case "effectful_echo":
        requireField(c, "path", path, out);
        break;
      case "blackboard_write":
        requireField(c, "key", path, out);
        requireField(c, "value", path, out);
        break;
      default:
        break;
    }
  });

  return out;
}

// require a non-empty field, matching checkStepConfig's "required field is
// missing" at `steps[i].config.<field>`.
function requireField(cfg: Record<string, unknown>, field: string, path: string, out: Issue[]): void {
  if (!isNonEmptyString(cfg[field]) && !hasContent(cfg[field])) {
    out.push(err(Code.ConfigFieldRequired, `${path}.${field}`, REQUIRED_MISSING));
  }
}

// hasContent treats a present non-string (object/array/number/bool) as filled —
// e.g. map.items / blackboard_write.value are raw JSON, required-when-present.
function hasContent(v: unknown): boolean {
  if (v === undefined || v === null) return false;
  if (typeof v === "string") return v !== "";
  return true;
}

// checkModelCall mirrors checkLLMConfig (llm + planner): a model, and exactly
// one of prompt|messages.
function checkModelCall(cfg: Record<string, unknown>, path: string, out: Issue[]): void {
  if (!isNonEmptyString(cfg["model"])) {
    out.push(err(Code.ConfigFieldRequired, `${path}.model`, REQUIRED_MISSING));
  }
  const hasPrompt = isNonEmptyString(cfg["prompt"]);
  const hasMessages = Array.isArray(cfg["messages"]) && (cfg["messages"] as unknown[]).length > 0;
  if (hasPrompt && hasMessages) {
    out.push(err(Code.ConfigFieldConflict, path, `"prompt" and "messages" are mutually exclusive`));
  } else if (!hasPrompt && !hasMessages) {
    out.push(err(Code.ConfigFieldRequired, path, `exactly one of "prompt" or "messages" is required`));
  }
  checkMessages(cfg["messages"], path, out);
  checkOutputFormat(cfg["output_format"], path, out);
}

function checkMessages(messages: unknown, path: string, out: Issue[]): void {
  if (!Array.isArray(messages)) return;
  messages.forEach((m, i) => {
    if (!isPlainObject(m)) return;
    const mp = `${path}.messages[${i}]`;
    const role = m["role"];
    if (typeof role === "string" && role !== "user" && role !== "assistant") {
      out.push(err(Code.ConfigFieldInvalid, `${mp}.role`, `must be "user" or "assistant", got ${JSON.stringify(role)}`));
    }
    if (!isNonEmptyString(m["content"])) {
      out.push(err(Code.ConfigFieldRequired, `${mp}.content`, REQUIRED_MISSING));
    }
  });
}

// checkOutputFormat mirrors checkOutputFormat: type required + enum, schema
// required-and-object for json_schema, forbidden for json, mode enum.
function checkOutputFormat(of: unknown, path: string, out: Issue[]): void {
  if (!isPlainObject(of)) return;
  const p = `${path}.output_format`;
  const type = of["type"];
  if (!isNonEmptyString(type)) {
    out.push(err(Code.ConfigFieldRequired, `${p}.type`, REQUIRED_MISSING));
  } else if (type !== "json" && type !== "json_schema") {
    out.push(err(Code.ConfigFieldInvalid, `${p}.type`, `unknown output format ${JSON.stringify(type)} (expected one of "json", "json_schema")`));
  }
  const schema = of["schema"];
  const hasSchema = schema !== undefined && schema !== null;
  if (type === "json_schema") {
    if (!hasSchema) {
      out.push(err(Code.ConfigFieldRequired, `${p}.schema`, `required when type is "json_schema"`));
    } else if (!isPlainObject(schema)) {
      out.push(err(Code.ConfigFieldInvalid, `${p}.schema`, "must be a JSON object"));
    }
  } else if (type === "json" && hasSchema) {
    out.push(err(Code.ConfigFieldInvalid, `${p}.schema`, `applies only to type "json_schema"; a plain "json" format takes no schema`));
  }
  const mode = of["mode"];
  if (isNonEmptyString(mode) && mode !== "auto" && mode !== "repair_only") {
    out.push(err(Code.ConfigFieldInvalid, `${p}.mode`, `unknown mode ${JSON.stringify(mode)} (expected one of "auto", "repair_only")`));
  }
}

// checkJoin: mode required + enum. The enum miss is a codeless decode error on
// the backend (decode.go), so it is reported code-free here.
function checkJoin(cfg: Record<string, unknown>, path: string, out: Issue[]): void {
  const mode = cfg["mode"];
  if (!isNonEmptyString(mode)) {
    out.push(err(Code.ConfigFieldRequired, `${path}.mode`, REQUIRED_MISSING));
    return;
  }
  if (mode !== "all" && mode !== "any") {
    out.push(err("", `${path}.mode`, `unknown join mode ${JSON.stringify(mode)} (expected "all" or "any")`));
  }
}

// checkAgent: the agent ref is required and must name a declared role. The
// merged-model check (needs role resolution) is deferred to 17.5.
function checkAgent(cfg: Record<string, unknown>, path: string, def: Record<string, unknown>, out: Issue[]): void {
  const ref = cfg["agent"];
  if (!isNonEmptyString(ref)) {
    out.push(err(Code.ConfigFieldRequired, `${path}.agent`, REQUIRED_MISSING));
    return;
  }
  const agents = def["agents"];
  if (isPlainObject(agents) && !(ref in agents)) {
    out.push(err("agent_ref_unknown", `${path}.agent`, `agent ${JSON.stringify(ref)} names no agent in the definition's agents section`));
  }
}

// checkHumanApproval mirrors checkHumanApproval's single-step-local rules:
// title required, distinct allowed_decisions, timeout bound, on_reject routing.
// Edge-cross rules (approval_reject_edge_required), edit_schema/allow_edit, and
// on_timeout are deferred to 17.5.
function checkHumanApproval(cfg: Record<string, unknown>, path: string, out: Issue[]): void {
  if (!isNonEmptyString(cfg["title"])) {
    out.push(err(Code.ConfigFieldRequired, `${path}.title`, REQUIRED_MISSING));
  }
  const decisions = cfg["allowed_decisions"];
  if (Array.isArray(decisions)) {
    const seen = new Set<string>();
    decisions.forEach((d, i) => {
      if (typeof d !== "string") return;
      if (seen.has(d)) {
        out.push(err(Code.ConfigFieldInvalid, `${path}.allowed_decisions[${i}]`, `duplicate decision ${JSON.stringify(d)}`));
      }
      seen.add(d);
    });
  }
  const timeout = cfg["timeout"];
  if (isNonEmptyString(timeout) && !hasTemplate(timeout)) {
    const secs = parseGoDuration(timeout);
    if (secs === null || secs <= 0) {
      out.push(err(Code.ConfigFieldInvalid, `${path}.timeout`, `must be a positive duration, got ${JSON.stringify(timeout)}`));
    } else if (secs > MAX_APPROVAL_TIMEOUT_SECONDS) {
      out.push(err(Code.ConfigFieldInvalid, `${path}.timeout`, `must be at most 720h0m0s, got ${JSON.stringify(timeout)}`));
    }
  }
  if (cfg["on_reject"] === "route") {
    const effective = effectiveDecisions(decisions);
    if (!effective.includes("reject")) {
      out.push(err(Code.ConfigFieldInvalid, `${path}.on_reject`, `"route" routing requires "reject" to be an allowed decision`));
    }
  }
}

// The effective allowed decisions: the declared non-empty list, else the
// default [approve, reject].
function effectiveDecisions(decisions: unknown): string[] {
  if (Array.isArray(decisions) && decisions.length > 0) {
    return decisions.filter((d): d is string => typeof d === "string");
  }
  return ["approve", "reject"];
}

function checkSleep(cfg: Record<string, unknown>, path: string, out: Issue[]): void {
  const d = cfg["duration"];
  if (!isNonEmptyString(d)) {
    out.push(err(Code.ConfigFieldRequired, `${path}.duration`, REQUIRED_MISSING));
    return;
  }
  if (hasTemplate(d)) return; // runtime re-validates a templated duration
  const secs = parseGoDuration(d);
  if (secs === null || secs <= 0) {
    out.push(err(Code.ConfigFieldInvalid, `${path}.duration`, `must be a positive duration, got ${JSON.stringify(d)}`));
  }
}

function checkFailNTimes(cfg: Record<string, unknown>, path: string, out: Issue[]): void {
  const n = cfg["n"];
  if (n === undefined || n === 0) {
    out.push(err(Code.ConfigFieldRequired, `${path}.n`, REQUIRED_MISSING));
  } else if (typeof n === "number" && n < 1) {
    out.push(err(Code.ConfigFieldInvalid, `${path}.n`, `must be at least 1, got ${n}`));
  }
}

// Re-export the Definition type so consumers of this module have the shape.
export type { Definition };
