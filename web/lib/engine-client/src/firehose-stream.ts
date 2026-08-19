/**
 * Multi-run firehose stream (ADR-018, 16.4): one connection, many
 * client-managed subscriptions, filtered cross-run event delivery.
 *
 * The server delivers each run's events in seq order (per-run Tailer) tagged
 * with the subscription ids they matched, so the client only dedupes across
 * reconnects (by `(run_id, seq)`). On reconnect every subscription is re-sent
 * with `cursors` = the tracked high-water, so runs resume without gaps.
 */
import type { AuthProvider } from "./auth.js";
import { resolveTicket } from "./auth.js";
import type { BackoffOptions } from "./backoff.js";
import { backoffDelay, resolveBackoff } from "./backoff.js";
import { RunCursors } from "./cursor.js";
import type { EventEnvelope, EventType } from "./generated/events.js";
import type {
  ErrorFrame,
  EventFilter,
  EventFrame,
  FirehoseCaughtUpFrame,
  SubscribedFrame,
  SubscribeMessage,
  UnsubscribedFrame,
  UnsubscribeMessage,
} from "./frames.js";
import { CLOSE_SLOW_CONSUMER, parseFrame } from "./frames.js";
import type {
  FetchLike,
  Logger,
  Scheduler,
  TimerHandle,
  WebSocketFactory,
  WebSocketLike,
} from "./transport.js";
import {
  defaultWebSocketFactory,
  noopLogger,
  systemScheduler,
  toWebSocketUrl,
} from "./transport.js";

const TERMINAL: readonly EventType[] = ["run_succeeded", "run_failed", "run_cancelled"];

export type FirehoseState = "connecting" | "live" | "reconnecting" | "closed";

export interface FirehoseHandlers {
  /** Each event, deduped by (run_id, seq), with the matched subscription ids. */
  onEvent?(env: EventEnvelope, subscriptions: string[]): void;
  onSubscribed?(id: string, filter: EventFilter): void;
  onUnsubscribed?(id: string): void;
  onCaughtUp?(id: string, cursors: Record<string, number>): void;
  onState?(state: FirehoseState, prev: FirehoseState): void;
  /** Non-fatal errors (ticket mint failures, in-band `error` frames). */
  onError?(err: Error): void;
  onClosed?(): void;
}

export interface FirehoseOptions {
  baseUrl: string;
  auth: AuthProvider;
  handlers?: FirehoseHandlers;
  backoff?: Partial<BackoffOptions>;
  scheduler?: Scheduler;
  webSocketFactory?: WebSocketFactory;
  fetchImpl?: FetchLike;
  rng?: () => number;
  logger?: Logger;
  /** Max runs to resume per subscribe (server default `MaxCursorRuns` = 256). */
  maxCursorRuns?: number;
}

interface DesiredSub {
  id: string;
  filter: EventFilter;
  cursors: Record<string, number>; // explicit resume cursors from subscribe()
}

const noHandlers: Required<FirehoseHandlers> = {
  onEvent: () => {},
  onSubscribed: () => {},
  onUnsubscribed: () => {},
  onCaughtUp: () => {},
  onState: () => {},
  onError: () => {},
  onClosed: () => {},
};

export class FirehoseStream {
  private readonly o: FirehoseOptions;
  private readonly h: Required<FirehoseHandlers>;
  private readonly backoff: BackoffOptions;
  private readonly sched: Scheduler;
  private readonly wsf: WebSocketFactory;
  private readonly rng: () => number;
  private readonly log: Logger;
  private readonly maxCursorRuns: number;

  private readonly subs = new Map<string, DesiredSub>();
  private readonly cursors = new RunCursors();
  private readonly terminal = new Set<string>();

  private ws: WebSocketLike | null = null;
  private timer: TimerHandle | null = null;
  private connectGen = 0;
  private attempt = 0;
  private started = false;
  private userClosed = false;
  private stateVal: FirehoseState = "closed";

