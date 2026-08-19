/**
 * gen-definition-types.ts — generate `src/generated/definition.ts` from the
 * backend's committed workflow-definition JSON Schema
 * (`docs/schema/workflow-definition.v1.json`, ADR-003).
 *
 * The Go dag structs are the source of truth; `make generate` emits the JSON
 * Schema from them (CI-diffed), and this script emits the TypeScript from that
 * schema. So the TS definition types cannot drift from the contract — the
 * committed `definition.ts` is CI-diffed against a fresh run of this generator
 * (the `web` job's `git diff --exit-code -- lib/graphdef/src/generated`).
 *
 * The emitter is a small, deterministic walker over the exact vocabulary the
 * invopop reflector produces for the dag package: object/enum/primitive/array
 * defs, `$ref`, `const`, `additionalProperties: {$ref}` maps, and `true` (any).
 * It has no external dependency, so its output is byte-stable across
 * environments — which is what a drift check needs. The one construct beyond the
 * event emitter is `Step`, whose `oneOf` binds each step `type` to its config
 * shape; it is rendered as a discriminated union so `switch (step.type)` narrows
 * `step.config`.
 *
 * We do NOT reuse api-client's generated Definition type: openapi-typescript
 * renders the Step oneOf as a base whose `config` collapses to
 * `Record<string, never>`, which a config editor cannot use (ADR-019).
 *
 * Run: `pnpm generate` (from web/lib/graphdef).
 */
import { readFileSync, writeFileSync } from "node:fs";
import { dirname, resolve } from "node:path";
import { fileURLToPath } from "node:url";

const here = dirname(fileURLToPath(import.meta.url));
const SCHEMA_PATH = resolve(here, "../../../../docs/schema/workflow-definition.v1.json");
const OUT_PATH = resolve(here, "../src/generated/definition.ts");
const SCHEMA_OUT_PATH = resolve(here, "../src/generated/definition.schema.ts");

// JSON Schema (draft 2020-12) subset the definition schema uses.
interface Schema {
  $ref?: string;
  $defs?: Record<string, Schema | true>;
  type?: string;
  properties?: Record<string, Schema | true>;
  required?: string[];
  additionalProperties?: boolean | Schema;
  enum?: string[];
  const?: string;
  items?: Schema | true;
  oneOf?: Schema[];
  description?: string;
}

const STEP = "Step";

function isValidIdent(name: string): boolean {
  return /^[A-Za-z_$][A-Za-z0-9_$]*$/.test(name);
}

function key(name: string): string {
  return isValidIdent(name) ? name : JSON.stringify(name);
}

function refName(ref: string): string {
  const m = /^#\/\$defs\/(.+)$/.exec(ref);
  if (!m) throw new Error(`unsupported $ref: ${ref}`);
  return m[1]!;
}

/** Render an inline TS type for a value schema (used for properties/items). */
function tsType(s: Schema | true): string {
  if (s === true) return "unknown";
  if (s.$ref) return refName(s.$ref);
  if (s.const !== undefined) return JSON.stringify(s.const);
  if (s.enum) return s.enum.map((v) => JSON.stringify(v)).join(" | ");
  switch (s.type) {
    case "string":
      return "string";
    case "integer":
    case "number":
      return "number";
    case "boolean":
      return "boolean";
    case "array": {
      const inner = tsType(s.items ?? true);
      return /[ |&]/.test(inner) ? `Array<${inner}>` : `${inner}[]`;
    }
    case "object": {
      if (s.properties) return renderObjectLiteral(s);
      if (s.additionalProperties && s.additionalProperties !== true) {
        return `Record<string, ${tsType(s.additionalProperties)}>`;
      }
      if (s.additionalProperties === false) return "Record<string, never>";
      return "Record<string, unknown>";
    }
    default:
      // A schema with no `type` and no combinator (e.g. `"schema": true` written
      // as `{}`) is the any-schema.
      return "unknown";
  }
}

function renderObjectLiteral(s: Schema): string {
  const required = new Set(s.required ?? []);
  const props = s.properties ?? {};
  const parts = Object.keys(props).map((name) => {
    const opt = required.has(name) ? "" : "?";
    return `${key(name)}${opt}: ${tsType(props[name]!)}`;
  });
  return parts.length === 0 ? "Record<string, never>" : `{ ${parts.join("; ")} }`;
}

/** Emit a named top-level definition (interface | type alias). */
function renderDef(name: string, s: Schema): string {
  if (s.enum) {
    const union = s.enum.map((v) => JSON.stringify(v)).join(" | ");
    return `export type ${name} = ${union};\n`;
  }
  if (s.type === "object" && s.properties) {
    const required = new Set(s.required ?? []);
    const props = s.properties;
    const lines = Object.keys(props).map((p) => {
      const opt = required.has(p) ? "" : "?";
      return `  ${key(p)}${opt}: ${tsType(props[p]!)};`;
    });
    return `export interface ${name} {\n${lines.join("\n")}\n}\n`;
  }
  // Primitive / array / any alias.
  return `export type ${name} = ${tsType(s)};\n`;
}

