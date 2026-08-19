/**
 * Run event stream (ADR-018, 16.3): connect → snapshot → backfill from
 * `last_seq` → live tail, with seq dedupe, resume, and reconnection backoff.
 *
 * Recovery is deterministic: every reconnect re-mints a ticket and reconnects
 * with `last_seq` = the highest seq seen, so the server re-sends a fresh
 * snapshot then backfills the exact tail — the union across reconnects is
 * gap-free and dup-free by construction.
 */
import type { AuthProvider } from "./auth.js";
import { resolveTicket } from "./auth.js";
import type { BackoffOptions } from "./backoff.js";
import { backoffDelay, resolveBackoff } from "./backoff.js";
import type { EventEnvelope, EventType } from "./generated/events.js";
import type { ErrorFrame, EventFrame, RunCaughtUpFrame, RunSnapshotFrame } from "./frames.js";
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

/** Event types that end a run's normal progress (a requeue can still resume it). */
export const TERMINAL_RUN_EVENTS: readonly EventType[] = [
  "run_succeeded",
  "run_failed",
  "run_cancelled",
];

export type RunStreamState =
  | "connecting"
  | "backfilling"
  | "live"
  | "reconnecting"
  | "closed";

export interface RunStreamHandlers<TRun = unknown> {
  /** The run snapshot; fired again on every reconnect (replace prior state). */
  onSnapshot?(run: TRun): void;
  /** Each event, ordered and deduped by seq. */
  onEvent?(env: EventEnvelope): void;
  /** Backfill complete for this connection; `lastSeq` is the resume cursor. */
  onCaughtUp?(lastSeq: number): void;
  /** State-machine transitions. */
  onState?(state: RunStreamState, prev: RunStreamState): void;
  /** Non-fatal errors (ticket mint failures, in-band error frames). */
  onError?(err: Error): void;
  /** The stream will not reconnect (user close). */
  onClosed?(): void;
}

export interface RunStreamOptions<TRun = unknown> {
  /** API origin, e.g. `http://localhost:8080`. */
  baseUrl: string;
  runId: string;
  auth: AuthProvider;
  /** Initial resume cursor (highest seq already processed). Default 0. */
  lastSeq?: number;
  /** Close the stream when a terminal run event arrives. Default false. */
  closeOnTerminal?: boolean;
  handlers?: RunStreamHandlers<TRun>;
  backoff?: Partial<BackoffOptions>;
  scheduler?: Scheduler;
  webSocketFactory?: WebSocketFactory;
  fetchImpl?: FetchLike;
  rng?: () => number;
  logger?: Logger;
}

const noHandlers: Required<RunStreamHandlers> = {
  onSnapshot: () => {},
  onEvent: () => {},
  onCaughtUp: () => {},
  onState: () => {},
  onError: () => {},
  onClosed: () => {},
};

export class RunStream<TRun = unknown> {
  private readonly o: RunStreamOptions<TRun>;
  private readonly h: Required<RunStreamHandlers<TRun>>;
  private readonly backoff: BackoffOptions;
  private readonly sched: Scheduler;
  private readonly wsf: WebSocketFactory;
  private readonly rng: () => number;
  private readonly log: Logger;

  private ws: WebSocketLike | null = null;
  private timer: TimerHandle | null = null;
  private connectGen = 0; // guards against a stale socket's late callbacks
  private attempt = 0; // reconnect attempt counter (reset on caught_up)
  private lastSeq: number;
  private started = false;
  private userClosed = false;
  private stateVal: RunStreamState = "closed";

  constructor(opts: RunStreamOptions<TRun>) {
    this.o = opts;
    this.h = { ...noHandlers, ...(opts.handlers ?? {}) } as Required<RunStreamHandlers<TRun>>;
    this.backoff = resolveBackoff(opts.backoff);
    this.sched = opts.scheduler ?? systemScheduler;
    this.wsf = opts.webSocketFactory ?? defaultWebSocketFactory;
    this.rng = opts.rng ?? Math.random;
    this.log = opts.logger ?? noopLogger;
    this.lastSeq = Math.max(0, opts.lastSeq ?? 0);
  }

  /** The highest seq processed so far (the resume cursor). */
  get cursor(): number {
    return this.lastSeq;
  }

  get state(): RunStreamState {
    return this.stateVal;
  }

