/**
 * gen-events-types.ts — generate `src/generated/events.ts` from the backend's
 * committed event-feed JSON Schema (`docs/schema/events.v1.json`, ADR-018).
 *
 * The Go event structs are the source of truth; `make generate` emits the JSON
 * Schema from them (CI-diffed), and this script emits the TypeScript from that
 * schema. So the TS event types cannot drift from what the engine writes — the
 * committed `events.ts` is CI-diffed against a fresh run of this generator.
 *
 * The emitter is a small, deterministic walker over the exact vocabulary the
 * invopop reflector produces for the event package: object/enum/primitive/array
 * defs, `$ref`, `const`, and `true` (any). It has no external dependency, so its
 * output is byte-stable across environments — which is what a drift check needs.
 *
 * Run: `pnpm generate` (from web/lib/engine-client).
 */
import { readFileSync, writeFileSync } from "node:fs";
import { dirname, resolve } from "node:path";
import { fileURLToPath } from "node:url";

const here = dirname(fileURLToPath(import.meta.url));
const SCHEMA_PATH = resolve(here, "../../../../docs/schema/events.v1.json");
const OUT_PATH = resolve(here, "../src/generated/events.ts");

// JSON Schema (draft 2020-12) subset the event schema uses.
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
  format?: string;
}

/**
 * Def-name overrides. `UUID` is reflected as a 16-byte array (`[16]byte`), but
 * google/uuid marshals it to a UUID string on the wire — the client consumes
 * the wire, so it is a `string`.
 */
const DEF_OVERRIDES: Record<string, string> = {
  UUID: "string",
};

const ENVELOPE = "Envelope";

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
  return `{ ${parts.join("; ")} }`;
}

/** Emit a named top-level definition (interface | type alias). */
function renderDef(name: string, s: Schema): string {
  if (DEF_OVERRIDES[name]) {
    return `export type ${name} = ${DEF_OVERRIDES[name]};\n`;
  }
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
  // Primitive / array / any alias (e.g. an overridable string alias).
  return `export type ${name} = ${tsType(s)};\n`;
}

function main(): void {
  const raw = readFileSync(SCHEMA_PATH, "utf8");
  const schema = JSON.parse(raw) as Schema;
  const defs = schema.$defs;
  if (!defs) throw new Error("event schema has no $defs");

  const envelope = defs[ENVELOPE];
  if (!envelope || envelope === true) throw new Error("event schema has no Envelope def");

  // Event-type vocabulary + per-type payload mapping, both from the Envelope.
  const typeEnum = (envelope.properties?.["type"] as Schema | undefined)?.enum;
  if (!typeEnum) throw new Error("Envelope.type has no enum");
  const variants = envelope.oneOf;
  if (!variants) throw new Error("Envelope has no oneOf variants");

  const payloadByType = new Map<string, string>();
  for (const v of variants) {
    const t = (v.properties?.["type"] as Schema | undefined)?.const;
    const p = v.properties?.["payload"] as Schema | undefined;
    if (!t || !p?.$ref) throw new Error("malformed Envelope oneOf variant");
    payloadByType.set(t, refName(p.$ref));
  }
  for (const t of typeEnum) {
    if (!payloadByType.has(t)) throw new Error(`event type ${t} has no payload variant`);
  }

  const out: string[] = [];
  out.push("// Code generated from docs/schema/events.v1.json — DO NOT EDIT.");
  out.push("//");
  out.push("// Regenerate with `pnpm generate` (web/lib/engine-client). The Go event");
  out.push("// structs are the source of truth (ADR-018): `make generate` emits the JSON");
  out.push("// Schema, this file is emitted from it, and CI fails if it is stale.");
  out.push("");

  // Named definitions (everything except the Envelope, in schema order).
  for (const name of Object.keys(defs)) {
    if (name === ENVELOPE) continue;
    const d = defs[name];
    if (d === undefined) continue;
    if (d === true) {
      out.push(`export type ${name} = unknown;\n`);
      continue;
    }
    out.push(renderDef(name, d));
  }

  // Event-type vocabulary as a runtime const + a union type.
  out.push("/** Every event type in the feed vocabulary (ADR-018), in catalog order. */");
  out.push("export const EVENT_TYPES = [");
  for (const t of typeEnum) out.push(`  ${JSON.stringify(t)},`);
  out.push("] as const;");
  out.push("");
  out.push("export type EventType = (typeof EVENT_TYPES)[number];");
  out.push("");

  // Per-type payload map.
  out.push("/** Maps each event type to its payload struct. */");
  out.push("export interface EventPayloadMap {");
  for (const t of typeEnum) out.push(`  ${key(t)}: ${payloadByType.get(t)};`);
  out.push("}");
  out.push("");

  // The discriminated envelope: switching on `type` narrows `payload`.
  out.push("/** Envelope fields shared by every event, minus the discriminated pair. */");
  out.push("export interface EventEnvelopeBase {");
  const envProps = envelope.properties ?? {};
  const envReq = new Set(envelope.required ?? []);
  for (const p of Object.keys(envProps)) {
    if (p === "type" || p === "payload") continue;
    const opt = envReq.has(p) ? "" : "?";
    out.push(`  ${key(p)}${opt}: ${tsType(envProps[p]!)};`);
  }
  out.push("}");
  out.push("");
  out.push("type EnvelopeFor<T extends EventType> = EventEnvelopeBase & {");
  out.push("  type: T;");
  out.push("  payload: EventPayloadMap[T];");
  out.push("};");
  out.push("");
  out.push("/**");
  out.push(" * One normalized event envelope. A union over every event type, discriminated");
  out.push(" * by `type` — `switch (env.type)` narrows `env.payload` to the matching struct.");
  out.push(" */");
  out.push("export type EventEnvelope = { [K in EventType]: EnvelopeFor<K> }[EventType];");
  out.push("");

  writeFileSync(OUT_PATH, out.join("\n"));
  process.stdout.write(`wrote ${OUT_PATH}\n`);
}

main();