  constructor(opts: FirehoseOptions) {
    this.o = opts;
    this.h = { ...noHandlers, ...(opts.handlers ?? {}) } as Required<FirehoseHandlers>;
    this.backoff = resolveBackoff(opts.backoff);
    this.sched = opts.scheduler ?? systemScheduler;
    this.wsf = opts.webSocketFactory ?? defaultWebSocketFactory;
    this.rng = opts.rng ?? Math.random;
    this.log = opts.logger ?? noopLogger;
    this.maxCursorRuns = Math.max(1, opts.maxCursorRuns ?? 256);
  }

  get state(): FirehoseState {
    return this.stateVal;
  }

  start(): this {
    if (this.started) return this;
    this.started = true;
    this.userClosed = false;
    void this.connect();
    return this;
  }

  /**
   * Open or replace a subscription. Takes effect on the wire when connected;
   * otherwise it is applied on the next (re)connect. `cursors` resumes specific
   * runs from a seq.
   */
  subscribe(id: string, filter: EventFilter = {}, cursors: Record<string, number> = {}): void {
    this.subs.set(id, { id, filter, cursors });
    for (const [runId, seq] of Object.entries(cursors)) this.cursors.advance(runId, seq);
    if (this.isOpen()) this.send({ type: "subscribe", id, filter, cursors: this.resumeCursors() });
  }

  /** Cancel a subscription. */
  unsubscribe(id: string): void {
    if (!this.subs.delete(id)) return;
    if (this.isOpen()) this.send({ type: "unsubscribe", id } satisfies UnsubscribeMessage);
  }

  close(): void {
    if (this.userClosed) return;
    this.userClosed = true;
    this.clearTimer();
    this.connectGen++;
    if (this.ws) {
      try {
        this.ws.close(1000, "client close");
      } catch {
        /* ignore */
      }
      this.ws = null;
    }
    this.setState("closed");
    this.h.onClosed();
  }

  private isOpen(): boolean {
    return this.ws !== null && this.stateVal === "live";
  }

  private setState(s: FirehoseState): void {
    if (s === this.stateVal) return;
    const prev = this.stateVal;
    this.stateVal = s;
    this.h.onState(s, prev);
  }

  private clearTimer(): void {
    if (this.timer !== null) {
      this.sched.clearTimeout(this.timer);
      this.timer = null;
    }
  }

  private send(msg: SubscribeMessage | UnsubscribeMessage): void {
    if (!this.ws) return;
    try {
      this.ws.send(JSON.stringify(msg));
    } catch (err) {
      this.log.warn("firehose: send failed", { error: String(err) });
    }
  }

  /** The bounded resume cursor map, preferring non-terminal runs. */
  private resumeCursors(): Record<string, number> {
    const all = this.cursors.runIds();
    const live = all.filter((r) => !this.terminal.has(r));
    const done = all.filter((r) => this.terminal.has(r));
    const chosen = [...live, ...done].slice(0, this.maxCursorRuns);
    const out: Record<string, number> = {};
    for (const r of chosen) out[r] = this.cursors.lastSeq(r);
    return out;
  }

  private async connect(): Promise<void> {
    if (this.userClosed) return;
    const gen = ++this.connectGen;
    this.setState("connecting");

    let ticket: string;
    try {
      ticket = await resolveTicket(this.o.auth, "firehose", {
        baseUrl: this.o.baseUrl,
        ...(this.o.fetchImpl ? { fetchImpl: this.o.fetchImpl } : {}),
      });
    } catch (err) {
      if (gen !== this.connectGen || this.userClosed) return;
      this.h.onError(err as Error);
      this.scheduleReconnect();
      return;
    }
    if (gen !== this.connectGen || this.userClosed) return;

    const query = new URLSearchParams({ ticket });
    const url = toWebSocketUrl(this.o.baseUrl, "/v1/events/ws", query);

    let ws: WebSocketLike;
    try {
      ws = this.wsf(url);
    } catch (err) {
      if (gen !== this.connectGen || this.userClosed) return;
      this.h.onError(err as Error);
      this.scheduleReconnect();
      return;
    }
    this.ws = ws;

    ws.onopen = () => {
      if (gen !== this.connectGen) return;
      this.attempt = 0;
      this.setState("live");
      // Re-send every desired subscription with the tracked resume cursors.
      const resume = this.resumeCursors();
      for (const sub of this.subs.values()) {
        this.send({ type: "subscribe", id: sub.id, filter: sub.filter, cursors: resume });
      }
    };
    ws.onmessage = (ev) => {
      if (gen === this.connectGen) this.onMessage(ev.data);
    };
    ws.onerror = () => {
      if (gen === this.connectGen) this.log.warn("firehose ws error");
    };
    ws.onclose = (ev) => {
      if (gen !== this.connectGen) return;
      this.ws = null;
      this.onClose(ev.code, ev.reason);
    };
  }

