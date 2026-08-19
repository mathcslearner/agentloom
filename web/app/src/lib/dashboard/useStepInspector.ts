"use client";

/**
 * React glue for the step inspector (ticket 18.3). Given the selected step's
 * live state, it:
 *   - triggers a run-detail refetch (`controller.refreshViews`) when the step
 *     advances past the seq its cached `view` was captured at, so new
 *     attempts/verdicts appear without a page reload;
 *   - fetches the run cost breakdown, refetching on a `cost_updated` event;
 *   - fetches step logs for a chosen attempt/level, and — in follow mode while
 *     the attempt is non-terminal — polls the tail (logs are poll-based in v1,
 *     ADR-018 / M18 backlog).
 *
 * All fetchers are injected through the module functions in `streams.ts`; a
 * unit test drives the pure reducers directly, so this hook stays thin.
 */
import { useCallback, useEffect, useRef, useState } from "react";
import type { RunCostResponse } from "@agentloom/api-client";
import type { RunController } from "@/lib/dashboard/run-controller";
import { fetchRunCost, fetchStepLogs } from "@/lib/dashboard/streams";
import { appendLogPage, emptyLogState, type LogLevel, type LogState } from "@/lib/pure/dashboard/logs";
import type { StepState } from "@/lib/pure/dashboard/run-state";

const LOG_FOLLOW_INTERVAL_MS = 2000;

/** Refetch the detail body when the selected step moved past its view seq. */
export function useStepDetailRefresh(controller: RunController, step: StepState | undefined): void {
  const lastSeenSeq = useRef<number>(-1);
  useEffect(() => {
    if (!step) return;
    const seq = step.lastEventSeq ?? 0;
    const viewSeq = step.viewSeq ?? -1;
    // The step advanced beyond what its cached view reflects — pull a fresh body.
    if (seq > viewSeq && seq !== lastSeenSeq.current) {
      lastSeenSeq.current = seq;
      void controller.refreshViews();
    }
  }, [controller, step]);
}

/** Fetch and live-refresh a run's cost breakdown. */
export function useRunCost(runId: string, costSeq: number): RunCostResponse | undefined {
  const [cost, setCost] = useState<RunCostResponse>();
  useEffect(() => {
    let alive = true;
    fetchRunCost(runId)
      .then((c) => {
        if (alive) setCost(c);
      })
      .catch(() => {
        /* cost is best-effort in the inspector */
      });
    return () => {
      alive = false;
    };
  }, [runId, costSeq]);
  return cost;
}

/**
 * Fetch step logs for an attempt at a minimum level, paging to the end. In
 * follow mode, re-poll the tail on an interval while `terminal` is false.
 */
export function useStepLogs(
  runId: string,
  stepId: string | undefined,
  attempt: number | undefined,
  level: LogLevel,
  follow: boolean,
  terminal: boolean,
): { state: LogState; loading: boolean; loadMore: () => void } {
  const [state, setState] = useState<LogState>(emptyLogState());
  const [loading, setLoading] = useState(false);
  const cursorRef = useRef<string | undefined>(undefined);

  // Reset when the target (step / attempt / level) changes.
  useEffect(() => {
    setState(emptyLogState());
    cursorRef.current = undefined;
  }, [runId, stepId, attempt, level]);

  const page = useCallback(
    async (cursor?: string) => {
      if (!stepId || attempt === undefined) return;
      setLoading(true);
      try {
        const p = await fetchStepLogs(runId, stepId, { attempt, level, cursor });
        cursorRef.current = p.next_cursor;
        setState((s) => appendLogPage(s, p));
      } catch {
        /* logs best-effort */
      } finally {
        setLoading(false);
      }
    },
    [runId, stepId, attempt, level],
  );

  // Initial load to the end of the current tail.
  useEffect(() => {
    if (!stepId || attempt === undefined) return;
    void page(undefined);
  }, [page, stepId, attempt]);

  // Follow: poll the tail while the attempt is still running.
  useEffect(() => {
    if (!follow || terminal || !stepId || attempt === undefined) return;
    const id = setInterval(() => void page(cursorRef.current), LOG_FOLLOW_INTERVAL_MS);
    return () => clearInterval(id);
  }, [follow, terminal, page, stepId, attempt]);

  const loadMore = useCallback(() => {
    if (cursorRef.current) void page(cursorRef.current);
  }, [page]);

  return { state, loading, loadMore };
}
