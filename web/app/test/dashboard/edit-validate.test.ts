import { describe, expect, it } from "vitest";
import { validateEditedPayload } from "@/lib/pure/dashboard/edit-validate";

const schema = { type: "object", properties: { text: { type: "string" } }, required: ["text"] };

describe("validateEditedPayload", () => {
  it("accepts a valid payload", () => {
    const r = validateEditedPayload('{"text":"hi"}', schema);
    expect(r.ok).toBe(true);
    if (r.ok) expect(r.value).toEqual({ text: "hi" });
  });

  it("reports a JSON parse error at the root", () => {
    const r = validateEditedPayload("{not json", schema);
    expect(r.ok).toBe(false);
    if (!r.ok) expect(r.issues[0]!.path).toBe("");
  });

  it("enforces required (unlike graphdef checkShape)", () => {
    const r = validateEditedPayload("{}", schema);
    expect(r.ok).toBe(false);
    if (!r.ok) expect(r.issues.some((i) => /required/.test(i.message) && i.path === "/text")).toBe(true);
  });

  it("reports a wrong type with an RFC-6901 pointer", () => {
    const r = validateEditedPayload('{"text":42}', schema);
    expect(r.ok).toBe(false);
    if (!r.ok) expect(r.issues[0]!).toMatchObject({ path: "/text", message: expect.stringContaining("expected string") });
  });

  it("allows extra properties by default (JSON-Schema semantics)", () => {
    const r = validateEditedPayload('{"text":"hi","extra":1}', schema);
    expect(r.ok).toBe(true);
  });

  it("forbids extra props when additionalProperties:false", () => {
    const strict = { ...schema, additionalProperties: false };
    const r = validateEditedPayload('{"text":"hi","extra":1}', strict);
    expect(r.ok).toBe(false);
    if (!r.ok) expect(r.issues[0]!.path).toBe("/extra");
  });

  it("enforces enum", () => {
    const enumSchema = { type: "object", properties: { c: { enum: ["a", "b"] } } };
    expect(validateEditedPayload('{"c":"a"}', enumSchema).ok).toBe(true);
    expect(validateEditedPayload('{"c":"z"}', enumSchema).ok).toBe(false);
  });

  it("no schema ⇒ any JSON accepted", () => {
    expect(validateEditedPayload('{"anything":true}', undefined).ok).toBe(true);
  });
});
