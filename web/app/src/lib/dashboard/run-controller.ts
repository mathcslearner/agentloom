/**
 * Run-detail controller (ticket 18.1). Owns one run event stream and folds its
 * snapshot + live events into a UI state object, exposed through the
 * subscribe/getSnapshot contract `useSyncExternalStore` wants.
 *
 * Render discipline:
 *   - the snapshot publishes immediately (the page shows run state at once);
 *   - events arriving during backfill are buffered and applied to derived run
 *     state in one batch at `caught_up` (no flicker while catching up);
 *   - live events apply individually.
 * Every event, backfilled or live, is appended to the timeline list. The
 * underlying `RunStream` dedupes by seq, so the list is gap-free and dup-free
 * across reconnects — the seq-resume the DoD exercises through the UI.
 *
 * The stream itself is injected (`streamFactory`), so tests drive the real
 * `RunStream` recovery logic against a fake socket + clock.
 */
import type { EventEnvelope, RunStreamState } from "@agentloom/engine-client";
import type { RunResponse } from "@agentloom/api-client";
import { applyEvent, fromSnapshot, type RunState } from "@/lib/pure/dashboard/run-state";

export type ConnectionState = "idle" | RunStreamState;

export interface RunDashboardState {
  connection: ConnectionState;
  /** Reconnect count since the first successful connection (DoD-2 signal). */
  reconnects: number;
  /** Highest seq reflected (the resume cursor). */
  lastSeq: number;
  run: RunState | null;
  events: EventEnvelope[];
  error?: string;
}

export interface StreamLike {
  start(): unknown;
  close(): void;
}

export interface RunStreamHandlersLike {
  onSnapshot?(run: RunResponse): void;
  onEvent?(env: EventEnvelope): void;
  onCaughtUp?(lastSeq: number): void;
  onState?(state: RunStreamState, prev: RunStreamState): void;
  onError?(err: Error): void;
  onClosed?(): void;
}

export type RunStreamFactory = (handlers: RunStreamHandlersLike) => StreamLike;

const MAX_EVENTS = 5000;

export class RunController {
  private state: RunDashboardState = {
    connection: "idle",
    reconnects: 0,
    lastSeq: 0,
    run: null,
    events: [],
  };
  private listeners = new Set<() => void>();
  private stream: StreamLike | null = null;
  private backfillBuffer: EventEnvelope[] = [];
  private catchingUp = false;
  private everConnected = false;

  constructor(private readonly streamFactory: RunStreamFactory) {}

  start(): void {
    if (this.stream) return;
    this.stream = this.streamFactory({
      onSnapshot: (run) => this.onSnapshot(run),
      onEvent: (env) => this.onEvent(env),
      onCaughtUp: (seq) => this.onCaughtUp(seq),
      onState: (s) => this.onState(s),
      onError: (err) => this.set({ error: err.message }),
    });
    this.stream.start();
  }

  stop(): void {
    this.stream?.close();
    this.stream = null;
  }

  subscribe = (cb: () => void): (() => void) => {
    this.listeners.add(cb);
    return () => this.listeners.delete(cb);
  };

  getSnapshot = (): RunDashboardState => this.state;

  private onSnapshot(run: RunResponse): void {
    // A fresh snapshot re-seeds derived state (also on reconnect); events that
    // follow re-apply the tail, and the seq guard keeps it idempotent.
    this.catchingUp = true;
    this.backfillBuffer = [];
    this.set({ run: fromSnapshot(run), error: undefined });
  }

  private onEvent(env: EventEnvelope): void {
    const events = appendCapped(this.state.events, env);
    const lastSeq = Math.max(this.state.lastSeq, env.seq);
    if (this.catchingUp) {
      this.backfillBuffer.push(env);
      this.set({ events, lastSeq });
      return;
    }
    const run = this.state.run ? applyEvent(this.state.run, env) : this.state.run;
    this.set({ events, lastSeq, run });
  }

  private onCaughtUp(seq: number): void {
    this.catchingUp = false;
    this.everConnected = true;
    let run = this.state.run;
    if (run) {
      for (const env of this.backfillBuffer) run = applyEvent(run, env);
    }
    this.backfillBuffer = [];
    this.set({ run, lastSeq: Math.max(this.state.lastSeq, seq), connection: "live" });
  }

  private onState(s: RunStreamState): void {
    if (s === "live") this.everConnected = true;
    // A reconnect is a return to connecting/reconnecting after we were live.
    const isReconnect =
      this.everConnected && (s === "connecting" || s === "reconnecting") && this.state.connection === "live";
    this.set({
      connection: s,
      reconnects: isReconnect ? this.state.reconnects + 1 : this.state.reconnects,
    });
  }

  private set(patch: Partial<RunDashboardState>): void {
    this.state = { ...this.state, ...patch };
    for (const cb of this.listeners) cb();
  }
}

function appendCapped(list: EventEnvelope[], env: EventEnvelope): EventEnvelope[] {
  const next = [...list, env];
  return next.length > MAX_EVENTS ? next.slice(next.length - MAX_EVENTS) : next;
}