  /** Begin streaming. Idempotent; a second call is a no-op. */
  start(): this {
    if (this.started) return this;
    this.started = true;
    this.userClosed = false;
    void this.connect();
    return this;
  }

  /** Stop for good. No further reconnects; safe to call multiple times. */
  close(): void {
    if (this.userClosed) return;
    this.userClosed = true;
    this.clearTimer();
    this.connectGen++; // invalidate any in-flight connect/callbacks
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

  private setState(s: RunStreamState): void {
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

  private async connect(): Promise<void> {
    if (this.userClosed) return;
    const gen = ++this.connectGen;
    this.setState("connecting");

    let ticket: string;
    try {
      ticket = await resolveTicket(this.o.auth, "run", {
        baseUrl: this.o.baseUrl,
        ...(this.o.fetchImpl ? { fetchImpl: this.o.fetchImpl } : {}),
        runId: this.o.runId,
      });
    } catch (err) {
      if (gen !== this.connectGen || this.userClosed) return;
      this.h.onError(err as Error);
      this.scheduleReconnect();
      return;
    }
    if (gen !== this.connectGen || this.userClosed) return;

    const query = new URLSearchParams({ ticket });
    if (this.lastSeq > 0) query.set("last_seq", String(this.lastSeq));
    const url = toWebSocketUrl(this.o.baseUrl, `/v1/runs/${encodeURIComponent(this.o.runId)}/ws`, query);

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
      /* server drives: snapshot arrives on its own */
    };
    ws.onmessage = (ev) => {
      if (gen === this.connectGen) this.onMessage(ev.data);
    };
    ws.onerror = () => {
      if (gen === this.connectGen) this.log.warn("run ws error");
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
      this.log.warn("run ws: unparseable frame");
      return;
    }
    switch (frame.type) {
      case "snapshot": {
        const f = frame as unknown as RunSnapshotFrame<TRun>;
        this.setState("backfilling");
        this.h.onSnapshot(f.run);
        return;
      }
      case "event": {
        const f = frame as unknown as EventFrame;
        this.deliver(f.event);
        return;
      }
      case "caught_up": {
        const f = frame as unknown as RunCaughtUpFrame;
        if (typeof f.last_seq === "number" && f.last_seq > this.lastSeq) {
          this.lastSeq = f.last_seq;
        }
        this.attempt = 0; // a healthy connection resets backoff
        this.setState("live");
        this.h.onCaughtUp(this.lastSeq);
        return;
      }
      case "error": {
        const f = frame as unknown as ErrorFrame;
        this.h.onError(new Error(`server error frame: ${f.code}: ${f.message}`));
        return;
      }
      default:
        this.log.warn("run ws: unknown frame type", { type: frame.type });
    }
  }

  private deliver(env: EventEnvelope): void {
    if (typeof env?.seq !== "number") return;
    if (env.seq <= this.lastSeq) return; // duplicate
    if (env.seq > this.lastSeq + 1) {
      // Should not happen (the server's Tailer is gap-free within a connection);
      // heal defensively by reconnecting with the current cursor.
      this.log.warn("run ws: seq gap, forcing resync", {
        expected: this.lastSeq + 1,
        got: env.seq,
      });
      this.reconnectNow();
      return;
    }
    this.lastSeq = env.seq;
    this.h.onEvent(env);
    if (this.o.closeOnTerminal && TERMINAL_RUN_EVENTS.includes(env.type)) {
      this.close();
    }
  }

  private onClose(code: number, reason: string): void {
    if (this.userClosed) return;
    this.log.info("run ws closed", { code, reason });
    if (code === CLOSE_SLOW_CONSUMER) {
      // Slow-consumer close: resume immediately with our cursor.
      this.reconnectNow();
      return;
    }
    this.scheduleReconnect();
  }

  /** Reconnect immediately (used for 4001 and forced resyncs). */
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
    this.log.info("run ws scheduling reconnect", { attempt: this.attempt, delayMs: delay });
    this.timer = this.sched.setTimeout(() => {
      this.timer = null;
      void this.connect();
    }, delay);
  }
}

/** Convenience: construct and start a run stream. */
export function tailRun<TRun = unknown>(opts: RunStreamOptions<TRun>): RunStream<TRun> {
  return new RunStream<TRun>(opts).start();
}
