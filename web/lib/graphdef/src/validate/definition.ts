// The full client-side graph validator (ADR-019 §"Validation parity", ticket
// 17.5): a faithful port of internal/dag Validate (validate.go + mapvalidate.go
// + the cel/template/graph/agent helpers). It runs the decode stage first
// (decode.ts) and — only when the document decodes cleanly, exactly like the
// backend — the validate stage, in the backend's rule order, so a client
// verdict and a server verdict name the same problems in the same places. The
// parity test (test/parity.test.ts) proves accept/reject over the whole backend
// corpus and exact code+path over the validate stage against the Go golden.
//
// What stays backend-only (the client under-reports, never over-reports): a
// plugin's ConfigCompiler pre-flight (regex/CEL/schema compilability), the exact
// cel-go error message, and the full 8.2 template rewrite (the client lints the
// three recognised reference roots). Those never make the backend accept
// something the client rejects.

import { isPlainObject, isFiniteNumber } from "../util.js";
import { Code, err, warn, type Issue, hasErrors } from "./issue.js";
import {
  MAX_APPROVAL_TIMEOUT_SECONDS,
  MAX_BACKOFF_CAP_SECONDS,
  MAX_BACKOFF_INITIAL_SECONDS,
  MAX_BACKOFF_MULTIPLIER,
  MAX_BLACKBOARD_WRITES,
  MAX_CACHE_TTL_SECONDS,
  MAX_COMPACTION_STRATEGIES,
  MAX_CONTEXT_SOURCES,
  MAX_CONTEXT_TOP_K,
  MAX_EDGES,
  MAX_EXPR_LEN,
  MAX_LOOP_ITERATIONS,
  MAX_NAME_LEN,
  MAX_RETRY_ATTEMPTS,
  MAX_RUN_WALL_CLOCK_SECONDS,
  MAX_SEMANTIC_ATTEMPTS,
  MAX_STEP_TIMEOUT_SECONDS,
  MAX_STEPS,
  MAX_SUMMARY_TIMEOUT_SECONDS,
  MAX_VALIDATORS,
  DEFAULT_MAX_ADDED_STEPS_PER_EXPANSION,
  DEFAULT_MAX_TOTAL_STEPS,
  PLUGIN_NAME_RE,
  PLUGIN_NAME_RE_TEXT,
  STEP_ID_RE,
  STEP_ID_RE_TEXT,
} from "./limits.js";
import { parseGoDuration } from "./duration.js";
import { checkJSONPointer, validateBlackboardKey, validateBlackboardTags } from "./grammar.js";
import { checkExprInto } from "./cel.js";
import { checkFeedbackTemplate, hasTemplate, scanConfigRefs, type ConfigRef } from "./template.js";
import { resolveAgentStep } from "./agent.js";
import { runDecodeStage } from "./decode.js";
import { Graph, hasUniqueIds, pathString, type GraphEdge } from "./graph.js";

const LLM_FAMILY = new Set(["llm", "planner", "agent"]);

function str(v: unknown): string {
  return typeof v === "string" ? v : "";
}
function obj(v: unknown): Record<string, unknown> {
  return isPlainObject(v) ? v : {};
}
function arr(v: unknown): unknown[] {
  return Array.isArray(v) ? v : [];
}
function isLoopEdge(e: Record<string, unknown>): boolean {
  return e["type"] === "loop";
}
function jsonType(v: unknown): string {
  if (v === null) return "null";
  if (Array.isArray(v)) return "array";
  return typeof v;
}

/** The validator accumulates issues in the backend's order. */
class Validator {
  issues: Issue[] = [];
  add(code: string, path: string, msg: string, related?: string[]): void {
    this.issues.push(err(code, path, msg, related));
  }
  warn(code: string, path: string, msg: string, related?: string[]): void {
    this.issues.push(warn(code, path, msg, related));
  }
  has(...codes: string[]): boolean {
    return this.issues.some((i) => codes.includes(i.code));
  }
}

/**
 * Validate a workflow definition value client-side. Runs the decode stage; if it
 * fails, returns its codeless findings alone (the backend does the same).
 * Otherwise runs the full validate stage.
 */
export function validateDefinition(value: unknown): Issue[] {
  const decodeIssues = runDecodeStage(value);
  if (hasErrors(decodeIssues)) return decodeIssues;

  const def = obj(value);
  const v = new Validator();

  checkLimits(v, def);
  checkMaxWallClock(v, def);
  checkRunBudget(v, def);
  checkExpansion(v, def);
  const stepIndex = checkSteps(v, def);
  checkEdges(v, def, stepIndex);
  checkApprovals(v, def, stepIndex);
  checkGraph(v, def, stepIndex);
  checkTemplateSection(v, def);
  checkMaps(v, def);
  checkAgents(v, def);

  if (!v.has(Code.DuplicateStepID, Code.UnknownEdgeEndpoint)) {
    const g = buildGraph(def);
    if (g) {
      checkGraphSemantics(v, def, g);
      checkTemplates(v, def, g);
      checkContextGraph(v, def, g);
    }
  }
  checkAdvisories(v, def);
  return v.issues;
}

// The cost-bearing step types (a run budget can only bind on one of these).
const COST_BEARING = new Set(["llm", "planner", "agent", "tool", "retrieve", "map"]);

// checkAdvisories emits client-only budget-sanity WARNINGS (never a backend
// code, never blocking submit; the `advisory_` prefix keeps them out of parity).
// The backend does not model these — they help an author before submit.
function checkAdvisories(v: Validator, def: Record<string, unknown>): void {
  const budget = def["budget_usd"];
  const steps = arr(def["steps"]);
  if (isFiniteNumber(budget) && budget > 0) {
    const anyCostBearing = steps.some((s) => COST_BEARING.has(str(obj(s)["type"])));
    if (!anyCostBearing) {
      v.warn(Code.AdvisoryBudget, "budget_usd", "a run budget is set but no step is cost-bearing (llm/agent/planner/tool/retrieve/map); the budget will never bind");
    }
    // A step cap at or above the whole run budget can never bind before the
    // run budget parks the run first.
    steps.forEach((raw, i) => {
      const b = obj(obj(raw)["budget"]);
      const maxUSD = b["max_usd"];
      if (isFiniteNumber(maxUSD) && maxUSD >= budget) {
        v.warn(Code.AdvisoryBudget, `steps[${i}].budget.max_usd`, `step cap $${maxUSD} is at or above the run budget $${budget}; the run budget parks first, so this cap never binds`);
      }
    });
  }
}

/** Convenience: true when the definition has no error-severity issues. */
export function isValidDefinition(value: unknown): boolean {
  return !hasErrors(validateDefinition(value));
}

function buildGraph(def: Record<string, unknown>): Graph | null {
  const steps = arr(def["steps"]).map((s) => ({ id: str(obj(s)["id"]) }));
  if (!hasUniqueIds(steps)) return null;
  const ids = new Set(steps.map((s) => s.id));
  const edges: GraphEdge[] = arr(def["edges"]).map((e) => {
    const eo = obj(e);
    return { from: str(eo["from"]), to: str(eo["to"]), loop: isLoopEdge(eo) };
  });
  // Endpoints must resolve (Validate's gate excludes unknown_edge_endpoint).
  for (const e of edges) {
    if (!ids.has(e.from) || !ids.has(e.to)) return null;
  }
  return new Graph(steps, edges);
}

// ── document-level ──────────────────────────────────────────────────────────

function checkLimits(v: Validator, def: Record<string, unknown>): void {
  const steps = arr(def["steps"]);
  const edges = arr(def["edges"]);
  if (steps.length > MAX_STEPS) v.add(Code.LimitExceeded, "steps", `definition has ${steps.length} steps (max ${MAX_STEPS})`);
  if (edges.length > MAX_EDGES) v.add(Code.LimitExceeded, "edges", `definition has ${edges.length} edges (max ${MAX_EDGES})`);
  const name = str(def["name"]);
  if (name.length > MAX_NAME_LEN) v.add(Code.LimitExceeded, "name", `name is ${name.length} bytes (max ${MAX_NAME_LEN})`);
}

function checkDurationField(
  v: Validator,
  path: string,
  val: string,
  boundSeconds: number,
  boundLabel: string,
  code: string,
): void {
  const secs = parseGoDuration(val);
  if (secs === null) v.add(code, path, `not a Go duration string: ${JSON.stringify(val)}`);
  else if (secs <= 0) v.add(code, path, `must be positive, got ${JSON.stringify(val)}`);
  else if (secs > boundSeconds) v.add(code, path, `must be at most ${boundLabel}, got ${JSON.stringify(val)}`);
}

function checkMaxWallClock(v: Validator, def: Record<string, unknown>): void {
  const val = str(def["max_wall_clock"]);
  if (val === "") return;
  checkDurationField(v, "max_wall_clock", val, MAX_RUN_WALL_CLOCK_SECONDS, "720h0m0s", Code.MaxWallClockInvalid);
}

