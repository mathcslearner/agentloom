/**
 * @agentloom/engine-client — typed client for the agentloom event feed
 * (ADR-018). The event types are generated from the backend's committed
 * `docs/schema/events.v1.json` (CI-drift-checked); the streams implement ticket
 * auth, snapshot → backfill → live-tail, seq dedupe, resume, and reconnection
 * backoff. Usable from Node (headless tailing) and the browser.
 */

// Generated event vocabulary + envelope types (the drift-checked contract).
export * from "./generated/events.js";

// Wire frames + firehose control messages.
export type {
  ErrorFrame,
  EventFilter,
  EventFrame,
  FirehoseCaughtUpFrame,
  FirehoseControl,
  FirehoseErrorCode,
  FirehoseFrame,
  RunCaughtUpFrame,
  RunFrame,
  RunSnapshotFrame,
  SubscribeMessage,
  SubscribedFrame,
  UnsubscribedFrame,
  UnsubscribeMessage,
} from "./frames.js";
export { CLOSE_SLOW_CONSUMER, FIREHOSE_ERROR_CODES } from "./frames.js";

// Auth.
export type { AuthProvider, MintOptions, TicketAudience } from "./auth.js";
export { resolveTicket, ticketPath, TicketError } from "./auth.js";

// Backoff + cursors (exported for advanced callers and tests).
export type { BackoffOptions } from "./backoff.js";
export { backoffDelay, defaultBackoff, resolveBackoff } from "./backoff.js";
export type { Offer } from "./cursor.js";
export { RunCursors } from "./cursor.js";

// Transport seams.
export type {
  FetchLike,
  Logger,
  Scheduler,
  TimerHandle,
  WebSocketFactory,
  WebSocketLike,
} from "./transport.js";
export { noopLogger, systemScheduler } from "./transport.js";

// Streams — the public surface.
export type {
  RunStreamHandlers,
  RunStreamOptions,
  RunStreamState,
} from "./run-stream.js";
export { RunStream, tailRun, TERMINAL_RUN_EVENTS } from "./run-stream.js";
export type {
  FirehoseHandlers,
  FirehoseOptions,
  FirehoseState,
} from "./firehose-stream.js";
export { FirehoseStream, tailFirehose } from "./firehose-stream.js";
