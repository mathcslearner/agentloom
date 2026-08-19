"use client";

/**
 * Ops views (ticket 18.6): the operator's queue-health panel and the cross-run
 * dead-letter list with a requeue action. The DLQ list is live over the
 * firehose (a requeue closes its row; a fresh death appears) and keyset-paged;
 * requeue is scope-gated (`submit`). The queue-health panel polls
 * `GET /v1/system/stats`.
 */
import { Fragment, Suspense, useCallback, useEffect, useMemo, useState } from "react";
import Link from "next/link";
import { useRouter, useSearchParams } from "next/navigation";
import { problem } from "@agentloom/api-client";
import { listDeadLetters, requeueStep } from "@/lib/dashboard/streams";
import { useDeadLetterList } from "@/lib/dashboard/useDeadLetterList";
import { usePermissions } from "@/lib/permissions";
import {
  dlqFiltersFromSearchParams,
  dlqFiltersToQuery,
  dlqFiltersToSearchParams,
  dlqKey,
  type DlqFilters,
  type DlqRow,
  type DlqSource,
} from "@/lib/pure/dashboard/dead-letters";
import { QueueHealthPanel } from "@/components/dashboard/QueueHealthPanel";
import { ConnectionPill } from "@/components/dashboard/ConnectionPill";
import { JsonViewer } from "@/components/dashboard/inspector/JsonViewer";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";
import { toast } from "@/components/ui/toast";

const SOURCES: DlqSource[] = ["retries_exhausted", "permanent", "poison"];

export default function OpsPage() {
  return (
    <Suspense fallback={<p className="text-sm text-muted-foreground">Loading…</p>}>
      <OpsView />
    </Suspense>
  );
}