function checkRunBudget(v: Validator, def: Record<string, unknown>): void {
  const budget = def["budget_usd"];
  if (budget !== undefined && isFiniteNumber(budget) && budget <= 0) {
    v.add(Code.BudgetFieldInvalid, "budget_usd", `must be positive, got ${budget}`);
  }
  if (str(def["on_budget_exceeded"]) !== "" && budget === undefined) {
    v.add(Code.BudgetFieldInvalid, "on_budget_exceeded", "has no effect without budget_usd");
  }
}

interface ResolvedCaps {
  maxAddedStepsPerExpansion: number;
  maxTotalSteps: number;
}

function resolveExpansionCaps(def: Record<string, unknown>): ResolvedCaps {
  const p = isPlainObject(def["expansion"]) ? (def["expansion"] as Record<string, unknown>) : undefined;
  const caps: ResolvedCaps = {
    maxAddedStepsPerExpansion: DEFAULT_MAX_ADDED_STEPS_PER_EXPANSION,
    maxTotalSteps: DEFAULT_MAX_TOTAL_STEPS,
  };
  if (p) {
    if (isFiniteNumber(p["max_added_steps"])) caps.maxAddedStepsPerExpansion = p["max_added_steps"] as number;
    if (isFiniteNumber(p["max_total_steps"])) caps.maxTotalSteps = p["max_total_steps"] as number;
  }
  return caps;
}

function checkExpansion(v: Validator, def: Record<string, unknown>): void {
  const caps = resolveExpansionCaps(def);
  const p = isPlainObject(def["expansion"]) ? (def["expansion"] as Record<string, unknown>) : undefined;
  if (p) {
    const positive = (field: string): void => {
      const val = p[field];
      if (val !== undefined && isFiniteNumber(val) && val < 1) {
        v.add(Code.ExpansionFieldInvalid, `expansion.${field}`, `must be positive, got ${val}`);
      }
    };
    positive("max_added_steps");
    positive("max_total_steps");
    positive("max_expansions");
    positive("max_depth");
    const mts = p["max_total_steps"];
    if (isFiniteNumber(mts) && mts > MAX_STEPS) {
      v.add(Code.ExpansionFieldInvalid, "expansion.max_total_steps", `must not exceed the definition ceiling ${MAX_STEPS}, got ${mts}`);
    }
    const mas = p["max_added_steps"];
    if (isFiniteNumber(mas) && mas >= 1 && mas > caps.maxTotalSteps) {
      v.add(Code.ExpansionFieldInvalid, "expansion.max_added_steps", `cannot exceed max_total_steps ${caps.maxTotalSteps}, got ${mas}`);
    }
  }
  arr(def["steps"]).forEach((s, i) => {
    const so = obj(s);
    if (so["type"] !== "planner") return;
    const c = obj(so["config"]);
    const mas = c["max_added_steps"];
    if (mas === undefined || mas === 0 || !isFiniteNumber(mas)) return;
    const path = `steps[${i}].config.max_added_steps`;
    if (mas < 0) v.add(Code.ConfigFieldInvalid, path, `must be positive, got ${mas}`);
    else if (mas > caps.maxAddedStepsPerExpansion) v.add(Code.ConfigFieldInvalid, path, `cannot exceed the run per-expansion cap ${caps.maxAddedStepsPerExpansion}, got ${mas}`);
  });
}

// ── steps ───────────────────────────────────────────────────────────────────

function checkSteps(v: Validator, def: Record<string, unknown>): Map<string, number> {
  const steps = arr(def["steps"]);
  const index = new Map<string, number>();
  if (steps.length === 0) v.add(Code.NoSteps, "steps", "definition has no steps");
  steps.forEach((raw, i) => {
    const s = obj(raw);
    const path = `steps[${i}]`;
    const id = str(s["id"]);
    if (!STEP_ID_RE.test(id)) v.add(Code.InvalidStepID, `${path}.id`, `step ID ${JSON.stringify(id)} does not match ${STEP_ID_RE_TEXT}`);
    if (index.has(id)) v.add(Code.DuplicateStepID, `${path}.id`, `duplicate step ID ${JSON.stringify(id)} (first declared at steps[${index.get(id)}])`);
    else index.set(id, i);
    checkStepConfig(v, path, s);
    checkModelFallbacks(v, def, path, s);
    checkOutputFormat(v, path, s);
    checkRetry(v, path, s["retry"]);
    if (s["type"] !== "human_approval") checkTimeout(v, path, str(s["timeout"]));
    checkCache(v, path, s["cache"]);
    checkStepBudget(v, path, str(s["type"]), s["budget"]);
    checkValidation(v, path, s);
    checkBlackboard(v, path, s["blackboard"]);
    checkContext(v, path, s);
  });
  return index;
}

function requireField(v: Validator, c: Record<string, unknown>, field: string, path: string): void {
  const val = c[field];
  const empty = val === undefined || val === null || (typeof val === "string" && val === "");
  if (empty) v.add(Code.ConfigFieldRequired, `${path}.${field}`, "required field is missing");
}

function checkStepConfig(v: Validator, path: string, s: Record<string, unknown>): void {
  const type = str(s["type"]);
  const c = obj(s["config"]);
  const cp = `${path}.config`;
  switch (type) {
    case "llm":
    case "planner":
      checkLLMConfig(v, path, str(c["model"]), str(c["prompt"]), c["messages"]);
      break;
    case "tool":
      requireField(v, c, "tool", cp);
      break;
    case "retrieve": {
      requireField(v, c, "retriever", cp);
      requireField(v, c, "query", cp);
      const topK = c["top_k"];
      if (isFiniteNumber(topK) && topK < 0) v.add(Code.ConfigFieldInvalid, `${cp}.top_k`, `must not be negative, got ${topK}`);
      break;
    }
    case "map": {
      // items is raw JSON (a whole-expression template string or an array):
      // required means the key is present, matching Go's len(RawMessage) > 0.
      if (c["items"] === undefined) v.add(Code.ConfigFieldRequired, `${cp}.items`, "required field is missing");
      requireField(v, c, "body", cp);
      const maxItems = c["max_items"];
      if (isFiniteNumber(maxItems) && maxItems < 0) v.add(Code.ConfigFieldInvalid, `${cp}.max_items`, `must be positive, got ${maxItems}`);
      break;
    }
    case "agent":
      requireField(v, c, "agent", cp);
      break;
    case "human_approval":
      checkHumanApproval(v, path, s);
      break;
    case "join":
      requireField(v, c, "mode", cp);
      break;
    case "sleep": {
      const d = str(c["duration"]);
      if (d === "") v.add(Code.ConfigFieldRequired, `${cp}.duration`, "required field is missing");
      else if (!hasTemplate(d)) {
        const secs = parseGoDuration(d);
        if (secs === null) v.add(Code.ConfigFieldInvalid, `${cp}.duration`, `not a Go duration string`);
        else if (secs <= 0) v.add(Code.ConfigFieldInvalid, `${cp}.duration`, `must be positive, got ${JSON.stringify(d)}`);
      }
      break;
    }
    case "fail_n_times": {
      const n = c["n"];
      if (n === undefined || n === 0) v.add(Code.ConfigFieldRequired, `${cp}.n`, "required field is missing");
      else if (isFiniteNumber(n) && n < 0) v.add(Code.ConfigFieldInvalid, `${cp}.n`, `must be at least 1, got ${n}`);
      break;
    }
    case "counter":
      requireField(v, c, "path", cp);
      break;
    case "effectful_echo": {
      const p = str(c["path"]);
      if (p === "") v.add(Code.ConfigFieldRequired, `${cp}.path`, "required field is missing");
      const ft = c["fail_times"];
      if (isFiniteNumber(ft) && ft < 0) v.add(Code.ConfigFieldInvalid, `${cp}.fail_times`, `must not be negative, got ${ft}`);
      break;
    }
    case "blackboard_write": {
      const key = str(c["key"]);
      if (key === "") v.add(Code.ConfigFieldRequired, `${cp}.key`, "required field is missing");
      else {
        const e = validateBlackboardKey(key);
        if (e) v.add(Code.ConfigFieldInvalid, `${cp}.key`, e);
      }
      if (c["value"] === undefined || c["value"] === null || (typeof c["value"] === "string" && c["value"] === "")) {
        v.add(Code.ConfigFieldRequired, `${cp}.value`, "required field is missing");
      }
      const ev = c["expected_version"];
      if (isFiniteNumber(ev) && ev < 0) v.add(Code.ConfigFieldInvalid, `${cp}.expected_version`, `must not be negative, got ${ev}`);
      const tags = arr(c["tags"]).filter((t): t is string => typeof t === "string");
      if (tags.length > 0) {
        const e = validateBlackboardTags(tags);
        if (e) v.add(Code.ConfigFieldInvalid, `${cp}.tags`, e);
      }
      const readKey = str(c["read_key"]);
      if (readKey !== "") {
        const e = validateBlackboardKey(readKey);
        if (e) v.add(Code.ConfigFieldInvalid, `${cp}.read_key`, e);
      }
      break;
    }
    default:
      // gather, branch, noop, echo — no required config fields.
      break;
  }
}

