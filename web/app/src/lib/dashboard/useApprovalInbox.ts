"use client";

/**
 * Live approvals-inbox overlay (ticket 18.5). The inbox mirror of
 * useRunListLive: subscribes to the multi-run firehose for approval events and
 * folds them onto the REST-loaded rows. An `approval_*` event for a row already
 * in the list updates it in place (status/decision); an `approval_requested`
 * for an unknown approval fetches that run's approvals page and merges any rows
 * matching the active filters (a fresh gate appears without a refresh).
 *
 * Firehose filters are run-scoped (there is no approval-id filter), so a run
 * filter narrows the subscription by `run_ids`; the status filter is applied
 * client-side (`matchesInboxFilters`).
 */
import { useEffect, useRef, useState } from "react";
import type { FirehoseState } from "@agentloom/engine-client";
import { createFirehose, listApprovals } from "@/lib/dashboard/streams";
import { useRuntimeConfig } from "@/lib/runtime-config";
import {
  applyInboxEvent,
  matchesInboxFilters,
  upsertRow,
  INBOX_EVENT_TYPES,
  type InboxFilters,
  type InboxRow,
} from "@/lib/pure/dashboard/approval-list";

export interface UseApprovalInboxArgs {
  filters: InboxFilters;
  rows: InboxRow[];
  setRows: React.Dispatch<React.SetStateAction<InboxRow[]>>;
}

export function useApprovalInbox({ filters, rows, setRows }: UseApprovalInboxArgs): FirehoseState {
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
        const p = env.payload as { approval_id?: string };
        const approvalId = p.approval_id;
        if (!approvalId) return;
        const existing = rowsRef.current.find((r) => r.id === approvalId);
        if (existing) {
          setRows((prev) => prev.map((r) => (r.id === approvalId ? applyInboxEvent(r, env) : r)));
          return;
        }
        // A gate we haven't loaded: fetch that run's approvals and merge the
        // matching rows (a new gate appears at its oldest-first position).
        if (env.type !== "approval_requested") return;
        const runId = env.run_id;
        if (discovering.current.has(approvalId)) return;
        discovering.current.add(approvalId);
        void listApprovals({ run_id: runId, limit: 200 })
          .then((page) => {
            setRows((prev) => {
              let next = prev;
              for (const v of page.approvals) {
                if (!matchesInboxFilters(v, filtersRef.current)) continue;
                if (next.some((r) => r.id === v.id)) continue;
                next = upsertRow(next, { ...v, lastSeq: env.seq });
              }
              return next;
            });
          })
          .finally(() => discovering.current.delete(approvalId));
      },
    });
    fh.start();
    const filter: { types: string[]; run_ids?: string[] } = { types: [...INBOX_EVENT_TYPES] };
    if (filters.runId) filter.run_ids = [filters.runId];
    // Per-run cursors: resume each tracked run just before its earliest row so
    // no approval event is missed. Approvals carry no event_seq, so we start
    // each run's tracking from 0 (a full re-backfill; deduped by id).
    const cursors: Record<string, number> = {};
    for (const r of rowsRef.current) if (r.run_id) cursors[r.run_id] = 0;
    fh.subscribe("approvals", filter, cursors);
    return () => fh.close();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [apiPublicUrl, filterKey]);

  return connection;
}
