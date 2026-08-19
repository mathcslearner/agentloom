/**
 * Dashboard stream factories (ticket 18.1). Wrap `@agentloom/engine-client`'s
 * `RunStream`/`FirehoseStream` with the browser auth mode: the API key stays
 * server-side, so tickets are minted through the same-origin proxy, and the
 * WebSocket is dialed directly at the public API origin (a Next.js route
 * handler cannot forward a WS upgrade).
 */
import {
  RunStream,
  FirehoseStream,
  type RunStreamHandlers,
  type FirehoseHandlers,
} from "@agentloom/engine-client";
import type {
  RunResponse,
  RunGraphResponse,
  RunCostResponse,
  StepLogsResponse,
  ApprovalListResponse,
  ApprovalView,
  DecideApprovalResponse,
  Issue,
} from "@agentloom/api-client";
import { problem } from "@agentloom/api-client";
import type { LogLevel } from "@/lib/pure/dashboard/logs";
import { mintRunTicket, mintFirehoseTicket } from "@/lib/dashboard/tickets";
import { browserApi } from "@/lib/api/browser";

export function createRunStream(
  baseUrl: string,
  runId: string,
  handlers: RunStreamHandlers<RunResponse>,
  lastSeq = 0,
): RunStream<RunResponse> {
  return new RunStream<RunResponse>({
    baseUrl,
    runId,
    auth: { mintTicket: () => mintRunTicket(runId) },
    handlers,
    lastSeq,
  });
}

/**
 * Fetch a run's graph introspection view (ticket 18.2) through the same-origin
 * proxy (the key stays server-side). Rejects on any non-2xx so the controller
 * can degrade to the snapshot topology.
 */
export async function fetchRunGraph(runId: string): Promise<RunGraphResponse> {
  const { data, error } = await browserApi().GET("/v1/runs/{run_id}/graph", {
    params: { path: { run_id: runId } },
  });
  if (error || !data) throw new Error(`run graph fetch failed: ${runId}`);
  return data;
}

/** Fetch a run's detail body (GET /v1/runs/{id}) through the proxy (ticket
 * 18.3). Rejects on any non-2xx so the controller surfaces it. */
export async function fetchRun(runId: string): Promise<RunResponse> {
  const { data, error } = await browserApi().GET("/v1/runs/{run_id}", {
    params: { path: { run_id: runId } },
  });
  if (error || !data) throw new Error(`run fetch failed: ${runId}`);
  return data;
}

/** Fetch a run's cost breakdown (GET /v1/runs/{id}/cost) through the proxy. */
export async function fetchRunCost(runId: string): Promise<RunCostResponse> {
  const { data, error } = await browserApi().GET("/v1/runs/{run_id}/cost", {
    params: { path: { run_id: runId } },
  });
  if (error || !data) throw new Error(`run cost fetch failed: ${runId}`);
  return data;
}

/** Fetch one keyset page of a step's captured logs (GET .../logs). */
export async function fetchStepLogs(
  runId: string,
  stepId: string,
  opts: { attempt?: number; level?: LogLevel; cursor?: string; limit?: number } = {},
): Promise<StepLogsResponse> {
  const query: Record<string, string | number> = {};
  if (opts.attempt !== undefined) query.attempt = opts.attempt;
  if (opts.level) query.level = opts.level;
  if (opts.cursor) query.cursor = opts.cursor;
  if (opts.limit !== undefined) query.limit = opts.limit;
  const { data, error } = await browserApi().GET("/v1/runs/{run_id}/steps/{step_id}/logs", {
    params: { path: { run_id: runId, step_id: stepId }, query },
  });
  if (error || !data) throw new Error(`step logs fetch failed: ${runId}/${stepId}`);
  return data;
}

/** Outcome of a budget/park action (ticket 18.4). */
export type BudgetActionOutcome =
  | { kind: "ok" }
  | { kind: "invalid"; message: string }
  | { kind: "conflict"; message: string }
  | { kind: "forbidden"; message: string }
  | { kind: "error"; message: string };