function checkLLMConfig(v: Validator, path: string, model: string, prompt: string, messages: unknown): void {
  if (model === "") v.add(Code.ConfigFieldRequired, `${path}.config.model`, "required field is missing");
  const msgs = arr(messages);
  const hasPrompt = prompt !== "";
  const hasMessages = msgs.length > 0;
  if (hasPrompt && hasMessages) v.add(Code.ConfigFieldConflict, `${path}.config`, `"prompt" and "messages" are mutually exclusive`);
  else if (!hasPrompt && !hasMessages) v.add(Code.ConfigFieldRequired, `${path}.config`, `exactly one of "prompt" or "messages" is required`);
  msgs.forEach((m, i) => {
    const mo = obj(m);
    const mp = `${path}.config.messages[${i}]`;
    const role = mo["role"];
    if (role !== "user" && role !== "assistant") v.add(Code.ConfigFieldInvalid, `${mp}.role`, `must be "user" or "assistant", got ${JSON.stringify(role)}`);
    if (str(mo["content"]) === "") v.add(Code.ConfigFieldRequired, `${mp}.content`, "required field is missing");
  });
}

function checkModelFallbacks(v: Validator, def: Record<string, unknown>, path: string, s: Record<string, unknown>): void {
  const type = str(s["type"]);
  const c = obj(s["config"]);
  const fallbacks = arr(c["model_fallbacks"]);
  if (type !== "llm") {
    if (fallbacks.length > 0) v.add(Code.ConfigFieldInvalid, `${path}.config.model_fallbacks`, `applies only to llm steps, not ${JSON.stringify(type)}`);
    return;
  }
  if (fallbacks.length === 0) return;
  const fp = `${path}.config.model_fallbacks`;
  const budget = obj(s["budget"]);
  const hasRunBudget = def["budget_usd"] !== undefined;
  const hasStepUSD = budget["max_usd"] !== undefined;
  if (!hasRunBudget && !hasStepUSD) v.add(Code.ConfigFieldInvalid, fp, "has no budget to trigger against: set budget_usd or the step's budget.max_usd");
  const seen = new Set<string>([str(c["model"])]);
  let prevFrac = 0;
  let havePrev = false;
  fallbacks.forEach((f, i) => {
    const fo = obj(f);
    const ep = `${fp}[${i}]`;
    const m = str(fo["model"]);
    if (m === "") v.add(Code.ConfigFieldRequired, `${ep}.model`, "required field is missing");
    else if (seen.has(m)) v.add(Code.ConfigFieldInvalid, `${ep}.model`, `duplicate model ${JSON.stringify(m)} in the fallback chain`);
    else seen.add(m);
    const frac = fo["at_budget_fraction"];
    if (frac === undefined || !isFiniteNumber(frac)) return;
    if (frac <= 0 || frac >= 1) v.add(Code.ConfigFieldInvalid, `${ep}.at_budget_fraction`, `must be between 0 and 1 (exclusive), got ${frac}`);
    if (!hasRunBudget) v.add(Code.ConfigFieldInvalid, `${ep}.at_budget_fraction`, "requires budget_usd (it is a fraction of the run budget)");
    if (havePrev && frac < prevFrac) v.add(Code.ConfigFieldInvalid, `${ep}.at_budget_fraction`, `must be >= the previous fallback's threshold ${prevFrac}, got ${frac} (cheaper tiers trigger at higher spend)`);
    prevFrac = frac;
    havePrev = true;
  });
}

function checkOutputFormat(v: Validator, path: string, s: Record<string, unknown>): void {
  const type = str(s["type"]);
  const c = obj(s["config"]);
  const of = c["output_format"];
  if (type !== "llm") {
    if (isPlainObject(of)) v.add(Code.ConfigFieldInvalid, `${path}.config.output_format`, `applies only to llm steps, not ${JSON.stringify(type)}`);
    return;
  }
  if (!isPlainObject(of)) return;
  const fp = `${path}.config.output_format`;
  const oft = of["type"];
  switch (oft) {
    case "":
    case undefined:
      v.add(Code.ConfigFieldRequired, `${fp}.type`, "required field is missing");
      break;
    case "json":
      if (of["schema"] !== undefined && of["schema"] !== null) v.add(Code.ConfigFieldInvalid, `${fp}.schema`, `applies only to type "json_schema"; a plain "json" format takes no schema`);
      break;
    case "json_schema":
      if (of["schema"] === undefined || of["schema"] === null) v.add(Code.ConfigFieldRequired, `${fp}.schema`, `required when type is "json_schema"`);
      else if (!isPlainObject(of["schema"])) v.add(Code.ConfigFieldInvalid, `${fp}.schema`, "must be a JSON object");
      break;
    default:
      v.add(Code.ConfigFieldInvalid, `${fp}.type`, `unknown output format ${JSON.stringify(oft)} (expected one of "json", "json_schema")`);
  }
  const mode = of["mode"];
  if (mode !== undefined && mode !== "" && mode !== "auto" && mode !== "repair_only") {
    v.add(Code.ConfigFieldInvalid, `${fp}.mode`, `unknown mode ${JSON.stringify(mode)} (expected one of "auto", "repair_only")`);
  }
}

function checkRetry(v: Validator, path: string, rp: unknown): void {
  if (!isPlainObject(rp)) return;
  const p = `${path}.retry`;
  const maxAttempts = rp["max_attempts"];
  if (isFiniteNumber(maxAttempts) && (maxAttempts < 0 || maxAttempts > MAX_RETRY_ATTEMPTS)) {
    v.add(Code.RetryFieldInvalid, `${p}.max_attempts`, `must be between 1 and ${MAX_RETRY_ATTEMPTS}, got ${maxAttempts}`);
  }
  const jitter = rp["jitter"];
  if (jitter !== undefined && jitter !== "" && jitter !== "full" && jitter !== "none") {
    v.add(Code.RetryFieldInvalid, `${p}.jitter`, `unknown jitter mode ${JSON.stringify(jitter)}`);
  }
  const b = rp["backoff"];
  if (isPlainObject(b)) {
    const initial = checkBackoffDuration(v, `${p}.backoff.initial`, str(b["initial"]), MAX_BACKOFF_INITIAL_SECONDS, "1h0m0s");
    const ceiling = checkBackoffDuration(v, `${p}.backoff.cap`, str(b["cap"]), MAX_BACKOFF_CAP_SECONDS, "24h0m0s");
    if (initial > 0 && ceiling > 0 && ceiling < initial) v.add(Code.RetryFieldInvalid, `${p}.backoff.cap`, `must be at least backoff.initial (${str(b["initial"])}), got ${str(b["cap"])}`);
    const mult = b["multiplier"];
    if (isFiniteNumber(mult) && mult !== 0 && (mult < 1 || mult > MAX_BACKOFF_MULTIPLIER)) v.add(Code.RetryFieldInvalid, `${p}.backoff.multiplier`, `must be between 1 and ${MAX_BACKOFF_MULTIPLIER}, got ${mult}`);
  }
  const retryOn = rp["retry_on"];
  if (Array.isArray(retryOn) && retryOn.length === 0) v.add(Code.RetryFieldInvalid, `${p}.retry_on`, "must not be empty when present (omit the key for the engine default)");
  const seen = new Set<string>();
  arr(retryOn).forEach((c, i) => {
    const cls = str(c);
    const entry = `${p}.retry_on[${i}]`;
    if (seen.has(cls)) {
      v.add(Code.RetryFieldInvalid, entry, `duplicate error class ${JSON.stringify(cls)}`);
      return;
    }
    seen.add(cls);
    if (cls === "transient" || cls === "timeout") return;
    if (cls === "validation_failed") v.add(Code.RetryFieldInvalid, entry, `${JSON.stringify(cls)} is not a transport retry class — configure semantic retries via the step's validation policy (ADR-013), not retry_on`);
    else if (cls === "permanent" || cls === "cancelled") v.add(Code.RetryFieldInvalid, entry, `${JSON.stringify(cls)} is never retryable`);
    else v.add(Code.RetryFieldInvalid, entry, `unknown error class ${JSON.stringify(cls)}`);
  });
}

