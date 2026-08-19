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
import type { RunResponse, RunGraphResponse, RunCostResponse, StepLogsResponse } from "@agentloom/api-client";
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

export function createFirehose(baseUrl: string, handlers: FirehoseHandlers): FirehoseStream {
  return new FirehoseStream({
    baseUrl,
    auth: { mintTicket: mintFirehoseTicket },
    handlers,
  });
}
