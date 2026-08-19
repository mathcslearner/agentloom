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
import type { RunResponse, RunGraphResponse } from "@agentloom/api-client";
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

export function createFirehose(baseUrl: string, handlers: FirehoseHandlers): FirehoseStream {
  return new FirehoseStream({
    baseUrl,
    auth: { mintTicket: mintFirehoseTicket },
    handlers,
  });
}