function checkBackoffDuration(v: Validator, path: string, val: string, boundSeconds: number, boundLabel: string): number {
  if (val === "") {
    v.add(Code.RetryFieldRequired, path, "required field is missing");
    return 0;
  }
  const secs = parseGoDuration(val);
  if (secs === null) {
    v.add(Code.RetryFieldInvalid, path, "not a Go duration string");
    return 0;
  }
  if (secs <= 0) {
    v.add(Code.RetryFieldInvalid, path, `must be positive, got ${JSON.stringify(val)}`);
    return 0;
  }
  if (secs > boundSeconds) {
    v.add(Code.RetryFieldInvalid, path, `must be at most ${boundLabel}, got ${JSON.stringify(val)}`);
    return 0;
  }
  return secs;
}

function checkTimeout(v: Validator, path: string, val: string): void {
  if (val === "") return;
  checkDurationField(v, `${path}.timeout`, val, MAX_STEP_TIMEOUT_SECONDS, "24h0m0s", Code.TimeoutFieldInvalid);
}

function checkCache(v: Validator, path: string, cp: unknown): void {
  if (!isPlainObject(cp)) return;
  const p = `${path}.cache`;
  if (str(cp["mode"]) === "") v.add(Code.CacheFieldRequired, `${p}.mode`, "mode is required when a cache block is present");
  const ttl = str(cp["ttl"]);
  if (ttl === "") return;
  checkDurationField(v, `${p}.ttl`, ttl, MAX_CACHE_TTL_SECONDS, "720h0m0s", Code.CacheFieldInvalid);
}

function checkStepBudget(v: Validator, path: string, type: string, b: unknown): void {
  if (!isPlainObject(b)) return;
  const p = `${path}.budget`;
  const maxUSD = b["max_usd"];
  const maxTokens = b["max_tokens"];
  const hasUSD = maxUSD !== undefined;
  const tokens = isFiniteNumber(maxTokens) ? maxTokens : 0;
  if (!hasUSD && tokens === 0) v.add(Code.BudgetFieldRequired, p, "at least one of max_usd or max_tokens is required when a budget block is present");
  if (isFiniteNumber(maxUSD) && maxUSD <= 0) v.add(Code.BudgetFieldInvalid, `${p}.max_usd`, `must be positive, got ${maxUSD}`);
  if (tokens < 0) v.add(Code.BudgetFieldInvalid, `${p}.max_tokens`, `must be positive, got ${tokens}`);
  if (tokens > 0 && type !== "llm" && type !== "planner") v.add(Code.BudgetFieldInvalid, `${p}.max_tokens`, `applies only to llm-family steps, not ${JSON.stringify(type)}`);
}

function checkValidation(v: Validator, path: string, s: Record<string, unknown>): void {
  const vp = s["validation"];
  if (!isPlainObject(vp)) return;
  const type = str(s["type"]);
  if (type === "human_approval") return; // checkHumanApproval owns that error
  const p = `${path}.validation`;
  const validators = arr(vp["validators"]);
  const c = obj(s["config"]);
  const hasImplicitChain = (type === "llm" && isPlainObject(c["output_format"])) || type === "planner" || type === "agent";
  if (validators.length === 0 && !hasImplicitChain) {
    v.add(Code.ValidationFieldRequired, `${p}.validators`, "at least one validator is required when a validation block is present");
    return;
  }
  if (validators.length > MAX_VALIDATORS) v.add(Code.ValidationFieldInvalid, `${p}.validators`, `must have at most ${MAX_VALIDATORS} validators, got ${validators.length}`);
  validators.forEach((spec, i) => {
    const so = obj(spec);
    const entry = `${p}.validators[${i}]`;
    const name = str(so["name"]);
    if (name === "") v.add(Code.ValidationFieldRequired, `${entry}.name`, "validator name is required");
    else if (!PLUGIN_NAME_RE.test(name)) v.add(Code.ValidationFieldInvalid, `${entry}.name`, `validator name ${JSON.stringify(name)} does not match ${PLUGIN_NAME_RE_TEXT}`);
    const target = str(so["target"]);
    if (target !== "") {
      const e = checkJSONPointer(target);
      if (e) v.add(Code.ValidationFieldInvalid, `${entry}.target`, `invalid JSON pointer: ${e}`);
    }
  });
  checkSemanticPolicy(v, p, type, vp);
}

function effectiveMaxAttempts(vp: Record<string, unknown>): number {
  const m = vp["max_attempts"];
  return isFiniteNumber(m) && m > 0 ? m : 1;
}

function checkSemanticPolicy(v: Validator, path: string, type: string, vp: Record<string, unknown>): void {
  const maxAttempts = vp["max_attempts"];
  if (isFiniteNumber(maxAttempts) && maxAttempts !== 0 && (maxAttempts < 1 || maxAttempts > MAX_SEMANTIC_ATTEMPTS)) {
    v.add(Code.ValidationFieldInvalid, `${path}.max_attempts`, `must be between 1 and ${MAX_SEMANTIC_ATTEMPTS}, got ${maxAttempts}`);
  }
  const fb = vp["feedback"];
  if (!isPlainObject(fb)) return;
  const fp = `${path}.feedback`;
  if (!LLM_FAMILY.has(type)) v.add(Code.ValidationFieldInvalid, fp, `applies only to llm-family steps, not ${JSON.stringify(type)}`);
  if (effectiveMaxAttempts(vp) < 2) v.add(Code.ValidationFieldInvalid, fp, "has no effect without max_attempts >= 2 (a critique is only used on a re-attempt)");
  const template = str(fb["template"]);
  if (template !== "") {
    const e = checkFeedbackTemplate(template);
    if (e) v.add(Code.ValidationFieldInvalid, `${fp}.template`, e);
  }
  const moc = fb["max_output_chars"];
  if (isFiniteNumber(moc) && moc < 0) v.add(Code.ValidationFieldInvalid, `${fp}.max_output_chars`, `must be positive, got ${moc}`);
}

function checkBlackboard(v: Validator, path: string, bp: unknown): void {
  if (!isPlainObject(bp)) return;
  const p = `${path}.blackboard`;
  const write = arr(bp["write"]);
  if (write.length === 0) {
    v.add(Code.BlackboardFieldRequired, `${p}.write`, "at least one write is required when a blackboard block is present");
    return;
  }
  if (write.length > MAX_BLACKBOARD_WRITES) v.add(Code.BlackboardFieldInvalid, `${p}.write`, `must have at most ${MAX_BLACKBOARD_WRITES} writes, got ${write.length}`);
  const seen = new Map<string, number>();
  write.forEach((w, i) => {
    const wo = obj(w);
    const entry = `${p}.write[${i}]`;
    const key = str(wo["key"]);
    const e = validateBlackboardKey(key);
    if (e) v.add(Code.BlackboardFieldInvalid, `${entry}.key`, e);
    else if (seen.has(key)) v.add(Code.BlackboardFieldInvalid, `${entry}.key`, `duplicate write to key ${JSON.stringify(key)} (first at write[${seen.get(key)}])`);
    else seen.set(key, i);
    const from = str(wo["from"]);
    if (from !== "") {
      const pe = checkJSONPointer(from);
      if (pe) v.add(Code.BlackboardFieldInvalid, `${entry}.from`, `invalid JSON pointer: ${pe}`);
    }
    const tags = arr(wo["tags"]).filter((t): t is string => typeof t === "string");
    if (tags.length > 0) {
      const te = validateBlackboardTags(tags);
      if (te) v.add(Code.BlackboardFieldInvalid, `${entry}.tags`, te);
    }
  });
}

// ── context ─────────────────────────────────────────────────────────────────

function checkContext(v: Validator, path: string, s: Record<string, unknown>): void {
  const cs = s["context"];
  if (!isPlainObject(cs)) return;
  const p = `${path}.context`;
  const sources = arr(cs["sources"]);
  if (sources.length === 0) {
    v.add(Code.ContextFieldRequired, `${p}.sources`, "at least one source is required when a context block is present");
    return;
  }
  const type = str(s["type"]);
  if (!LLM_FAMILY.has(type)) v.add(Code.ContextFieldInvalid, p, `applies only to llm-family steps, not ${JSON.stringify(type)}`);
  if (sources.length > MAX_CONTEXT_SOURCES) v.add(Code.ContextFieldInvalid, `${p}.sources`, `must have at most ${MAX_CONTEXT_SOURCES} sources, got ${sources.length}`);
  const seenName = new Map<string, number>();
  sources.forEach((src, i) => {
    const so = obj(src);
    const entry = `${p}.sources[${i}]`;
    const name = str(so["name"]);
    if (name !== "") {
      if (seenName.has(name)) v.add(Code.ContextFieldInvalid, `${entry}.name`, `duplicate source name ${JSON.stringify(name)} (first at sources[${seenName.get(name)}])`);
      else seenName.set(name, i);
    }
    checkContextSource(v, entry, so);
  });
  checkCompaction(v, p, cs);
}

