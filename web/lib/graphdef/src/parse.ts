// parseDefinition — a thin JSON.parse wrapper that reports a syntax error as a
// GraphdefError (used by the import flow, 17.6). It does no shape or content
// validation; pass the result to toFlow (shape) and the client validator (17.5).

import { GraphdefError } from "./types.js";

/** Parse definition JSON text, throwing {@link GraphdefError} on a syntax error. */
export function parseDefinition(text: string): unknown {
  try {
    return JSON.parse(text);
  } catch (err) {
    const message = err instanceof Error ? err.message : String(err);
    throw new GraphdefError("invalid_json", "", `definition is not valid JSON: ${message}`);
  }
}
