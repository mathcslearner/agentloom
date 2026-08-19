// The plugin catalog (ticket 17.4): the builder's view over GET /v1/plugins,
// grouping the backend's self-describing plugins by kind and deriving the
// per-step-type config schemas the config panel renders forms from. Pure — no
// React, no fetch (the fetch lives in the catalog store). When the live catalog
// is unavailable, `configSchemas` falls back to the published definition schema
// (graphdef.fallbackConfigSchemas), so the executor forms still render offline.

import type { PluginInfoView } from "@agentloom/api-client";
import type { StepType } from "@agentloom/graphdef";
import { fallbackConfigSchemas, pluginConfigSchema, type ConfigSchemas } from "@agentloom/graphdef";

/** The plugin catalog, grouped by kind. */
export interface PluginCatalog {
  /** Executor plugins keyed by step type (executor name == step type). */
  executors: Partial<Record<StepType, PluginInfoView>>;
  /** Tool plugins (config_schema describes the tool's arguments). */
  tools: PluginInfoView[];
  /** Validator plugins (config_schema describes a ValidatorSpec.config). */
  validators: PluginInfoView[];
  /** Configured model-provider plugins (no config schema; a provider name list). */
  providers: PluginInfoView[];
  /** Retriever plugins (no config schema; a retriever name list). */
  retrievers: PluginInfoView[];
}

/** An empty catalog (used before the fetch resolves or when it fails). */
export function emptyCatalog(): PluginCatalog {
  return { executors: {}, tools: [], validators: [], providers: [], retrievers: [] };
}

/** Group a GET /v1/plugins payload into the catalog. */
export function buildCatalog(plugins: readonly PluginInfoView[]): PluginCatalog {
  const cat = emptyCatalog();
  for (const p of plugins) {
    switch (p.kind) {
      case "executor":
        cat.executors[p.name as StepType] = p;
        break;
      case "tool":
        cat.tools.push(p);
        break;
      case "validator":
        cat.validators.push(p);
        break;
      case "model_provider":
        cat.providers.push(p);
        break;
      case "retriever":
        cat.retrievers.push(p);
        break;
      default:
        break;
    }
  }
  return cat;
}

/**
 * Per-step-type config schemas for the config validator and the form renderer:
 * the live catalog's executor schemas layered over the offline fallback, so a
 * step type present in the catalog uses its (live) schema and any gap falls back
 * to the published definition schema.
 */
export function configSchemas(catalog: PluginCatalog): ConfigSchemas {
  const out: ConfigSchemas = { ...fallbackConfigSchemas() };
  for (const [type, plugin] of Object.entries(catalog.executors)) {
    const cs = plugin?.config_schema;
    if (cs && typeof cs === "object") {
      out[type] = pluginConfigSchema(cs as Record<string, unknown>);
    }
  }
  return out;
}

/** Names of tool plugins, sorted — for the `tool` field picker. */
export function toolNames(catalog: PluginCatalog): string[] {
  return catalog.tools.map((t) => t.name).sort();
}

/** Names of retriever plugins, sorted — for the `retriever` field picker. */
export function retrieverNames(catalog: PluginCatalog): string[] {
  return catalog.retrievers.map((r) => r.name).sort();
}

/** Names of validator plugins, sorted — for the validation-envelope picker. */
export function validatorNames(catalog: PluginCatalog): string[] {
  return catalog.validators.map((v) => v.name).sort();
}

/** Names of configured model providers, sorted — for the model picker groups. */
export function providerNames(catalog: PluginCatalog): string[] {
  return catalog.providers.map((p) => p.name).sort();
}

/**
 * Model suggestions for the picker. No model list exists in the API (providers
 * carry only names), so we seed with known-good demo models and group them by
 * whichever providers are actually configured. Free text is always allowed; a
 * bare model that matches no vendor prefix is only a warning at submit.
 */
export function modelSuggestions(catalog: PluginCatalog): string[] {
  const configured = new Set(providerNames(catalog));
  const seed: Record<string, string[]> = {
    mock: ["mock/sim-1", "mock/cheap", "mock/small", "mock/judge-1"],
    anthropic: ["anthropic/claude-sonnet-5", "anthropic/claude-haiku-4-5"],
    openai: ["openai/gpt-5-mini"],
  };
  const out: string[] = [];
  for (const provider of providerNames(catalog)) {
    for (const m of seed[provider] ?? []) out.push(m);
  }
  // If the catalog is empty (offline), offer the mock models so the form is
  // still usable in a keyless dev/demo environment.
  if (out.length === 0 && configured.size === 0) return seed["mock"] ?? [];
  return out;
}