const CONTEXT_FIELD_SETTERS: Record<string, (so: Record<string, unknown>) => boolean> = {
  step: (so) => str(so["step"]) !== "",
  path: (so) => str(so["path"]) !== "",
  key: (so) => str(so["key"]) !== "",
  tags: (so) => arr(so["tags"]).length > 0,
  retriever: (so) => str(so["retriever"]) !== "",
  query: (so) => str(so["query"]) !== "",
  top_k: (so) => so["top_k"] !== undefined,
  text: (so) => str(so["text"]) !== "",
  role: (so) => str(so["role"]) !== "",
};

function forbidContextFields(v: Validator, entry: string, so: Record<string, unknown>, kind: string, fields: string[]): void {
  for (const f of fields) {
    if (CONTEXT_FIELD_SETTERS[f]?.(so)) v.add(Code.ContextFieldInvalid, `${entry}.${f}`, `${f} is not valid for a ${kind} source`);
  }
}

function checkContextSource(v: Validator, entry: string, so: Record<string, unknown>): void {
  const pinned = so["pinned"] === true;
  const maxTokens = so["max_tokens"];
  if (pinned && maxTokens !== undefined) v.add(Code.ContextFieldInvalid, `${entry}.max_tokens`, "a pinned source cannot also carry a per-source max_tokens cap");
  if (isFiniteNumber(maxTokens) && maxTokens < 1) v.add(Code.ContextFieldInvalid, `${entry}.max_tokens`, `must be at least 1, got ${maxTokens}`);
  if (hasTemplate(so["text"])) v.add(Code.ContextFieldInvalid, `${entry}.text`, "template expressions are not supported inside a context block");
  if (hasTemplate(so["query"])) v.add(Code.ContextFieldInvalid, `${entry}.query`, "template expressions are not supported inside a context block");
  const kind = str(so["kind"]);
  switch (kind) {
    case "":
      v.add(Code.ContextFieldRequired, `${entry}.kind`, "source kind is required");
      break;
    case "step_output": {
      if (str(so["step"]) === "") v.add(Code.ContextFieldRequired, `${entry}.step`, "step is required for a step_output source");
      const pe = str(so["path"]) !== "" ? checkJSONPointer(str(so["path"])) : null;
      if (pe) v.add(Code.ContextFieldInvalid, `${entry}.path`, `invalid JSON pointer: ${pe}`);
      forbidContextFields(v, entry, so, "step_output", ["key", "tags", "retriever", "query", "top_k", "text", "role"]);
      break;
    }
    case "blackboard": {
      const key = str(so["key"]);
      const tags = arr(so["tags"]);
      if (key === "" && tags.length === 0) v.add(Code.ContextFieldRequired, entry, "a blackboard source requires exactly one of key or tags");
      else if (key !== "" && tags.length > 0) v.add(Code.ContextFieldInvalid, entry, "a blackboard source cannot set both key and tags");
      if (key !== "") {
        const e = validateBlackboardKey(key);
        if (e) v.add(Code.ContextFieldInvalid, `${entry}.key`, e);
      }
      const tagStrs = tags.filter((t): t is string => typeof t === "string");
      if (tagStrs.length > 0) {
        const e = validateBlackboardTags(tagStrs);
        if (e) v.add(Code.ContextFieldInvalid, `${entry}.tags`, e);
      }
      forbidContextFields(v, entry, so, "blackboard", ["step", "path", "retriever", "query", "top_k", "text", "role"]);
      break;
    }
    case "retrieval": {
      const retriever = str(so["retriever"]);
      if (retriever === "") v.add(Code.ContextFieldRequired, `${entry}.retriever`, "retriever is required for a retrieval source");
      else if (!PLUGIN_NAME_RE.test(retriever)) v.add(Code.ContextFieldInvalid, `${entry}.retriever`, `retriever name ${JSON.stringify(retriever)} does not match ${PLUGIN_NAME_RE_TEXT}`);
      if (str(so["query"]) === "") v.add(Code.ContextFieldRequired, `${entry}.query`, "query is required for a retrieval source");
      const topK = so["top_k"];
      if (isFiniteNumber(topK) && (topK < 0 || topK > MAX_CONTEXT_TOP_K)) v.add(Code.ContextFieldInvalid, `${entry}.top_k`, `must be between 0 and ${MAX_CONTEXT_TOP_K}, got ${topK}`);
      forbidContextFields(v, entry, so, "retrieval", ["step", "path", "key", "tags", "text", "role"]);
      break;
    }
    case "literal":
      if (str(so["text"]) === "") v.add(Code.ContextFieldRequired, `${entry}.text`, "text is required for a literal source");
      forbidContextFields(v, entry, so, "literal", ["step", "path", "key", "tags", "retriever", "query", "top_k", "role"]);
      break;
    case "thread": {
      const key = str(so["key"]);
      if (key !== "") {
        const e = validateBlackboardKey(key);
        if (e) v.add(Code.ContextFieldInvalid, `${entry}.key`, e);
      }
      forbidContextFields(v, entry, so, "thread", ["step", "path", "tags", "retriever", "query", "top_k", "text"]);
      break;
    }
  }
}

function checkCompaction(v: Validator, path: string, cs: Record<string, unknown>): void {
  const budget = cs["budget_tokens"];
  if (isFiniteNumber(budget) && budget < 1) v.add(Code.ContextFieldInvalid, `${path}.budget_tokens`, `must be at least 1, got ${budget}`);
  const compaction = arr(cs["compaction"]);
  if (compaction.length > MAX_COMPACTION_STRATEGIES) v.add(Code.ContextFieldInvalid, `${path}.compaction`, `must have at most ${MAX_COMPACTION_STRATEGIES} strategies, got ${compaction.length}`);
  const seen = new Map<string, number>();
  compaction.forEach((st, i) => {
    const so = obj(st);
    const entry = `${path}.compaction[${i}]`;
    const strategy = str(so["strategy"]);
    switch (strategy) {
      case "":
        v.add(Code.ContextFieldRequired, `${entry}.strategy`, "strategy is required");
        return;
      case "sliding_window": {
        const n = so["n"];
        if (n === undefined) v.add(Code.ContextFieldRequired, `${entry}.n`, "n is required for a sliding_window strategy");
        else if (isFiniteNumber(n) && n < 1) v.add(Code.ContextFieldInvalid, `${entry}.n`, `must be at least 1, got ${n}`);
        break;
      }
      case "truncate_oldest": {
        const mt = so["min_tokens"];
        if (isFiniteNumber(mt) && mt < 0) v.add(Code.ContextFieldInvalid, `${entry}.min_tokens`, `must be non-negative, got ${mt}`);
        break;
      }
      case "summarize":
        checkSummarizeStrategy(v, entry, so);
        break;
    }
    if (so["n"] !== undefined && strategy !== "sliding_window") v.add(Code.ContextFieldInvalid, `${entry}.n`, `n is not valid for a ${strategy} strategy`);
    if (so["min_tokens"] !== undefined && strategy !== "truncate_oldest") v.add(Code.ContextFieldInvalid, `${entry}.min_tokens`, `min_tokens is not valid for a ${strategy} strategy`);
    if (str(so["model"]) !== "" && strategy !== "summarize") v.add(Code.ContextFieldInvalid, `${entry}.model`, `model is not valid for a ${strategy} strategy`);
    if (str(so["key"]) !== "" && strategy !== "summarize") v.add(Code.ContextFieldInvalid, `${entry}.key`, `key is not valid for a ${strategy} strategy`);
    if (so["max_tokens"] !== undefined && strategy !== "summarize") v.add(Code.ContextFieldInvalid, `${entry}.max_tokens`, `max_tokens is not valid for a ${strategy} strategy`);
    if (str(so["timeout"]) !== "" && strategy !== "summarize") v.add(Code.ContextFieldInvalid, `${entry}.timeout`, `timeout is not valid for a ${strategy} strategy`);
    if (strategy !== "") {
      if (seen.has(strategy)) v.add(Code.ContextFieldInvalid, `${entry}.strategy`, `duplicate strategy ${JSON.stringify(strategy)} (first at compaction[${seen.get(strategy)}])`);
      else seen.set(strategy, i);
    }
  });
}

