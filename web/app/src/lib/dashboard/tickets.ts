/**
 * WebSocket ticket minting (ticket 18.1). A browser cannot set an
 * `Authorization` header on a WebSocket, so the engine-client mints a
 * short-lived signed ticket over REST and passes it as `?ticket=`. In the
 * browser the mint goes through the same-origin proxy (which injects the
 * server-held key), so the API key never reaches the browser — this is the
 * `mintTicket` auth mode `@agentloom/engine-client` designed for.
 */
import { browserApi } from "@/lib/api/browser";

/** Mint a ticket for one run's event WebSocket. */
export async function mintRunTicket(runId: string): Promise<string> {
  const { data, error, response } = await browserApi().POST("/v1/runs/{run_id}/ws-ticket", {
    params: { path: { run_id: runId } },
  });
  if (error || !data?.ticket) {
    throw new Error(`mint run ws-ticket failed: ${response.status}`);
  }
  return data.ticket;
}

/** Mint a ticket for the multi-run firehose WebSocket. */
export async function mintFirehoseTicket(): Promise<string> {
  const { data, error, response } = await browserApi().POST("/v1/events/ws-ticket");
  if (error || !data?.ticket) {
    throw new Error(`mint firehose ws-ticket failed: ${response.status}`);
  }
  return data.ticket;
}
