/**
 * Client-side pre-check of an edited approval payload against a gate's
 * `edit_schema` (ticket 18.5).
 *
 * This is a *pre-check*, not the authority: the backend validates an
 * `edited_payload` through the built-in `validate.JSONSchema` validator (a real
 * JSON-Schema-2020-12 validator), and its 422 `approval_decision_invalid`
 * `issues[]` (RFC-6901 pointers) are the final word. The dialog surfaces those
 * verbatim. This local walker only catches the common mistakes early (bad JSON,
 * a missing required field, a wrong type) so the operator gets instant
 * feedback; it deliberately mirrors JSON-Schema *validator* semantics (NOT the
 * Go strict-decoder / graphdef `checkShape`), so:
 *   - additional properties are ALLOWED unless the schema says `false`
 *     (the JSON-Schema default), and
 *   - `required` and `enum` ARE enforced.
 *
 * It supports the subset a hand-authored `edit_schema` realistically uses
 * (object/properties/required/type/enum/items). Anything it does not recognise
 * is treated as valid — the server catches the rest. Issue paths are RFC-6901
 * pointers, matching the backend's issue paths so the two agree on location.
 *
 * No React/UI imports — under the app's `pure/` eslint boundary.
 */

export interface EditIssue {
  /** RFC-6901 pointer into the edited payload (matches the backend's issues). */
  path: string;
  message: string;
}

export type EditCheck =
  | { ok: true; value: unknown }
  | { ok: false; issues: EditIssue[] };

/** Parse `text` as JSON and validate it against `editSchema` (if any). */
export function validateEditedPayload(text: string, editSchema: unknown): EditCheck {
  let value: unknown;
  try {
    value = JSON.parse(text);
  } catch (e) {
    const msg = e instanceof Error ? e.message : "invalid JSON";
    return { ok: false, issues: [{ path: "", message: `not valid JSON: ${msg}` }] };
  }
  const issues: EditIssue[] = [];
  if (isSchema(editSchema)) checkNode(value, editSchema, "", issues);
  return issues.length === 0 ? { ok: true, value } : { ok: false, issues };
}

type Schema = Record<string, unknown>;

function isSchema(s: unknown): s is Schema {
  return typeof s === "object" && s !== null && !Array.isArray(s);
}

function jsonType(v: unknown): string {
  if (v === null) return "null";
  if (Array.isArray(v)) return "array";
  if (typeof v === "number") return Number.isInteger(v) ? "integer" : "number";
  return typeof v; // string | boolean | object
}

function typeMatches(want: string, v: unknown): boolean {
  const got = jsonType(v);
  if (want === "number") return got === "number" || got === "integer";
  return got === want;
}

function checkNode(value: unknown, schema: Schema, path: string, out: EditIssue[]): void {
  const type = schema.type;
  if (typeof type === "string" && !typeMatches(type, value)) {
    out.push({ path, message: `expected ${type}, got ${jsonType(value)}` });
    return; // a wrong type makes deeper checks noise
  }

  if (Array.isArray(schema.enum) && !schema.enum.some((e) => deepEqual(e, value))) {
    out.push({ path, message: `value is not one of the allowed options` });
  }

  if (isSchema(value)) {
    const props = isSchema(schema.properties) ? schema.properties : undefined;
    const required = Array.isArray(schema.required) ? schema.required : [];
    for (const key of required) {
      if (typeof key === "string" && !(key in (value as Schema))) {
        out.push({ path: `${path}/${escapePtr(key)}`, message: `missing required field "${key}"` });
      }
    }
    // additionalProperties: only enforced when explicitly `false`.
    const addl = schema.additionalProperties;
    if (props) {
      for (const [key, raw] of Object.entries(value as Schema)) {
        const child = props[key];
        if (child === undefined) {
          if (addl === false) {
            out.push({ path: `${path}/${escapePtr(key)}`, message: `unknown field "${key}"` });
          }
          continue;
        }
        if (isSchema(child)) checkNode(raw, child, `${path}/${escapePtr(key)}`, out);
      }
    }
  }

  if (Array.isArray(value) && isSchema(schema.items)) {
    value.forEach((item, i) => checkNode(item, schema.items as Schema, `${path}/${i}`, out));
  }
}

function escapePtr(key: string): string {
  return key.replace(/~/g, "~0").replace(/\//g, "~1");
}

function deepEqual(a: unknown, b: unknown): boolean {
  return JSON.stringify(a) === JSON.stringify(b);
}
