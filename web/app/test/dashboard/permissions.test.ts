import { describe, expect, it } from "vitest";
import {
  can,
  controlState,
  controlStateFor,
  hasScope,
  type Permissions,
} from "@/lib/pure/dashboard/permissions";

const ready = (scopes: Permissions["scopes"]): Permissions => ({ status: "ready", scopes });

describe("hasScope", () => {
  it("matches an explicit scope", () => {
    expect(hasScope(["read", "submit"], "submit")).toBe(true);
    expect(hasScope(["read"], "submit")).toBe(false);
  });
  it("admin implies every scope", () => {
    expect(hasScope(["admin"], "submit")).toBe(true);
    expect(hasScope(["admin"], "approve")).toBe(true);
  });
});

describe("can", () => {
  it("is definite only in the ready state", () => {
    expect(can(ready(["submit"]), "submit")).toBe(true);
    expect(can(ready(["read"]), "submit")).toBe(false);
  });
  it("is permissive while unknown (server enforces)", () => {
    expect(can({ status: "loading", scopes: [] }, "submit")).toBe(true);
    expect(can({ status: "unavailable", scopes: [] }, "submit")).toBe(true);
  });
});

describe("controlState", () => {
  it("disables while loading", () => {
    expect(controlState({ status: "loading", scopes: [] }, "submit")).toBe("disabled");
  });
  it("enables when the scope is held", () => {
    expect(controlState(ready(["submit"]), "submit")).toBe("enabled");
    expect(controlState(ready(["admin"]), "submit")).toBe("enabled");
  });
  it("hides when the scope is definitely missing", () => {
    expect(controlState(ready(["read"]), "submit")).toBe("hidden");
  });
  it("enables (fail-open) when whoami is unavailable", () => {
    expect(controlState({ status: "unavailable", scopes: [] }, "submit")).toBe("enabled");
  });
});

describe("controlStateFor", () => {
  it("maps controls to their scopes", () => {
    // A read-only key hides submit-gated controls but not decide's own gate.
    expect(controlStateFor(ready(["read"]), "cancel")).toBe("hidden");
    expect(controlStateFor(ready(["read"]), "requeue")).toBe("hidden");
    expect(controlStateFor(ready(["read"]), "decide")).toBe("hidden");
    expect(controlStateFor(ready(["submit"]), "cancel")).toBe("enabled");
    // decide needs approve, not submit.
    expect(controlStateFor(ready(["submit"]), "decide")).toBe("hidden");
    expect(controlStateFor(ready(["approve"]), "decide")).toBe("enabled");
  });
});
