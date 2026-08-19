"use client";

// The plugin-catalog store (ticket 17.4): fetches GET /v1/plugins once (through
// the same-origin proxy) and exposes the grouped catalog + the per-step-type
// config schemas the config panel renders and validates against. On failure it
// falls back to the published definition schema (graphdef), so the executor
// config forms still render offline — only the tool/validator/provider/retriever
// pickers degrade to free text. Kept out of the undoable builder store: it is
// remote reference data, not canvas state.

import { create } from "zustand";
import type { ConfigSchemas } from "@agentloom/graphdef";
import { browserApi } from "@/lib/api/browser";
import { buildCatalog, configSchemas, emptyCatalog, type PluginCatalog } from "@/lib/pure/builder/plugins";

export type CatalogStatus = "idle" | "loading" | "ready" | "error";

export interface CatalogState {
  status: CatalogStatus;
  catalog: PluginCatalog;
  schemas: ConfigSchemas;
  error?: string;
  /** Fetch the catalog (idempotent — only fetches once past idle). */
  load: () => Promise<void>;
}

export const useCatalogStore = create<CatalogState>((set, get) => ({
  status: "idle",
  catalog: emptyCatalog(),
  // Before the fetch resolves, the fallback schemas (from the published
  // definition schema) keep the executor forms working.
  schemas: configSchemas(emptyCatalog()),
  load: async () => {
    if (get().status === "loading" || get().status === "ready") return;
    set({ status: "loading" });
    try {
      const { data, error } = await browserApi().GET("/v1/plugins");
      if (error || !data) {
        set({ status: "error", error: "plugin catalog unavailable" });
        return;
      }
      const catalog = buildCatalog(data.plugins ?? []);
      set({ status: "ready", catalog, schemas: configSchemas(catalog), error: undefined });
    } catch (err) {
      set({ status: "error", error: err instanceof Error ? err.message : "plugin catalog unavailable" });
    }
  },
}));
