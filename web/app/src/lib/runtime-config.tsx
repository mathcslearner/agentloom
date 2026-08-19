"use client";

/**
 * Runtime config provider (ticket 18.1). Carries the small set of *public*
 * runtime values the browser needs — currently the WebSocket-reachable API
 * origin — from the server (root layout, which reads the environment) into
 * client components, without a build-time `NEXT_PUBLIC_` bake and without the
 * API key (which never leaves the server; the live feed authenticates with a
 * proxy-minted ws-ticket).
 */
import { createContext, useContext } from "react";

export interface RuntimeConfig {
  /** Browser-reachable API origin for the WebSocket event feed (no key). */
  apiPublicUrl: string;
}

const RuntimeConfigContext = createContext<RuntimeConfig | null>(null);

export function RuntimeConfigProvider({
  value,
  children,
}: {
  value: RuntimeConfig;
  children: React.ReactNode;
}) {
  return <RuntimeConfigContext.Provider value={value}>{children}</RuntimeConfigContext.Provider>;
}

export function useRuntimeConfig(): RuntimeConfig {
  const cfg = useContext(RuntimeConfigContext);
  if (!cfg) throw new Error("useRuntimeConfig used outside RuntimeConfigProvider");
  return cfg;
}
