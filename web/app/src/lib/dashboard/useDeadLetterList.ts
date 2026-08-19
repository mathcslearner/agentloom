"use client";

/**
 * Live DLQ-list overlay (ticket 18.6). The DLQ mirror of useApprovalInbox:
 * subscribes to the multi-run firehose for dead-letter/requeue events and folds
 * them onto the REST-loaded rows. A `step_requeued` for a listed row closes it
 * (no longer open); a `step_dead_lettered` for a row already listed keeps it
 * open; a `step_dead_lettered` for a step not yet listed re-fetches that run's
 * dead-letters (the error context lives only on the REST body, like the 18.5
 * partial approval) and merges rows matching the active filters.
 *
 * Firehose filters are run-scoped, so a run filter narrows the subscription by
 * `run_ids`; the status/source filters are applied client-side.
 */
import { useEffect, useRef, useState } from "react";
import type { FirehoseState } from "@agentloom/engine-client";
import { createFirehose, listDeadLetters } from "@/lib/dashboard/streams";
import { useRuntimeConfig } from "@/lib/runtime-config";
import {
  applyDlqEvent,
  dlqKey,
  matchesDlqFilters,
  upsertDlqRow,
  DLQ_EVENT_TYPES,
  type DlqFilters,
  type DlqRow,
} from "@/lib/pure/dashboard/dead-letters";

export interface UseDeadLetterListArgs {
  filters: DlqFilters;
  rows: DlqRow[];
  setRows: React.Dispatch<React.SetStateAction<DlqRow[]>>;
}

export function useDeadLetterList({ filters, rows, setRows }: UseDeadLetterListArgs): FirehoseState {
  const { apiPublicUrl } = useRuntimeConfig();
  const [connection, setConnection] = useState<FirehoseState>("connecting");
  const rowsRef = useRef(rows);
  rowsRef.current = rows;
  const filtersRef = useRef(filters);
  filtersRef.current = filters;
  const discovering = useRef(new Set<string>());

  const filterKey = JSON.stringify(filters);

  useEffect(() => {
    const fh = createFirehose(apiPublicUrl, {
      onState: (s) => setConnection(s),
      onEvent: (env) => {
        const stepId = (env.payload as { step_id?: string }).step_id;
        if (!stepId) return;
        const touched = rowsRef.current.some((r) => r.run_id === env.run_id && r.step_id === stepId);
        if (touched) {
          setRows((prev) => prev.map((r) => applyDlqEvent(r, env)));
          return;
        }
        // A death we haven't loaded: fetch that run's dead-letters and merge
        // rows matching the filters (a fresh death appears with its error).
        if (env.type !== "step_dead_lettered") return;
        const runId = env.run_id;
        const marker = `${runId}/${stepId}/${env.seq}`;
        if (discovering.current.has(marker)) return;
        discovering.current.add(marker);
        void listDeadLetters({ run_id: runId, status: "all", limit: 200 })
          .then((page) => {
            setRows((prev) => {
              let next = prev;
              for (const v of page.dead_letters) {
                if (!matchesDlqFilters(v, filtersRef.current)) continue;
                if (next.some((r) => dlqKey(r) === dlqKey(v))) continue;
                next = upsertDlqRow(next, { ...v, lastSeq: env.seq });
              }
              return next;
            });
          })
          .finally(() => discovering.current.delete(marker));
      },
    });
    fh.start();
    const filter: { types: string[]; run_ids?: string[] } = { types: [...DLQ_EVENT_TYPES] };
    if (filters.runId) filter.run_ids = [filters.runId];
    const cursors: Record<string, number> = {};
    for (const r of rowsRef.current) cursors[r.run_id] = 0;
    fh.subscribe("dead-letters", filter, cursors);
    return () => fh.close();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [apiPublicUrl, filterKey]);

  return connection;
}
