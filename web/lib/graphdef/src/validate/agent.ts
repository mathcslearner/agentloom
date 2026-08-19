// The agent-role merge for validation (ADR-016, ticket 14.1), a port of
// internal/dag/agent.go ResolveAgentStep restricted to the fields the validator
// reads: an agent step's own overrides win, else the role's defaults apply. Used
// by checkAgents to check the merged model-call (model + prompt-xor-messages)
// and the merged output_format / model_fallbacks. Operates on the raw JSON
// shapes, so it never depends on the typed Definition.

import { isPlainObject } from "../util.js";

/** The subset of a merged agent config the validator needs. */
export interface MergedAgent {
  model: string;
  prompt: string;
  messages: unknown[];
  modelFallbacks: unknown[];
  outputFormat: unknown; // present (object) or undefined
}

function str(v: unknown): string {
  return typeof v === "string" ? v : "";
}
function arr(v: unknown): unknown[] {
  return Array.isArray(v) ? v : [];
}

/**
 * Resolve an agent step's config against its role. Returns null when the step is
 * not resolvable (no config, empty/unknown ref) — the caller reports those.
 */
export function resolveAgentStep(def: Record<string, unknown>, stepConfig: unknown): MergedAgent | null {
  if (!isPlainObject(stepConfig)) return null;
  const ref = str(stepConfig["agent"]);
  if (ref === "") return null;
  const agents = def["agents"];
  const role = isPlainObject(agents) ? agents[ref] : undefined;
  if (!isPlainObject(role)) return null;

  const model = str(stepConfig["model"]) || str(role["model"]);
  const modelFallbacks = stepConfig["model_fallbacks"] !== undefined && arr(stepConfig["model_fallbacks"]).length > 0
    ? arr(stepConfig["model_fallbacks"])
    : arr(role["model_fallbacks"]);
  const outputFormat = stepConfig["output_format"] !== undefined ? stepConfig["output_format"] : role["output_format"];

  return {
    model,
    prompt: str(stepConfig["prompt"]),
    messages: arr(stepConfig["messages"]),
    modelFallbacks,
    outputFormat: isPlainObject(outputFormat) ? outputFormat : undefined,
  };
}