function checkSummarizeStrategy(v: Validator, entry: string, so: Record<string, unknown>): void {
  if (str(so["model"]) === "") v.add(Code.ContextFieldRequired, `${entry}.model`, "model is required for a summarize strategy");
  const key = str(so["key"]);
  if (key !== "") {
    const e = validateBlackboardKey(key);
    if (e) v.add(Code.ContextFieldInvalid, `${entry}.key`, `invalid blackboard key: ${e}`);
  }
  const mt = so["max_tokens"];
  if (isFiniteNumber(mt) && mt < 1) v.add(Code.ContextFieldInvalid, `${entry}.max_tokens`, `must be at least 1, got ${mt}`);
  const timeout = str(so["timeout"]);
  if (timeout !== "") {
    const secs = parseGoDuration(timeout);
    if (secs === null) v.add(Code.ContextFieldInvalid, `${entry}.timeout`, "not a Go duration string");
    else if (secs <= 0) v.add(Code.ContextFieldInvalid, `${entry}.timeout`, `must be positive, got ${JSON.stringify(timeout)}`);
    else if (secs > MAX_SUMMARY_TIMEOUT_SECONDS) v.add(Code.ContextFieldInvalid, `${entry}.timeout`, `must be at most 10m0s, got ${JSON.stringify(timeout)}`);
  }
}

// ── human approval ──────────────────────────────────────────────────────────

function checkHumanApproval(v: Validator, path: string, s: Record<string, unknown>): void {
  const c = obj(s["config"]);
  const cp = `${path}.config`;
  if (str(c["title"]) === "") v.add(Code.ConfigFieldRequired, `${cp}.title`, "required field is missing");

  const allowedList = arr(c["allowed_decisions"]).filter((d): d is string => typeof d === "string");
  const allowed = new Set<string>();
  arr(c["allowed_decisions"]).forEach((d, i) => {
    const ds = str(d);
    if (allowed.has(ds)) v.add(Code.ConfigFieldInvalid, `${cp}.allowed_decisions[${i}]`, `duplicate decision ${JSON.stringify(ds)}`);
    allowed.add(ds);
  });
  const decisionAllowed = (d: string): boolean => (allowedList.length === 0 ? true : allowed.has(d));

  // Go gates on len(EditSchema) > 0: the raw bytes exist, i.e. the key is
  // present in the JSON (an absent key is nil). `{}` counts as present.
  const editSchema = c["edit_schema"];
  if (editSchema !== undefined) {
    if (c["allow_edit"] !== true) v.add(Code.ConfigFieldInvalid, `${cp}.edit_schema`, "requires allow_edit to be true");
    if (jsonType(editSchema) !== "object") v.add(Code.ConfigFieldInvalid, `${cp}.edit_schema`, "must be a JSON object");
  }
  if (c["allow_edit"] === true && !decisionAllowed("approve")) v.add(Code.ConfigFieldInvalid, `${cp}.allow_edit`, `requires "approve" to be an allowed decision`);

  const timeout = str(c["timeout"]);
  if (timeout !== "") {
    const secs = parseGoDuration(timeout);
    if (secs === null) v.add(Code.ConfigFieldInvalid, `${cp}.timeout`, "not a Go duration string");
    else if (secs <= 0) v.add(Code.ConfigFieldInvalid, `${cp}.timeout`, `must be positive, got ${JSON.stringify(timeout)}`);
    else if (secs > MAX_APPROVAL_TIMEOUT_SECONDS) v.add(Code.ConfigFieldInvalid, `${cp}.timeout`, `must be at most 720h0m0s, got ${JSON.stringify(timeout)}`);
  } else if (str(c["on_timeout"]) !== "") {
    v.add(Code.ConfigFieldInvalid, `${cp}.on_timeout`, "requires a timeout to be set");
  }
  const onTimeout = str(c["on_timeout"]);
  if (onTimeout === "reject" && !decisionAllowed("reject")) v.add(Code.ConfigFieldInvalid, `${cp}.on_timeout`, `records a "reject" decision, which is not allowed`);
  if (onTimeout === "approve" && !decisionAllowed("approve")) v.add(Code.ConfigFieldInvalid, `${cp}.on_timeout`, `records an "approve" decision, which is not allowed`);

  if (str(c["on_reject"]) === "route" && !decisionAllowed("reject")) v.add(Code.ConfigFieldInvalid, `${cp}.on_reject`, `"route" routing requires "reject" to be an allowed decision`);

  if (s["validation"] !== undefined && s["validation"] !== null) {
    v.add(Code.ValidationFieldInvalid, `${path}.validation`, "not valid on a human_approval step (it produces no model output to validate; constrain edits with config.edit_schema)");
  }
  if (str(s["timeout"]) !== "") {
    v.add(Code.TimeoutFieldInvalid, `${path}.timeout`, "not valid on a human_approval step (the wait is config.timeout)");
  }
}

// ── edges ───────────────────────────────────────────────────────────────────

function checkEdges(v: Validator, def: Record<string, unknown>, stepIndex: Map<string, number>): void {
  const steps = arr(def["steps"]);
  arr(def["edges"]).forEach((raw, i) => {
    const e = obj(raw);
    const path = `edges[${i}]`;
    const from = str(e["from"]);
    const to = str(e["to"]);
    if (!stepIndex.has(from)) v.add(Code.UnknownEdgeEndpoint, `${path}.from`, `unknown step ${JSON.stringify(from)}`);
    if (!stepIndex.has(to)) v.add(Code.UnknownEdgeEndpoint, `${path}.to`, `unknown step ${JSON.stringify(to)}`);
    if (isLoopEdge(e)) {
      if (str(e["when"]) !== "") v.add(Code.LoopFieldForbidden, `${path}.when`, `"when" is not valid on a loop edge (its predicate is "condition")`);
      const condition = str(e["condition"]);
      if (condition === "") v.add(Code.LoopFieldRequired, `${path}.condition`, "required on a loop edge");
      else if (condition.length > MAX_EXPR_LEN) v.add(Code.LimitExceeded, `${path}.condition`, `expression is ${condition.length} bytes (max ${MAX_EXPR_LEN})`);
      else checkExprInto(`${path}.condition`, condition, v.issues);
      const maxIter = e["max_iterations"];
      if (maxIter === undefined || maxIter === 0) v.add(Code.LoopFieldRequired, `${path}.max_iterations`, "required on a loop edge");
      else if (isFiniteNumber(maxIter) && (maxIter < 1 || maxIter > MAX_LOOP_ITERATIONS)) v.add(Code.LimitExceeded, `${path}.max_iterations`, `must be between 1 and ${MAX_LOOP_ITERATIONS}, got ${maxIter}`);
      const onExhausted = str(e["on_exhausted"]);
      if (onExhausted !== "" && onExhausted !== "proceed" && onExhausted !== "fail") v.add(Code.ConfigFieldInvalid, `${path}.on_exhausted`, `must be "proceed" or "fail", got ${JSON.stringify(onExhausted)}`);
      const np = e["no_progress"];
      if (isPlainObject(np)) {
        const policy = str(np["policy"]);
        if (policy !== "" && policy !== "proceed" && policy !== "fail") v.add(Code.ConfigFieldInvalid, `${path}.no_progress.policy`, `must be "proceed" or "fail", got ${JSON.stringify(policy)}`);
        const npPath = str(np["path"]);
        if (npPath !== "") {
          const pe = checkJSONPointer(npPath);
          if (pe) v.add(Code.ConfigFieldInvalid, `${path}.no_progress.path`, `invalid JSON pointer: ${pe}`);
        }
      }
      if (str(e["decision"]) !== "") v.add(Code.ApprovalEdgeInvalid, `${path}.decision`, "not valid on a loop edge");
    } else {
      if (str(e["condition"]) !== "") v.add(Code.LoopFieldForbidden, `${path}.condition`, "only valid on loop edges");
      if (e["max_iterations"] !== undefined && e["max_iterations"] !== 0) v.add(Code.LoopFieldForbidden, `${path}.max_iterations`, "only valid on loop edges");
      if (str(e["on_exhausted"]) !== "") v.add(Code.LoopFieldForbidden, `${path}.on_exhausted`, "only valid on loop edges");
      if (e["no_progress"] !== undefined && e["no_progress"] !== null) v.add(Code.LoopFieldForbidden, `${path}.no_progress`, "only valid on loop edges");
      if (str(e["decision"]) !== "") {
        const idx = stepIndex.get(from);
        const fromType = idx !== undefined ? str(obj(steps[idx])["type"]) : "";
        if (idx === undefined || fromType !== "human_approval") v.add(Code.ApprovalEdgeInvalid, `${path}.decision`, "only valid on an edge leaving a human_approval step");
      }
      const when = str(e["when"]);
      if (when.length > MAX_EXPR_LEN) v.add(Code.LimitExceeded, `${path}.when`, `expression is ${when.length} bytes (max ${MAX_EXPR_LEN})`);
      else if (when !== "") checkExprInto(`${path}.when`, when, v.issues);
    }
  });
}