  private onMessage(data: unknown): void {
    const frame = parseFrame(data);
    if (!frame) {
      this.log.warn("firehose: unparseable frame");
      return;
    }
    switch (frame.type) {
      case "event": {
        const f = frame as unknown as EventFrame;
        this.deliver(f.event, f.subscriptions ?? []);
        return;
      }
      case "subscribed": {
        const f = frame as unknown as SubscribedFrame;
        this.h.onSubscribed(f.id, f.filter);
        return;
      }
      case "unsubscribed": {
        const f = frame as unknown as UnsubscribedFrame;
        this.h.onUnsubscribed(f.id);
        return;
      }
      case "caught_up": {
        const f = frame as unknown as FirehoseCaughtUpFrame;
        for (const [runId, seq] of Object.entries(f.cursors ?? {})) this.cursors.advance(runId, seq);
        this.h.onCaughtUp(f.id, f.cursors ?? {});
        return;
      }
      case "error": {
        const f = frame as unknown as ErrorFrame;
        // In-band error: the connection stays open (16.4). Surface, don't close.
        this.h.onError(new Error(`firehose error${f.id ? ` [${f.id}]` : ""}: ${f.code}: ${f.message}`));
        return;
      }
      default:
        this.log.warn("firehose: unknown frame type", { type: frame.type });
    }
  }

  private deliver(env: EventEnvelope, subscriptions: string[]): void {
    if (typeof env?.seq !== "number" || typeof env.run_id !== "string") return;
    const runId = env.run_id;
    const c = this.cursors.classify(runId, env.seq);
    if (c.kind === "duplicate") return;
    if (c.kind === "gap") {
      this.log.warn("firehose: seq gap (delivering; server backfill heals)", {
        run_id: runId,
        expected: c.expected,
        got: env.seq,
      });
    }
    this.cursors.advance(runId, env.seq);
    if (TERMINAL.includes(env.type)) this.terminal.add(runId);
    else this.terminal.delete(runId); // a requeue revived it
    this.h.onEvent(env, subscriptions);
  }

  private onClose(code: number, reason: string): void {
    if (this.userClosed) return;
    this.log.info("firehose closed", { code, reason });
    if (code === CLOSE_SLOW_CONSUMER) {
      this.reconnectNow();
      return;
    }
    this.scheduleReconnect();
  }

  private reconnectNow(): void {
    if (this.userClosed) return;
    this.clearTimer();
    this.connectGen++;
    if (this.ws) {
      try {
        this.ws.close(1000, "resync");
      } catch {
        /* ignore */
      }
      this.ws = null;
    }
    this.setState("reconnecting");
    this.timer = this.sched.setTimeout(() => {
      this.timer = null;
      void this.connect();
    }, 0);
  }

  private scheduleReconnect(): void {
    if (this.userClosed) return;
    const delay = backoffDelay(this.attempt, this.backoff, this.rng);
    this.attempt++;
    this.setState("reconnecting");
    this.log.info("firehose scheduling reconnect", { attempt: this.attempt, delayMs: delay });
    this.timer = this.sched.setTimeout(() => {
      this.timer = null;
      void this.connect();
    }, delay);
  }
}

/** Convenience: construct and start a firehose stream. */
export function tailFirehose(opts: FirehoseOptions): FirehoseStream {
  return new FirehoseStream(opts).start();
}