function classifyActionError(error: unknown): BudgetActionOutcome {
  const p = problem(error);
  if (!p) return { kind: "error", message: "the request failed" };
  switch (p.code) {
    case "invalid_request":
      return { kind: "invalid", message: p.message };
    case "conflict":
      return { kind: "conflict", message: p.message };
    case "forbidden":
      return { kind: "forbidden", message: p.message };
    default:
      return { kind: "error", message: p.message };
  }
}

/** Raise a run's spend budget (PATCH /v1/runs/{id}/budget) through the proxy. */
export async function setRunBudget(runId: string, budgetUsd: number): Promise<BudgetActionOutcome> {
  const { error } = await browserApi().PATCH("/v1/runs/{run_id}/budget", {
    params: { path: { run_id: runId } },
    body: { budget_usd: budgetUsd },
  });
  return error ? classifyActionError(error) : { kind: "ok" };
}

/** Unpark a run (POST /v1/runs/{id}/unpark) through the proxy. */
export async function unparkRun(runId: string): Promise<BudgetActionOutcome> {
  const { error } = await browserApi().POST("/v1/runs/{run_id}/unpark", {
    params: { path: { run_id: runId } },
  });
  return error ? classifyActionError(error) : { kind: "ok" };
}

/** Fetch a keyset page of approvals (GET /v1/approvals) through the proxy
 * (ticket 18.5). */
export async function listApprovals(
  query: { status?: string; run_id?: string; cursor?: string; limit?: number } = {},
): Promise<ApprovalListResponse> {
  const q: Record<string, string | number> = {};
  if (query.status) q.status = query.status;
  if (query.run_id) q.run_id = query.run_id;
  if (query.cursor) q.cursor = query.cursor;
  if (query.limit !== undefined) q.limit = query.limit;
  const { data, error } = await browserApi().GET("/v1/approvals", { params: { query: q } });
  if (error || !data) throw new Error("approvals list fetch failed");
  return data;
}

/** The typed outcome of a decide request (ticket 18.5). Beyond the budget
 * action codes it distinguishes the two approval-specific 4xx: a 409
 * `approval_not_pending` (a concurrent decision from another session — the DoD-2
 * recovery case) and a 422 `approval_decision_invalid` carrying the edit-schema
 * `issues[]`. */
export type DecisionOutcome =
  | { kind: "ok"; response: DecideApprovalResponse }
  | { kind: "not_found"; message: string }
  | { kind: "not_pending"; message: string }
  | { kind: "invalid"; message: string; issues?: Issue[] }
  | { kind: "conflict"; message: string }
  | { kind: "forbidden"; message: string }
  | { kind: "error"; message: string };

/** Decide an approval (POST /v1/approvals/{id}:decide) through the proxy. */
export async function decideApproval(
  approvalID: string,
  body: { decision: "approve" | "reject"; edited_payload?: unknown; comment?: string },
): Promise<DecisionOutcome> {
  const { data, error } = await browserApi().POST("/v1/approvals/{approvalID}:decide", {
    params: { path: { approvalID } },
    body,
  });
  if (data) return { kind: "ok", response: data };
  const p = problem(error);
  if (!p) return { kind: "error", message: "the request failed" };
  switch (p.code) {
    case "approval_not_found":
      return { kind: "not_found", message: p.message };
    case "approval_not_pending":
      return { kind: "not_pending", message: p.message };
    case "approval_decision_invalid":
      return { kind: "invalid", message: p.message, issues: p.issues };
    case "invalid_request":
      return { kind: "invalid", message: p.message };
    case "conflict":
      return { kind: "conflict", message: p.message };
    case "forbidden":
      return { kind: "forbidden", message: p.message };
    default:
      return { kind: "error", message: p.message };
  }
}

/** Fetch a single approval's current record by run + id (there is no
 * GET /v1/approvals/{id}; the inbox re-reads a run's page and finds the row). */
export async function refetchApproval(
  approvalID: string,
  runId: string | undefined,
): Promise<ApprovalView | undefined> {
  if (!runId) return undefined;
  try {
    const page = await listApprovals({ run_id: runId, limit: 200 });
    return page.approvals.find((a) => a.id === approvalID);
  } catch {
    return undefined;
  }
}

export function createFirehose(baseUrl: string, handlers: FirehoseHandlers): FirehoseStream {
  return new FirehoseStream({
    baseUrl,
    auth: { mintTicket: mintFirehoseTicket },
    handlers,
  });
}
