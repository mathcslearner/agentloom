"use client";

/**
 * Permissions provider (ticket 18.6). Fetches the caller's own scopes once from
 * `GET /v1/auth/whoami` (through the same-origin proxy — the key stays
 * server-side) and provides them to client components so scope-gated controls
 * render appropriately: disabled while loading, hidden when a scope is
 * definitely missing, enabled otherwise. The server still enforces every scope,
 * so a whoami failure fails open (controls stay enabled and a 403 surfaces
 * inline) — this is a UX affordance, never the authorization itself.
 */
import { createContext, useContext, useEffect, useState } from "react";
import type { Scope } from "@agentloom/api-client";
import { fetchWhoAmI } from "@/lib/dashboard/streams";
import {
  can as canFor,
  controlStateFor as controlStateForPerms,
  type ControlName,
  type ControlState,
  type Permissions,
} from "@/lib/pure/dashboard/permissions";

interface PermissionsContextValue extends Permissions {
  /** Whether the caller may use a scope (unknown scopes resolve permissive). */
  can(want: Scope): boolean;
  /** How a named control should render under the current permissions. */
  controlState(control: ControlName): ControlState;
}

const PermissionsContext = createContext<PermissionsContextValue | null>(null);

export function PermissionsProvider({ children }: { children: React.ReactNode }) {
  const [perms, setPerms] = useState<Permissions>({ status: "loading", scopes: [] });

  useEffect(() => {
    let cancelled = false;
    void fetchWhoAmI()
      .then((me) => {
        if (!cancelled) setPerms({ status: "ready", scopes: me.scopes as Scope[] });
      })
      .catch(() => {
        // Unconfigured / offline / unauthorized — fail open (server enforces).
        if (!cancelled) setPerms({ status: "unavailable", scopes: [] });
      });
    return () => {
      cancelled = true;
    };
  }, []);

  const value: PermissionsContextValue = {
    ...perms,
    can: (want) => canFor(perms, want),
    controlState: (control) => controlStateForPerms(perms, control),
  };
  return <PermissionsContext.Provider value={value}>{children}</PermissionsContext.Provider>;
}

/** The resolved permissions. Safe outside a provider: returns an "unavailable"
 * state (fail-open) so a component never crashes for lack of the provider. */
export function usePermissions(): PermissionsContextValue {
  const ctx = useContext(PermissionsContext);
  if (ctx) return ctx;
  const fallback: Permissions = { status: "unavailable", scopes: [] };
  return {
    ...fallback,
    can: (want) => canFor(fallback, want),
    controlState: (control) => controlStateForPerms(fallback, control),
  };
}