function checkApprovals(v: Validator, def: Record<string, unknown>, stepIndex: Map<string, number>): void {
  const steps = arr(def["steps"]);
  const edges = arr(def["edges"]);
  const rejectEdge = new Map<string, boolean>();
  edges.forEach((raw) => {
    const e = obj(raw);
    if (isLoopEdge(e)) return;
    if (str(e["decision"]) === "reject") rejectEdge.set(str(e["from"]), true);
  });
  steps.forEach((raw, i) => {
    const s = obj(raw);
    if (str(s["type"]) !== "human_approval") return;
    const c = obj(s["config"]);
    if (str(c["on_reject"]) === "route" && !rejectEdge.get(str(s["id"]))) {
      v.add(Code.ApprovalRejectEdgeRequired, `steps[${i}].config.on_reject`, `"route" reject routing requires at least one outgoing edge with "decision": "reject"`);
    }
  });
}

// ── graph ───────────────────────────────────────────────────────────────────

function checkGraph(v: Validator, def: Record<string, unknown>, stepIndex: Map<string, number>): void {
  const steps = arr(def["steps"]);
  if (steps.length === 0) return;
  const inDegree = new Map<string, number>();
  const touched = new Set<string>();
  const outEdges = new Map<string, number[]>();
  arr(def["edges"]).forEach((raw, i) => {
    const e = obj(raw);
    const from = str(e["from"]);
    const to = str(e["to"]);
    const fromOK = stepIndex.has(from);
    const toOK = stepIndex.has(to);
    if (fromOK && toOK) {
      touched.add(from);
      touched.add(to);
    }
    if (isLoopEdge(e)) return;
    if (fromOK) {
      const list = outEdges.get(from) ?? [];
      list.push(i);
      outEdges.set(from, list);
    }
    if (fromOK && toOK) inDegree.set(to, (inDegree.get(to) ?? 0) + 1);
  });

  let entry = false;
  for (const s of steps) {
    if ((inDegree.get(str(obj(s)["id"])) ?? 0) === 0) {
      entry = true;
      break;
    }
  }
  if (!entry) v.add(Code.NoEntryStep, "", "no entry step: every step has an incoming normal edge");

  if (steps.length > 1) {
    steps.forEach((raw, i) => {
      const id = str(obj(raw)["id"]);
      if (!touched.has(id)) v.warn(Code.IsolatedStep, `steps[${i}]`, `step ${JSON.stringify(id)} has no edges; it will run as an independent entry step`);
    });
  }

  steps.forEach((raw, i) => {
    const s = obj(raw);
    if (str(s["type"]) !== "branch") return;
    const id = str(s["id"]);
    const outs = outEdges.get(id) ?? [];
    if (outs.length === 0) {
      v.add(Code.BranchNoOutEdges, `steps[${i}]`, `branch step ${JSON.stringify(id)} has no outgoing edges`);
      return;
    }
    outs.forEach((ei, pos) => {
      if (str(obj(arr(def["edges"])[ei])["when"]) === "" && pos !== outs.length - 1) {
        v.add(Code.BranchEdgeUnconditioned, `edges[${ei}]`, `unconditioned out-edge of branch ${JSON.stringify(id)} must be the single trailing default`);
      }
    });
  });
}

function checkGraphSemantics(v: Validator, def: Record<string, unknown>, g: Graph): void {
  const steps = arr(def["steps"]);
  for (const c of g.findCycles()) {
    const related = c.path.map((id) => `steps[${g.index.get(id)}]`).concat(`edges[${c.edgeIdx}]`);
    v.add(Code.Cycle, `edges[${c.edgeIdx}]`, `edge closes the cycle ${pathString(c)} (mark a loop edge instead: only loop edges may form cycles)`, related);
  }
  for (const ei of g.loopEdges) {
    const e = obj(arr(def["edges"])[ei]);
    const from = str(e["from"]);
    const to = str(e["to"]);
    const toIdx = g.index.get(to);
    const fromIdx = g.index.get(from);
    if (toIdx === undefined || fromIdx === undefined || !g.reaches(toIdx, fromIdx)) {
      v.add(Code.LoopEdgeNotAncestor, `edges[${ei}]`, `loop edge target ${JSON.stringify(to)} is not an ancestor of source ${JSON.stringify(from)} (no loop body to iterate)`);
      continue;
    }
    const np = e["no_progress"];
    if (isPlainObject(np) && str(np["step"]) !== "") {
      const body = loopBody(g, from, to);
      if (body && !body.has(str(np["step"]))) {
        v.add(Code.ConfigFieldInvalid, `edges[${ei}].no_progress.step`, `step ${JSON.stringify(str(np["step"]))} is not a member of the loop body of ${JSON.stringify(to)} → ${JSON.stringify(from)}`);
      }
    }
  }
  void steps;
}

// loopBody is the normal-edge span from `to` to `from` (the cloned iterations).
function loopBody(g: Graph, from: string, to: string): Set<string> | null {
  const toIdx = g.index.get(to);
  const fromIdx = g.index.get(from);
  if (toIdx === undefined || fromIdx === undefined) return null;
  // Descendants of `to` (inclusive) that are ancestors of `from` (inclusive).
  const desc = descendantsInclusive(g, toIdx);
  const anc = new Set(g.ancestors(from));
  anc.add(from);
  const body = new Set<string>();
  for (const id of desc) if (anc.has(id)) body.add(id);
  return body;
}

function descendantsInclusive(g: Graph, start: number): Set<string> {
  const out = new Set<string>([g.steps[start]!.id]);
  const queue = [start];
  const visited = new Array<boolean>(g.steps.length).fill(false);
  visited[start] = true;
  while (queue.length > 0) {
    const s = queue.shift()!;
    for (const e of g.outNormal[s]!) {
      const t = g.index.get(g.edges[e]!.to)!;
      if (!visited[t]) {
        visited[t] = true;
        out.add(g.steps[t]!.id);
        queue.push(t);
      }
    }
  }
  return out;
}

function checkContextGraph(v: Validator, def: Record<string, unknown>, g: Graph): void {
  arr(def["steps"]).forEach((raw, i) => {
    const s = obj(raw);
    const cs = s["context"];
    if (!isPlainObject(cs)) return;
    const id = str(s["id"]);
    let ancestors: Set<string> | null = null;
    arr(cs["sources"]).forEach((src, j) => {
      const so = obj(src);
      if (so["kind"] !== "step_output" || str(so["step"]) === "") return;
      const path = `steps[${i}].context.sources[${j}].step`;
      ancestors ??= g.ancestors(id);
      const step = str(so["step"]);
      if (!g.index.has(step)) v.add(Code.ContextFieldInvalid, path, `step_output source names unknown step ${JSON.stringify(step)}`);
      else if (step === id) v.add(Code.ContextFieldInvalid, path, "step_output source: a step cannot reference its own output");
      else if (!ancestors.has(step)) v.add(Code.ContextFieldInvalid, path, `step_output source: step ${JSON.stringify(step)} is not upstream of ${JSON.stringify(id)} (no normal-edge path)`);
    });
  });
}

// ── template ref lint (config `${{ }}`) ──────────────────────────────────────

function checkTemplates(v: Validator, def: Record<string, unknown>, g: Graph): void {
  arr(def["steps"]).forEach((raw, i) => {
    const s = obj(raw);
    const config = s["config"];
    if (config === undefined || config === null) return;
    const base = `steps[${i}].config`;
    const { refs, parseError } = scanConfigRefs(config);
    if (parseError) {
      v.add(Code.TemplateInvalid, base, parseError);
      return;
    }
    lintRefs(v, def, g, str(s["id"]), base, refs, false);
  });
}

function lintRefs(
  v: Validator,
  def: Record<string, unknown>,
  g: Graph,
  stepID: string,
  base: string,
  refs: ConfigRef[],
  inBody: boolean,
): void {
  let ancestors: Set<string> | null = null;
  for (const r of refs) {
    const path = r.configPath === "" ? base : `${base}.${r.configPath}`;
    if (r.itemRef) {
      if (!inBody) v.add(Code.TemplateRefInvalid, path, `reference ${JSON.stringify(r.raw)}: item / item_index are only valid inside a map sub-template (templates section)`);
      continue;
    }
    if (r.stepID !== "") {
      ancestors ??= g.ancestors(stepID);
      if (!g.index.has(r.stepID)) v.add(Code.TemplateRefUnknownStep, path, `reference ${JSON.stringify(r.raw)} names unknown step ${JSON.stringify(r.stepID)}`);
      else if (r.stepID === stepID) v.add(Code.TemplateRefNotUpstream, path, `reference ${JSON.stringify(r.raw)}: a step cannot reference its own output`);
      else if (!ancestors.has(r.stepID)) v.add(Code.TemplateRefNotUpstream, path, `reference ${JSON.stringify(r.raw)}: step ${JSON.stringify(r.stepID)} is not upstream of ${JSON.stringify(stepID)} (no normal-edge path)`);
    } else if (r.paramKey !== "") {
      const params = obj(def["params"]);
      if (!(r.paramKey in params)) v.add(Code.TemplateRefUnknownParam, path, `reference ${JSON.stringify(r.raw)} names undeclared run parameter ${JSON.stringify(r.paramKey)}`);
    }
  }
}

