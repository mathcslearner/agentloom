// Persistence flows for the builder (ticket 17.6): save/version a definition and
// submit a run, over the typed same-origin client (`browserApi`, which targets
// the proxy). Each returns a typed result union so the dialogs can render the
// exact outcome — created, appended, a version conflict (stale save), a name
// conflict, or a validation 400 with issues[] to map onto the canvas.

import { problem, type Schema } from "@agentloom/api-client";
import { browserApi } from "@/lib/api/browser";

/** A definition value as produced by `canonicalize`/`toDefinition` (opaque). */
export type DefinitionValue = Schema<"Definition">;

/** Validation issues from a 400 (mapped onto nodes/edges by the caller). */
export type ApiIssue = Schema<"Issue">;

export type SaveOutcome =
  | { kind: "created"; id: string; name: string; version: number }
  | { kind: "appended"; id: string; name: string; version: number }
  | { kind: "name_conflict"; name: string; message: string }
  | { kind: "version_conflict"; name: string; message: string }
  | { kind: "invalid"; issues: ApiIssue[]; message: string }
  | { kind: "error"; message: string };

/** Create a new definition (version 1) under its own name. */
export async function createDefinition(name: string, def: DefinitionValue): Promise<SaveOutcome> {
  const { data, error } = await browserApi().POST("/v1/definitions", { body: { definition: def } });
  if (data) return { kind: "created", id: data.id, name: data.name, version: data.version };
  return classifySaveError(name, error);
}

/** Append the next version of an existing name. `expected` is the version the
 *  builder opened at — the `If-Match` precondition guarding a stale save. */
export async function appendDefinitionVersion(
  name: string,
  def: DefinitionValue,
  expected?: number,
): Promise<SaveOutcome> {
  const { data, error } = await browserApi().POST("/v1/definitions/{name}/versions", {
    params: {
      path: { name },
      ...(expected !== undefined ? { header: { "If-Match": expected } } : {}),
    },
    body: { definition: def },
  });
  if (data) return { kind: "appended", id: data.id, name: data.name, version: data.version };
  return classifySaveError(name, error);
}

function classifySaveError(name: string, error: unknown): SaveOutcome {
  const p = problem(error);
  if (!p) return { kind: "error", message: "the request failed" };
  switch (p.code) {
    case "conflict":
      return { kind: "name_conflict", name, message: p.message };
    case "version_conflict":
      return { kind: "version_conflict", name, message: p.message };
    case "invalid_definition":
      return { kind: "invalid", issues: p.issues ?? [], message: p.message };
    default:
      return { kind: "error", message: p.message };
  }
}

export type SubmitOutcome =
  | { kind: "submitted"; runId: string; status: string; reused: boolean }
  | { kind: "invalid"; issues: ApiIssue[]; message: string }
  | { kind: "error"; message: string };

/** Submit a run. When `definitionId` is given the run references the stored
 *  definition (clean, saved canvas); otherwise the inline `def` is submitted. */
export async function submitRun(args: {
  def?: DefinitionValue;
  definitionId?: string;
  params?: unknown;
  idempotencyKey?: string;
}): Promise<SubmitOutcome> {
  const body = args.definitionId
    ? { definition_id: args.definitionId, params: args.params }
    : { definition: args.def, params: args.params };
  const { data, error } = await browserApi().POST("/v1/runs", {
    body,
    ...(args.idempotencyKey ? { params: { header: { "Idempotency-Key": args.idempotencyKey } } } : {}),
  });
  if (data) return { kind: "submitted", runId: data.run_id, status: data.status, reused: Boolean(data.reused) };
  const p = problem(error);
  if (p?.code === "invalid_definition") return { kind: "invalid", issues: p.issues ?? [], message: p.message };
  return { kind: "error", message: p?.message ?? "submit failed" };
}

/** Load a stored definition's spec for opening in the builder. */
export async function fetchDefinition(id: string): Promise<
  { kind: "ok"; id: string; name: string; version: number; spec: DefinitionValue } | { kind: "error"; message: string }
> {
  const { data, error } = await browserApi().GET("/v1/definitions/{definition_id}", {
    params: { path: { definition_id: id } },
  });
  if (data) return { kind: "ok", id: data.id, name: data.name, version: data.version, spec: data.spec };
  return { kind: "error", message: problem(error)?.message ?? "failed to load definition" };
}

/** The name's current latest version, for a friendly pre-save staleness check
 *  (the server `If-Match` CAS is the authority). Returns null if unknown. */
export async function latestVersion(name: string): Promise<number | null> {
  const { data } = await browserApi().GET("/v1/definitions/{name}/versions", {
    params: { path: { name } },
  });
  if (!data || data.versions.length === 0) return null;
  return data.versions[data.versions.length - 1]!.version;
}
