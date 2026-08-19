/**
 * Pure permission logic (ticket 18.6): the scope model the dashboard uses to
 * render permission-gated controls. The browser learns its key's scopes from
 * `GET /v1/auth/whoami`; the server still enforces every scope on the
 * underlying route, so this is purely a UX affordance (hide/disable a control
 * the key cannot use) — never the authorization itself.
 *
 * No React/UI imports — under the app's `pure/` eslint boundary.
 */
import type { Scope } from "@agentloom/api-client";

/**
 * The permission state the provider resolves:
 *   - "loading": whoami is in flight — controls disabled until known;
 *   - "ready": scopes known — hide controls the key cannot use;
 *   - "unavailable": whoami failed (unconfigured/offline) — render controls
 *     enabled and let the server's 403 surface inline (fail-open for UX, never
 *     for authz).
 */
export type PermissionStatus = "loading" | "ready" | "unavailable";

export interface Permissions {
  status: PermissionStatus;
  scopes: Scope[];
}

/** Does a granted scope set include `want`? `admin` implies every scope. */
export function hasScope(scopes: readonly Scope[], want: Scope): boolean {
  return scopes.includes("admin") || scopes.includes(want);
}

/**
 * `can(want)` on a resolved Permissions: true when the key definitely holds the
 * scope, OR when scopes are unknown (loading/unavailable) — in the unknown case
 * the control renders (disabled while loading, enabled when unavailable) and the
 * server stays the authority. Only a "ready" state with a missing scope is a
 * definite "cannot".
 */
export function can(perms: Permissions, want: Scope): boolean {
  if (perms.status === "ready") return hasScope(perms.scopes, want);
  return true;
}

/** How a scope-gated control should render for a required scope. */
export type ControlState = "hidden" | "disabled" | "enabled";

/**
 * Resolve a control's render state:
 *   - loading  → "disabled" (scopes unknown yet; don't flash-then-hide);
 *   - ready + has scope → "enabled";
 *   - ready + missing scope → "hidden" (a definite "cannot");
 *   - unavailable → "enabled" (server enforces; 403 surfaces inline).
 */
export function controlState(perms: Permissions, want: Scope): ControlState {
  switch (perms.status) {
    case "loading":
      return "disabled";
    case "unavailable":
      return "enabled";
    case "ready":
      return hasScope(perms.scopes, want) ? "enabled" : "hidden";
  }
}

/** The scope each run/ops control requires. */
export const CONTROL_SCOPES = {
  cancel: "submit",
  park: "submit",
  unpark: "submit",
  requeue: "submit",
  raiseBudget: "submit",
  decide: "approve",
} as const satisfies Record<string, Scope>;

export type ControlName = keyof typeof CONTROL_SCOPES;

/** The render state of a named control under the current permissions. */
export function controlStateFor(perms: Permissions, control: ControlName): ControlState {
  return controlState(perms, CONTROL_SCOPES[control]);
}
