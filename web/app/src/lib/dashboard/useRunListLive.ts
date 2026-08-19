"use client";

/**
 * Live run-list overlay (ticket 18.1). Subscribes to the multi-run firehose and
 * folds events onto the REST-loaded rows: existing rows update in place (status
 * chips, counters, cost), and a `run_created` for a run that matches the active
 * filters is fetched and prepended. Rows resume after their REST `event_seq`
 * (the per-row cursor), so no event is missed or double-counted.
 */
import { useEffect, useRef, useState } from "react";
import type { FirehoseState } from "@agentloom/engine-client";
import type { RunView } from "@agentloom/api-client";
import { createFirehose } from "@/lib/dashboard/streams";
import { useRuntimeConfig } from "@/lib/runtime-config";
import {
  applyRunListEvent,
  matchesFilters,
  RUN_LIST_EVENT_TYPES,
  type RunFilters,
} from "@/lib/pure/dashboard/run-list";

export interface UseRunListLiveArgs {
  filters: RunFilters;
  /** Current rows (REST-loaded); the hook reads them via a ref to avoid
   * re-subscribing when they change. */
  rows: RunView[];
  setRows: React.Dispatch<React.SetStateAction<RunView[]>>;
  /** Fetch one run's view (through the proxy) to materialize a discovered run. */
  fetchRun: (id: string) => Promise<RunView | null>;
}

export function useRunListLive({ filters, rows, setRows, fetchRun }: UseRunListLiveArgs): FirehoseState {
  const { apiPublicUrl } = useRuntimeConfig();
  const [connection, setConnection] = useState<FirehoseState>("connecting");
  const rowsRef = useRef(rows);
  rowsRef.current = rows;
  const filtersRef = useRef(filters);
  filtersRef.current = filters;
  const discovering = useRef(new Set<string>());

  // Re-subscribe when the filter identity changes.
  const filterKey = JSON.stringify(filters);

  useEffect(() => {
    const fh = createFirehose(apiPublicUrl, {
      onState: (s) => setConnection(s),
      onEvent: (env) => {
        const id = env.run_id;
        const existing = rowsRef.current.find((r) => r.id === id);
        if (existing) {
          setRows((prev) => prev.map((r) => (r.id === id ? applyRunListEvent(r, env) : r)));
          return;
        }
        // A run we haven't loaded: materialize it if it now matches the filters
        // (a fresh submission appears at the top without a refresh).
        if (env.type !== "run_created") return;
        if (discovering.current.has(id)) return;
        discovering.current.add(id);
        void fetchRun(id)
          .then((row) => {
            if (row && matchesFilters(row, filtersRef.current)) {
              setRows((prev) => (prev.some((r) => r.id === row.id) ? prev : [row, ...prev]));
            }
          })
          .finally(() => discovering.current.delete(id));
      },
    });
    fh.start();
    const cursors: Record<string, number> = {};
    for (const r of rowsRef.current) cursors[r.id] = r.event_seq;
    const filter: { types: string[]; definition_id?: string } = {
      types: [...RUN_LIST_EVENT_TYPES],
    };
    if (filters.definitionId) filter.definition_id = filters.definitionId;
    fh.subscribe("runs", filter, cursors);
    return () => fh.close();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [apiPublicUrl, filterKey]);

  return connection;
}
