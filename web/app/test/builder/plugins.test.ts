// Plugin catalog (ticket 17.4): grouping GET /v1/plugins and deriving config
// schemas + picker option lists.
import { describe, expect, it } from "vitest";
import type { PluginInfoView } from "@agentloom/api-client";
import {
  buildCatalog,
  configSchemas,
  modelSuggestions,
  providerNames,
  retrieverNames,
  toolNames,
  validatorNames,
} from "@/lib/pure/builder/plugins";

function plugin(kind: string, name: string, config_schema?: Record<string, unknown>): PluginInfoView {
  return {
    kind: kind as PluginInfoView["kind"],
    name,
    version: "1.0.0",
    capabilities: { side_effectful: false, cacheable: true, cost_bearing: false },
    ...(config_schema ? { config_schema } : {}),
  };
}

const sample: PluginInfoView[] = [
  plugin("executor", "llm", { type: "object", additionalProperties: false, properties: { model: { type: "string" } } }),
  plugin("executor", "join"),
  plugin("tool", "http_request", { type: "object", properties: { url: { type: "string" } } }),
  plugin("tool", "json_transform"),
  plugin("validator", "regex"),
  plugin("model_provider", "mock"),
  plugin("model_provider", "anthropic"),
  plugin("retriever", "pg_fulltext"),
];

describe("buildCatalog", () => {
  const cat = buildCatalog(sample);
  it("groups by kind", () => {
    expect(cat.executors.llm?.name).toBe("llm");
    expect(toolNames(cat)).toEqual(["http_request", "json_transform"]);
    expect(validatorNames(cat)).toEqual(["regex"]);
    expect(providerNames(cat)).toEqual(["anthropic", "mock"]);
    expect(retrieverNames(cat)).toEqual(["pg_fulltext"]);
  });
});

describe("configSchemas", () => {
  it("uses the live executor schema and falls back for the rest", () => {
    const schemas = configSchemas(buildCatalog(sample));
    // Live: llm uses the provided (root-inlined) schema.
    expect(schemas["llm"]?.schema).toEqual(sample[0]!.config_schema);
    // Fallback: retrieve (not in the sample) still has a schema.
    expect(schemas["retrieve"]).toBeDefined();
  });
});

describe("modelSuggestions", () => {
  it("groups known models by the configured providers", () => {
    const s = modelSuggestions(buildCatalog(sample));
    expect(s).toContain("mock/sim-1");
    expect(s).toContain("anthropic/claude-sonnet-5");
    expect(s).not.toContain("openai/gpt-5-mini"); // openai not configured
  });
  it("offers mock models when the catalog is empty (offline)", () => {
    const s = modelSuggestions(buildCatalog([]));
    expect(s).toContain("mock/sim-1");
  });
});