// ── templates / maps / agents ────────────────────────────────────────────────

function sortedKeys(m: Record<string, unknown>): string[] {
  return Object.keys(m).sort();
}

function checkTemplateSection(v: Validator, def: Record<string, unknown>): void {
  const templates = obj(def["templates"]);
  for (const name of sortedKeys(templates)) {
    const t = obj(templates[name]);
    const base = `templates.${name}`;
    if (!STEP_ID_RE.test(name)) v.add(Code.TemplateSectionInvalid, base, `template name ${JSON.stringify(name)} does not match ${STEP_ID_RE_TEXT}`);
    const steps = arr(t["steps"]);
    if (steps.length === 0) {
      v.add(Code.TemplateSectionInvalid, `${base}.steps`, "a map sub-template must have at least one step");
      continue;
    }
    const index = new Map<string, number>();
    let wellFormed = true;
    steps.forEach((raw, i) => {
      const s = obj(raw);
      const sp = `${base}.steps[${i}]`;
      const id = str(s["id"]);
      if (!STEP_ID_RE.test(id)) v.add(Code.InvalidStepID, `${sp}.id`, `step ID ${JSON.stringify(id)} does not match ${STEP_ID_RE_TEXT}`);
      if (index.has(id)) {
        v.add(Code.DuplicateStepID, `${sp}.id`, `duplicate step ID ${JSON.stringify(id)} (first at steps[${index.get(id)}])`);
        wellFormed = false;
      } else index.set(id, i);
      const type = str(s["type"]);
      if (type === "map") v.add(Code.TemplateSectionInvalid, `${sp}.type`, "a map sub-template body may not itself contain a map step");
      else if (type === "gather") v.add(Code.TemplateSectionInvalid, `${sp}.type`, "a map sub-template body may not contain a gather step");
      checkStepConfig(v, sp, s);
      checkModelFallbacks(v, def, sp, s);
      checkOutputFormat(v, sp, s);
      checkRetry(v, sp, s["retry"]);
      checkTimeout(v, sp, str(s["timeout"]));
      checkCache(v, sp, s["cache"]);
      checkStepBudget(v, sp, type, s["budget"]);
      checkValidation(v, sp, s);
      checkBlackboard(v, sp, s["blackboard"]);
      checkContext(v, sp, s);
    });
    const edges = arr(t["edges"]);
    edges.forEach((raw, i) => {
      const e = obj(raw);
      const ep = `${base}.edges[${i}]`;
      if (!index.has(str(e["from"]))) {
        v.add(Code.UnknownEdgeEndpoint, `${ep}.from`, `edge names unknown step ${JSON.stringify(str(e["from"]))} (not in this template)`);
        wellFormed = false;
      }
      if (!index.has(str(e["to"]))) {
        v.add(Code.UnknownEdgeEndpoint, `${ep}.to`, `edge names unknown step ${JSON.stringify(str(e["to"]))} (not in this template)`);
        wellFormed = false;
      }
    });
    if (wellFormed) {
      const gSteps = steps.map((s) => ({ id: str(obj(s)["id"]) }));
      const gEdges: GraphEdge[] = edges.map((e) => {
        const eo = obj(e);
        return { from: str(eo["from"]), to: str(eo["to"]), loop: isLoopEdge(eo) };
      });
      const g = new Graph(gSteps, gEdges);
      checkTemplateGraph(v, def, base, steps, edges, g);
    }
  }
}

function checkTemplateGraph(
  v: Validator,
  def: Record<string, unknown>,
  base: string,
  steps: unknown[],
  edges: unknown[],
  g: Graph,
): void {
  for (const c of g.findCycles()) {
    v.add(Code.Cycle, `${base}.edges[${c.edgeIdx}]`, `edge closes the cycle ${pathString(c)} (mark a loop edge instead: only loop edges may form cycles)`);
  }
  for (const ei of g.loopEdges) {
    const e = obj(edges[ei]);
    const toIdx = g.index.get(str(e["to"]));
    const fromIdx = g.index.get(str(e["from"]));
    if (toIdx === undefined || fromIdx === undefined || !g.reaches(toIdx, fromIdx)) {
      v.add(Code.LoopEdgeNotAncestor, `${base}.edges[${ei}]`, `loop edge target ${JSON.stringify(str(e["to"]))} is not an ancestor of source ${JSON.stringify(str(e["from"]))} (no loop body to iterate)`);
    }
  }
  const sinks: string[] = [];
  steps.forEach((raw, i) => {
    if (g.outNormal[i]!.length === 0) sinks.push(str(obj(raw)["id"]));
  });
  if (sinks.length !== 1) {
    v.add(Code.TemplateSectionInvalid, base, `a map sub-template must have exactly one sink (a step with no outgoing normal edge, whose output the gather collects); found ${sinks.length} ${JSON.stringify(sinks).replace(/"/g, "")}`);
  }
  checkTemplateRefs(v, def, base, steps, g);
}

function checkTemplateRefs(v: Validator, def: Record<string, unknown>, base: string, steps: unknown[], g: Graph): void {
  steps.forEach((raw, i) => {
    const s = obj(raw);
    const config = s["config"];
    if (config === undefined || config === null) return;
    const stepBase = `${base}.steps[${i}].config`;
    const { refs, parseError } = scanConfigRefs(config);
    if (parseError) {
      v.add(Code.TemplateInvalid, stepBase, parseError);
      return;
    }
    lintRefs(v, def, g, str(s["id"]), stepBase, refs, true);
  });
}

function checkMaps(v: Validator, def: Record<string, unknown>): void {
  const templates = obj(def["templates"]);
  arr(def["steps"]).forEach((raw, i) => {
    const s = obj(raw);
    if (str(s["type"]) !== "map") return;
    const c = obj(s["config"]);
    const body = str(c["body"]);
    if (body === "") return;
    const tmpl = templates[body];
    if (tmpl === undefined) {
      v.add(Code.MapBodyUnknown, `steps[${i}].config.body`, `map body ${JSON.stringify(body)} names no template in the definition's templates section`);
      return;
    }
    if (c["on_item_failure"] === "collect_errors") {
      const steps = arr(obj(tmpl)["steps"]);
      if (steps.length > 1) v.add(Code.ConfigFieldInvalid, `steps[${i}].config.on_item_failure`, `collect_errors requires a single-step body, but ${JSON.stringify(body)} has ${steps.length} steps`);
    }
  });
}

function checkAgents(v: Validator, def: Record<string, unknown>): void {
  const agents = obj(def["agents"]);
  for (const name of sortedKeys(agents)) {
    const role = obj(agents[name]);
    const path = `agents.${name}`;
    if (!PLUGIN_NAME_RE.test(name)) v.add(Code.AgentSectionInvalid, path, `agent name ${JSON.stringify(name)} does not match ${PLUGIN_NAME_RE_TEXT}`);
    arr(role["tools"]).forEach((t, i) => {
      if (!PLUGIN_NAME_RE.test(str(t))) v.add(Code.AgentSectionInvalid, `${path}.tools[${i}]`, `tool name ${JSON.stringify(str(t))} does not match ${PLUGIN_NAME_RE_TEXT}`);
    });
    // Validate the role's defaults as a synthetic llm step.
    const syn = {
      type: "llm",
      config: {
        model: role["model"],
        model_fallbacks: role["model_fallbacks"],
        output_format: role["output_format"],
        max_tokens: role["max_tokens"],
        temperature: role["temperature"],
      },
      validation: role["validation"],
      context: role["context"],
    };
    checkModelFallbacks(v, def, path, syn);
    checkOutputFormat(v, path, syn);
    checkValidation(v, path, syn);
    checkContext(v, path, syn);
  }

  arr(def["steps"]).forEach((raw, i) => {
    const s = obj(raw);
    if (str(s["type"]) !== "agent") return;
    const c = obj(s["config"]);
    const ref = str(c["agent"]);
    if (ref === "") return; // reported by checkStepConfig
    const path = `steps[${i}]`;
    if (!(ref in agents)) {
      v.add(Code.AgentRefUnknown, `${path}.config.agent`, `agent ${JSON.stringify(ref)} names no agent in the definition's agents section`);
      return;
    }
    const merged = resolveAgentStep(def, c);
    if (!merged) return;
    checkLLMConfig(v, path, merged.model, merged.prompt, merged.messages);
    const msyn = {
      type: "llm",
      config: { model: merged.model, model_fallbacks: merged.modelFallbacks, output_format: merged.outputFormat },
      budget: s["budget"],
    };
    checkModelFallbacks(v, def, path, msyn);
    checkOutputFormat(v, path, msyn);
  });
}
