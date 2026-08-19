"use client";

/**
 * The human-approval inbox (ticket 18.5). Lists approvals oldest-first (the
 * `GET /v1/approvals` order), live over the firehose, filterable by status
 * (default `pending`) and run. Each row shows the gate title, its run, an aging
 * indicator, a deadline countdown, and — for a pending gate — a Decide action
 * opening the decision dialog. Decided/expired rows show the outcome.
 */
import { Suspense, useCallback, useEffect, useMemo, useRef, useState } from "react";
import Link from "next/link";
import { useRouter, useSearchParams } from "next/navigation";
import { problem } from "@agentloom/api-client";
import { listApprovals } from "@/lib/dashboard/streams";
import { useApprovalInbox } from "@/lib/dashboard/useApprovalInbox";
import {
  inboxFiltersFromSearchParams,
  inboxFiltersToQuery,
  inboxFiltersToSearchParams,
  type InboxFilters,
  type InboxRow,
  type InboxStatus,
} from "@/lib/pure/dashboard/approval-list";
import { deriveAging, deriveDeadline } from "@/lib/pure/dashboard/approval-aging";
import { decidable, parkExpired, type ApprovalRecord } from "@/lib/pure/dashboard/approvals";
import { decisionOutcome } from "@/lib/pure/dashboard/decision-outcome";
import { DecisionDialog } from "@/components/dashboard/DecisionDialog";
import { ConnectionPill } from "@/components/dashboard/ConnectionPill";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";

const STATUS_FILTERS: (InboxStatus | "all")[] = [
  "pending",
  "approved",
  "rejected",
  "expired",
  "cancelled",
  "all",
];

export default function ApprovalsPage() {
  return (
    <Suspense fallback={<p className="text-sm text-muted-foreground">Loading…</p>}>
      <ApprovalsView />
    </Suspense>
  );
}

function toRecord(row: InboxRow): ApprovalRecord {
  return { ...row, partial: false };
}

