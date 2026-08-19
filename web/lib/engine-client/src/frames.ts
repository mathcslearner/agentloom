/**
 * WebSocket wire frames (ADR-018, 16.3/16.4).
 *
 * These mirror the `WS*Frame` component schemas in `api/openapi.yaml`. They are
 * hand-written here because M16.5 predates the generated OpenAPI REST client
 * (M17.1); when that lands, these can be swapped for its output. The `event`
 * field is the generated event envelope, so the one type that actually drifts
 * (the event vocabulary) is generated and CI-checked.
 *
 * Frame close code for a slow consumer is `4001` — reconnect with `last_seq`.
 */
import type { EventEnvelope } from "./generated/events.js";

/** Application close code the server uses for a slow consumer (16.3/16.4). */
export const CLOSE_SLOW_CONSUMER = 4001;

// ── Run WebSocket frames (server → client) ────────────────────────────────────

/** First frame on the run WS: the run snapshot (the `GET /v1/runs/{id}` body). */
export interface RunSnapshotFrame<TRun = unknown> {
  type: "snapshot";
  run: TRun;
}

/** One normalized event envelope, backfilled then live. */
export interface EventFrame {
  type: "event";
  event: EventEnvelope;
  /** Firehose only — the subscription ids this envelope matched. */
  subscriptions?: string[];
}

/** Run WS: marks the end of backfill; `last_seq` is the resume cursor. */
export interface RunCaughtUpFrame {
  type: "caught_up";
  last_seq: number;
}

/** A control-message rejection (firehose) or a pre-close reason (run WS). */
export interface ErrorFrame {
  type: "error";
  code: string;
  message: string;
  /** The subscription id the error concerns, when applicable (firehose). */
  id?: string;
}

export type RunFrame<TRun = unknown> =
  | RunSnapshotFrame<TRun>
  | EventFrame
  | RunCaughtUpFrame
  | ErrorFrame;

// ── Firehose frames (server → client) ─────────────────────────────────────────

/** Firehose: acknowledges a `subscribe`, echoing the effective filter. */
export interface SubscribedFrame {
  type: "subscribed";
  id: string;
  filter: EventFilter;
}

/** Firehose: acknowledges an `unsubscribe`. */
export interface UnsubscribedFrame {
  type: "unsubscribed";
  id: string;
}

/** Firehose: end of a subscription's cursor backfill; highest seq per run. */
export interface FirehoseCaughtUpFrame {
  type: "caught_up";
  id: string;
  cursors: Record<string, number>;
}

export type FirehoseFrame =
  | EventFrame
  | SubscribedFrame
  | UnsubscribedFrame
  | FirehoseCaughtUpFrame
  | ErrorFrame;

// ── Firehose control messages (client → server) ───────────────────────────────

/** Server-side filter. Empty matches everything; fields are ANDed. */
export interface EventFilter {
  run_ids?: string[];
  types?: string[];
  definition_id?: string;
  definition_name?: string;
}

export interface SubscribeMessage {
  type: "subscribe";
  id: string;
  filter: EventFilter;
  cursors?: Record<string, number>;
}

export interface UnsubscribeMessage {
  type: "unsubscribe";
  id: string;
}

export type FirehoseControl = SubscribeMessage | UnsubscribeMessage;

/** In-band firehose error codes (16.4). */
export const FIREHOSE_ERROR_CODES = [
  "bad_message",
  "filter_invalid",
  "subscription_limit",
  "unknown_subscription",
] as const;
export type FirehoseErrorCode = (typeof FIREHOSE_ERROR_CODES)[number];

/** Parse a text frame; returns null when it isn't JSON with a string `type`. */
export function parseFrame(data: unknown): { type: string; [k: string]: unknown } | null {
  if (typeof data !== "string") return null;
  let v: unknown;
  try {
    v = JSON.parse(data);
  } catch {
    return null;
  }
  if (v === null || typeof v !== "object") return null;
  const t = (v as { type?: unknown }).type;
  if (typeof t !== "string") return null;
  return v as { type: string; [k: string]: unknown };
}