/**
 * Render the `Step` def. Its `oneOf` variants each bind `type: <const>` to a
 * `config: $ref`, and the base Step carries the shared fields (id, retry,
 * timeout, envelope blocks, …) with `config` erased on the base. We emit:
 *   - StepBase   — every base property except `type` and `config`
 *   - StepConfigMap — { <type>: <ConfigRef> } from the variants
 *   - StepType   — the union of the variant consts
 *   - Step       — StepBase & the discriminated { type; config? } pair
 */
function renderStep(defs: Record<string, Schema | true>): string {
  const step = defs[STEP];
  if (!step || step === true || !step.oneOf) {
    throw new Error("definition schema has no Step oneOf");
  }
  // The base object carries the shared step fields. The reflector emits them on
  // the enclosing Step object alongside the oneOf; find them there.
  const baseProps = step.properties ?? {};
  const baseReq = new Set(step.required ?? []);

  const configByType = new Map<string, string>();
  for (const v of step.oneOf) {
    const t = (v.properties?.["type"] as Schema | undefined)?.const;
    const c = v.properties?.["config"] as Schema | undefined;
    if (!t) throw new Error("malformed Step oneOf variant (no type const)");
    // A variant may legally omit config (a config-less step type).
    configByType.set(t, c?.$ref ? refName(c.$ref) : "Record<string, never>");
  }
  const types = [...configByType.keys()];

  const out: string[] = [];

  // StepBase — shared fields minus type/config.
  const baseLines = Object.keys(baseProps)
    .filter((p) => p !== "type" && p !== "config")
    .map((p) => {
      const opt = baseReq.has(p) ? "" : "?";
      return `  ${key(p)}${opt}: ${tsType(baseProps[p]!)};`;
    });
  out.push(`export interface StepBase {\n${baseLines.join("\n")}\n}\n`);

  // The discriminant vocabulary as a runtime array (StepType itself is the
  // enum `$def`, already emitted above — we do not redeclare it).
  out.push("/** Every step type in the catalog (ADR-003), in Step-oneOf order. */");
  out.push("export const STEP_TYPES: readonly StepType[] = [");
  for (const t of types) out.push(`  ${JSON.stringify(t)},`);
  out.push("];");
  out.push("");

  // StepConfigMap — type → config shape.
  out.push("/** Maps each step type to its typed config shape. */");
  out.push("export interface StepConfigMap {");
  for (const t of types) out.push(`  ${key(t)}: ${configByType.get(t)};`);
  out.push("}");
  out.push("");

  // Step — the discriminated union.
  out.push("/**");
  out.push(" * One workflow step. A discriminated union over StepType: switching on");
  out.push(" * `step.type` narrows `step.config` to the matching config shape.");
  out.push(" */");
  out.push(
    "export type Step = { [K in StepType]: StepBase & { type: K; config?: StepConfigMap[K] } }[StepType];",
  );
  out.push("");

  return out.join("\n");
}

function main(): void {
  const raw = readFileSync(SCHEMA_PATH, "utf8");
  const schema = JSON.parse(raw) as Schema;
  const defs = schema.$defs;
  if (!defs) throw new Error("definition schema has no $defs");

  const out: string[] = [];
  out.push("// Code generated from docs/schema/workflow-definition.v1.json — DO NOT EDIT.");
  out.push("//");
  out.push("// Regenerate with `pnpm generate` (web/lib/graphdef). The Go dag structs are");
  out.push("// the source of truth (ADR-003): `make generate` emits the JSON Schema, this");
  out.push("// file is emitted from it, and CI fails if it is stale.");
  out.push("");

  // Named definitions (everything except Step, in schema order). Step is emitted
  // specially (discriminated union) after the plain defs it references.
  for (const name of Object.keys(defs)) {
    if (name === STEP) continue;
    const d = defs[name];
    if (d === undefined) continue;
    if (d === true) {
      out.push(`export type ${name} = unknown;\n`);
      continue;
    }
    out.push(renderDef(name, d));
  }

  out.push(renderStep(defs));

  writeFileSync(OUT_PATH, out.join("\n"));
  process.stdout.write(`wrote ${OUT_PATH}\n`);

  // Also emit the schema itself as a runtime constant, so graphdef's shape
  // checker (17.4) and the app's offline config-schema fallback have the
  // published JSON Schema without a fetch. Embedded as a JSON string parsed at
  // load (not an object literal) so tsc does no giant literal-type inference
  // and the output is byte-stable — the committed workflow-definition.v1.json
  // is itself deterministic (`make generate`), so this file is CI-diffable too.
  const schemaOut: string[] = [];
  schemaOut.push("// Code generated from docs/schema/workflow-definition.v1.json — DO NOT EDIT.");
  schemaOut.push("//");
  schemaOut.push("// The published definition JSON Schema (draft 2020-12) as a runtime constant,");
  schemaOut.push("// emitted by `pnpm generate` (web/lib/graphdef) alongside definition.ts.");
  schemaOut.push("");
  schemaOut.push("/** The workflow-definition JSON Schema (ADR-003), parsed from its committed source. */");
  schemaOut.push(`export const DEFINITION_SCHEMA = JSON.parse(${JSON.stringify(raw)}) as Record<string, unknown>;`);
  schemaOut.push("");
  writeFileSync(SCHEMA_OUT_PATH, schemaOut.join("\n"));
  process.stdout.write(`wrote ${SCHEMA_OUT_PATH}\n`);
}

main();
