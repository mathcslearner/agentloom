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
import type { RunResponse } from "@agentloom/api-client";
import { mintRunTicket, mintFirehoseTicket } from "@/lib/dashboard/tickets";

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

export function createFirehose(baseUrl: string, handlers: FirehoseHandlers): FirehoseStream {
  return new FirehoseStream({
    baseUrl,
    auth: { mintTicket: mintFirehoseTicket },
    handlers,
  });
}