function ApprovalsView() {
  const router = useRouter();
  const searchParams = useSearchParams();
  const filters = useMemo(
    () => inboxFiltersFromSearchParams(new URLSearchParams(searchParams.toString())),
    [searchParams],
  );
  const filterKey = JSON.stringify(filters);

  const [rows, setRows] = useState<InboxRow[]>([]);
  const [cursor, setCursor] = useState<string | undefined>(undefined);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<string | undefined>(undefined);
  const [decideId, setDecideId] = useState<string | undefined>(undefined);
  const nowRef = useRef(Date.now());

  const load = useCallback(
    async (opts: { reset: boolean; cursor?: string }) => {
      setLoading(true);
      setError(undefined);
      const query: { status?: string; run_id?: string; limit: number; cursor?: string } = {
        limit: 50,
        ...inboxFiltersToQuery(filters),
      };
      if (!opts.reset && opts.cursor) query.cursor = opts.cursor;
      try {
        const page = await listApprovals(query);
        const next: InboxRow[] = page.approvals.map((a) => ({ ...a, lastSeq: 0 }));
        setRows((prev) => (opts.reset ? next : [...prev, ...next]));
        setCursor(page.next_cursor);
      } catch (err) {
        setError(problem(err)?.message ?? "failed to load approvals");
      } finally {
        setLoading(false);
      }
    },
    [filters],
  );

  useEffect(() => {
    void load({ reset: true });
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [filterKey]);

  const connection = useApprovalInbox({ filters, rows, setRows });

  const applyFilters = useCallback(
    (next: InboxFilters) => {
      const p = inboxFiltersToSearchParams(next);
      // Encode "all" explicitly so it survives a reload (absent ⇒ pending).
      if (!next.status) p.set("status", "all");
      router.replace(p.toString() ? `/approvals?${p.toString()}` : "/approvals");
    },
    [router],
  );

  const decideRow = decideId ? rows.find((r) => r.id === decideId) : undefined;

  return (
    <div className="space-y-4">
      <div className="flex items-center justify-between">
        <h1 className="text-xl font-semibold tracking-tight">Approvals</h1>
        <ConnectionPill state={connection} />
      </div>

      <div className="flex flex-wrap items-center gap-2">
        <span className="text-xs text-muted-foreground">Status</span>
        {STATUS_FILTERS.map((s) => (
          <Button
            key={s}
            size="sm"
            data-testid={`filter-${s}`}
            variant={(s === "all" ? !filters.status : filters.status === s) ? "default" : "outline"}
            onClick={() => applyFilters({ ...filters, status: s === "all" ? undefined : s })}
          >
            {s}
          </Button>
        ))}
        {filters.runId ? (
          <Button size="sm" variant="outline" onClick={() => applyFilters({ ...filters, runId: undefined })}>
            run: {filters.runId.slice(0, 8)}… ✕
          </Button>
        ) : null}
      </div>

      {error ? (
        <p className="text-sm text-red-600 dark:text-red-400">{error}</p>
      ) : rows.length === 0 && !loading ? (
        <p className="text-sm text-muted-foreground" data-testid="inbox-empty">
          No approvals match this filter.
        </p>
      ) : (
        <Table>
          <TableHeader>
            <TableRow>
              <TableHead>Gate</TableHead>
              <TableHead>Run</TableHead>
              <TableHead>Status</TableHead>
              <TableHead>Age</TableHead>
              <TableHead>Deadline</TableHead>
              <TableHead />
            </TableRow>
          </TableHeader>
          <TableBody>
            {rows.map((r) => {
              const rec = toRecord(r);
              const aging = deriveAging(rec, nowRef.current);
              const deadline = deriveDeadline(rec, nowRef.current);
              const pending = decidable(rec);
              return (
                <TableRow key={r.id} data-testid="approval-row" data-approval-id={r.id} data-status={r.status}>
                  <TableCell className="max-w-[18rem] truncate">
                    <span className="font-medium">{r.title || r.step_id}</span>
                    {r.allow_edit ? (
                      <Badge variant="outline" className="ml-2 text-[9px]">
                        editable
                      </Badge>
                    ) : null}
                    {!pending ? (
                      <span className="block text-xs text-muted-foreground">{decisionOutcome(rec)}</span>
                    ) : null}
                  </TableCell>
                  <TableCell className="font-mono text-xs">
                    {r.run_id ? (
                      <Link href={`/runs/${r.run_id}`} className="text-primary hover:underline">
                        {r.run_id.slice(0, 8)}…
                      </Link>
                    ) : (
                      "—"
                    )}
                  </TableCell>
                  <TableCell>
                    <Badge variant={pending ? "parked" : "muted"}>{r.status}</Badge>
                    {parkExpired(rec) ? (
                      <span className="ml-1 text-[10px] text-amber-600 dark:text-amber-400">parked</span>
                    ) : null}
                  </TableCell>
                  <TableCell>
                    <span
                      className={
                        aging.tier === "stale"
                          ? "text-red-600 dark:text-red-400"
                          : aging.tier === "aging"
                            ? "text-amber-600 dark:text-amber-400"
                            : "text-muted-foreground"
                      }
                      data-testid="approval-age"
                      data-tier={aging.tier}
                    >
                      {aging.label}
                    </span>
                  </TableCell>
                  <TableCell className="text-xs text-muted-foreground">
                    {pending && deadline ? (
                      <span className={deadline.overdue ? "text-red-600 dark:text-red-400" : ""}>{deadline.label}</span>
                    ) : (
                      "—"
                    )}
                  </TableCell>
                  <TableCell className="text-right">
                    {pending ? (
                      <Button size="sm" data-testid="row-decide" onClick={() => setDecideId(r.id)}>
                        Decide
                      </Button>
                    ) : null}
                  </TableCell>
                </TableRow>
              );
            })}
          </TableBody>
        </Table>
      )}

      <div className="flex items-center gap-3">
        {cursor ? (
          <Button variant="outline" size="sm" disabled={loading} onClick={() => void load({ reset: false, cursor })}>
            {loading ? "Loading…" : "Load more"}
          </Button>
        ) : null}
      </div>

      {decideRow ? (
        <DecisionDialog
          approval={toRecord(decideRow)}
          open={decideId !== undefined}
          onOpenChange={(o) => {
            if (!o) setDecideId(undefined);
          }}
          onDecided={(outcome) => {
            // Reflect the decision on the row at once; the live feed confirms.
            if (outcome.kind === "ok") {
              const decided = outcome.response.approval;
              setRows((prev) => prev.map((r) => (r.id === decided.id ? { ...r, ...decided, lastSeq: r.lastSeq } : r)));
            }
          }}
        />
      ) : null}
    </div>
  );
}