function OpsView() {
  const router = useRouter();
  const searchParams = useSearchParams();
  const filters = useMemo(
    () => dlqFiltersFromSearchParams(new URLSearchParams(searchParams.toString())),
    [searchParams],
  );
  const filterKey = JSON.stringify(filters);

  const [rows, setRows] = useState<DlqRow[]>([]);
  const [cursor, setCursor] = useState<string | undefined>(undefined);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<string | undefined>(undefined);
  const [expanded, setExpanded] = useState<string | undefined>(undefined);

  const perms = usePermissions();
  const requeueGate = perms.controlState("requeue");

  const load = useCallback(
    async (opts: { reset: boolean; cursor?: string }) => {
      setLoading(true);
      setError(undefined);
      try {
        const page = await listDeadLetters({ limit: 50, ...dlqFiltersToQuery(filters), cursor: opts.cursor });
        const next: DlqRow[] = page.dead_letters.map((d) => ({ ...d, lastSeq: 0 }));
        setRows((prev) => (opts.reset ? next : [...prev, ...next]));
        setCursor(page.next_cursor);
      } catch (err) {
        setError(problem(err)?.message ?? "failed to load dead letters");
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

  const connection = useDeadLetterList({ filters, rows, setRows });

  const applyFilters = useCallback(
    (next: DlqFilters) => {
      const p = dlqFiltersToSearchParams(next);
      router.replace(p.toString() ? `/ops?${p.toString()}` : "/ops");
    },
    [router],
  );

  const doRequeue = useCallback(async (row: DlqRow) => {
    const outcome = await requeueStep(row.run_id, row.step_id);
    if (outcome.kind === "ok") {
      toast({ title: `Requeued ${row.step_id}`, variant: "success" });
      // The live step_requeued event closes the row; reflect immediately too.
      setRows((prev) => prev.map((r) => (dlqKey(r) === dlqKey(row) ? { ...r, open: false, step_status: "ready" } : r)));
    } else {
      toast({ title: "Requeue failed", description: outcome.message, variant: "error" });
    }
  }, []);

  return (
    <div className="space-y-6">
      <div className="flex items-center justify-between">
        <h1 className="text-xl font-semibold tracking-tight">Ops</h1>
        <ConnectionPill state={connection} />
      </div>

      <QueueHealthPanel />

      <div className="space-y-3">
        <div className="flex flex-wrap items-center gap-2">
          <h2 className="text-base font-semibold">Dead letters</h2>
          <div className="ml-2 flex items-center gap-1">
            <Button
              size="sm"
              data-testid="dlq-filter-open"
              variant={filters.status === "open" ? "default" : "outline"}
              onClick={() => applyFilters({ ...filters, status: "open" })}
            >
              open
            </Button>
            <Button
              size="sm"
              data-testid="dlq-filter-all"
              variant={filters.status === "all" ? "default" : "outline"}
              onClick={() => applyFilters({ ...filters, status: "all" })}
            >
              all
            </Button>
          </div>
          <div className="flex items-center gap-1">
            {SOURCES.map((s) => (
              <Button
                key={s}
                size="sm"
                data-testid={`dlq-source-${s}`}
                variant={filters.source === s ? "default" : "outline"}
                onClick={() => applyFilters({ ...filters, source: filters.source === s ? undefined : s })}
              >
                {s}
              </Button>
            ))}
          </div>
          {filters.runId ? (
            <Button size="sm" variant="outline" onClick={() => applyFilters({ ...filters, runId: undefined })}>
              run: {filters.runId.slice(0, 8)}… ✕
            </Button>
          ) : null}
        </div>

        {error ? (
          <p className="text-sm text-red-600 dark:text-red-400">{error}</p>
        ) : rows.length === 0 && !loading ? (
          <p className="text-sm text-muted-foreground" data-testid="dlq-empty">
            No dead letters match this filter.
          </p>
        ) : (
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>Step</TableHead>
                <TableHead>Run</TableHead>
                <TableHead>Source</TableHead>
                <TableHead>Attempts</TableHead>
                <TableHead>Status</TableHead>
                <TableHead />
              </TableRow>
            </TableHeader>
            <TableBody>
              {rows.map((r) => {
                const key = dlqKey(r);
                const isOpen = expanded === key;
                return (
                  <Fragment key={key}>
                    <TableRow data-testid="dlq-row" data-run-id={r.run_id} data-step-id={r.step_id} data-open={r.open}>
                      <TableCell>
                        <button
                          type="button"
                          className="font-medium hover:underline"
                          data-testid="dlq-expand"
                          onClick={() => setExpanded(isOpen ? undefined : key)}
                        >
                          {r.step_id}
                        </button>
                        <span className="ml-2 text-xs text-muted-foreground">{r.step_type}</span>
                      </TableCell>
                      <TableCell className="font-mono text-xs">
                        <Link href={`/runs/${r.run_id}`} className="text-primary hover:underline">
                          {r.run_id.slice(0, 8)}…
                        </Link>
                      </TableCell>
                      <TableCell className="text-xs">
                        {r.source}
                        {r.class ? <span className="text-muted-foreground"> · {r.class}</span> : null}
                      </TableCell>
                      <TableCell className="tabular-nums">{r.attempts_at_death}</TableCell>
                      <TableCell>
                        <Badge variant={r.open ? "failed" : "muted"}>{r.open ? "open" : r.step_status}</Badge>
                      </TableCell>
                      <TableCell className="text-right">
                        {r.open && requeueGate !== "hidden" ? (
                          <Button
                            size="sm"
                            disabled={requeueGate === "disabled"}
                            data-testid="dlq-requeue"
                            onClick={() => void doRequeue(r)}
                          >
                            Requeue
                          </Button>
                        ) : null}
                      </TableCell>
                    </TableRow>
                    {isOpen ? (
                      <TableRow>
                        <TableCell colSpan={6}>
                          <div className="text-xs" data-testid="dlq-error">
                            <div className="mb-1 text-muted-foreground">Error at death</div>
                            <JsonViewer value={r.error ?? { note: "no error document" }} />
                          </div>
                        </TableCell>
                      </TableRow>
                    ) : null}
                  </Fragment>
                );
              })}
            </TableBody>
          </Table>
        )}

        {cursor ? (
          <Button variant="outline" size="sm" disabled={loading} onClick={() => void load({ reset: false, cursor })}>
            {loading ? "Loading…" : "Load more"}
          </Button>
        ) : null}
      </div>
    </div>
  );
}
