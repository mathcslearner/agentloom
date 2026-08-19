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
import type { RunResponse, RunGraphResponse } from "@agentloom/api-client";
import { applyEvent, fromSnapshot, mergeRunResponse, type RunState } from "@/lib/pure/dashboard/run-state";
import {
  applyGraphEvent,
  emptyTopology,
  mergeTopology,
  topologyFromGraph,
  topologyFromSnapshot,
  type GraphTopology,
} from "@/lib/pure/dashboard/graph-topology";

export type ConnectionState = "idle" | RunStreamState;

export interface RunDashboardState {
  connection: ConnectionState;
  /** Reconnect count since the first successful connection (DoD-2 signal). */
  reconnects: number;
  /** Highest seq reflected (the resume cursor). */
  lastSeq: number;
  run: RunState | null;
  /** The live DAG topology (18.2): union of snapshot ∪ graph read ∪ expansions. */
  topology: GraphTopology;
  events: EventEnvelope[];
  error?: string;
  /** Set when the run-graph read failed; the canvas degrades to the snapshot
   * topology rather than going blank. */
  graphError?: string;
}

/** Fetches the run-graph introspection view (GET /v1/runs/{id}/graph). Injected
 * so tests drive the controller without a network. */
export type GraphFetcher = () => Promise<RunGraphResponse>;

/** Fetches the run-detail body (GET /v1/runs/{id}) so the inspector picks up
 * new attempts/verdicts/logs cursors (ticket 18.3). Injected for tests. */
export type RunFetcher = () => Promise<RunResponse>;

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
    topology: emptyTopology(),
    events: [],
  };
  private listeners = new Set<() => void>();
  private stream: StreamLike | null = null;
  private backfillBuffer: EventEnvelope[] = [];
  private catchingUp = false;
  private everConnected = false;
  private graphFetched = false;

  private refreshing = false;

  constructor(
    private readonly streamFactory: RunStreamFactory,
    private readonly graphFetcher?: GraphFetcher,
    private readonly runFetcher?: RunFetcher,
  ) {}

  /**
   * Refetch the run-detail body and fold its step views into state (ticket
   * 18.3): the inspector calls this when a selected step advanced past its
   * captured view seq, so new attempts/verdicts appear without a page reload.
   * Coalesces concurrent calls; failures are surfaced on `error` but never
   * disturb the live event-derived state.
   */
  async refreshViews(): Promise<void> {
    if (this.refreshing || !this.runFetcher || !this.state.run) return;
    this.refreshing = true;
    try {
      const body = await this.runFetcher();
      if (this.state.run) {
        this.set({ run: mergeRunResponse(this.state.run, body) });
      }
    } catch (err) {
      this.set({ error: err instanceof Error ? err.message : String(err) });
    } finally {
      this.refreshing = false;
    }
  }

  start(): void {
    if (this.stream) return;
    void this.fetchGraph();
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
    // follow re-apply the tail, and the seq guard keeps it idempotent. The
    // snapshot topology is merged into whatever the graph read already brought
    // (union — expansion is additive-only, so order does not matter).
    this.catchingUp = true;
    this.backfillBuffer = [];
    const topology = mergeTopology(this.state.topology, topologyFromSnapshot(run));
    this.set({ run: fromSnapshot(run), topology, error: undefined });
  }

  private onEvent(env: EventEnvelope): void {
    const events = appendCapped(this.state.events, env);
    const lastSeq = Math.max(this.state.lastSeq, env.seq);
    if (this.catchingUp) {
      this.backfillBuffer.push(env);
      // Apply topology-shaping events during backfill too (idempotent), so an
      // expansion that happened before this connection is reflected at once.
      const topology = applyGraphEvent(this.state.topology, env);
      this.set({ events, lastSeq, topology });
      return;
    }
    const run = this.state.run ? applyEvent(this.state.run, env) : this.state.run;
    const topology = applyGraphEvent(this.state.topology, env);
    this.set({ events, lastSeq, run, topology });
    // A new gate parked (18.5): the event carries no payload/edit_schema, so
    // pull the authoritative record from the run body — cheap and rare — so the
    // decision dialog can render the proposed action.
    if (env.type === "approval_requested") void this.refreshViews();
  }

  private onCaughtUp(seq: number): void {
    this.catchingUp = false;
    this.everConnected = true;
    let run = this.state.run;
    let topology = this.state.topology;
    if (run) {
      for (const env of this.backfillBuffer) {
        run = applyEvent(run, env);
        topology = applyGraphEvent(topology, env);
      }
    }
    this.backfillBuffer = [];
    this.set({ run, topology, lastSeq: Math.max(this.state.lastSeq, seq), connection: "live" });
  }

  private async fetchGraph(): Promise<void> {
    if (this.graphFetched || !this.graphFetcher) return;
    this.graphFetched = true;
    try {
      const graph = await this.graphFetcher();
      // Union with whatever the snapshot / events already brought.
      this.set({ topology: mergeTopology(this.state.topology, topologyFromGraph(graph)), graphError: undefined });
    } catch (err) {
      // Degrade to the snapshot topology; the canvas still renders.
      this.set({ graphError: err instanceof Error ? err.message : String(err) });
    }
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
