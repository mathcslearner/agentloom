"use client";

import { Suspense, useCallback, useEffect, useMemo, useRef, useState } from "react";
import Link from "next/link";
import { useRouter, useSearchParams } from "next/navigation";
import { problem, type RunStatus, type RunView } from "@agentloom/api-client";
import { browserApi } from "@/lib/api/browser";
import { runStatusVariant } from "@/lib/status";
import {
  filtersFromSearchParams,
  filtersToQuery,
  filtersToSearchParams,
  presetCutoff,
  type RunFilters,
} from "@/lib/pure/dashboard/run-list";
import { useRunListLive } from "@/lib/dashboard/useRunListLive";
import { useDefinitionLabels } from "@/lib/dashboard/useDefinitionLabels";
import { ConnectionPill } from "@/components/dashboard/ConnectionPill";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";

const STATUS_FILTERS: (RunStatus | "all")[] = [
  "all",
  "running",
  "succeeded",
  "failed",
  "parked",
  "cancelling",
  "cancelled",
];

const TIME_PRESETS = ["all", "15m", "1h", "24h", "7d"] as const;

export default function RunsPage() {
  return (
    <Suspense fallback={<p className="text-sm text-muted-foreground">Loading…</p>}>
      <RunsView />
    </Suspense>
  );
}

function RunsView() {
  const router = useRouter();
  const searchParams = useSearchParams();
  const filters = useMemo(
    () => filtersFromSearchParams(new URLSearchParams(searchParams.toString())),
    [searchParams],
  );
  const [preset, setPreset] = useState<string>("all");

  const [runs, setRuns] = useState<RunView[]>([]);
  const [cursor, setCursor] = useState<string | undefined>(undefined);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<string | undefined>(undefined);
  const filterKey = JSON.stringify(filters);

  const load = useCallback(
    async (opts: { reset: boolean; cursor?: string }) => {
      setLoading(true);
      setError(undefined);
      const query: {
        limit: number;
        cursor?: string;
        status?: RunStatus;
        definition_id?: string;
        created_after?: string;
        created_before?: string;
      } = { limit: 25, ...filtersToQuery(filters) };
      if (!opts.reset && opts.cursor) query.cursor = opts.cursor;

      const { data, error: err } = await browserApi().GET("/v1/runs", { params: { query } });
      setLoading(false);
      if (err) {
        setError(problem(err)?.message ?? "failed to load runs");
        return;
      }
      setRuns((prev) => (opts.reset ? data.runs : [...prev, ...data.runs]));
      setCursor(data.next_cursor);
    },
    [filters],
  );

  // Reload from the top whenever the filter set changes.
  useEffect(() => {
    void load({ reset: true });
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [filterKey]);

  const fetchRun = useCallback(async (id: string): Promise<RunView | null> => {
    const { data, error: err } = await browserApi().GET("/v1/runs/{run_id}", {
      params: { path: { run_id: id } },
    });
    if (err || !data) return null;
    return data.run;
  }, []);

  const connection = useRunListLive({ filters, rows: runs, setRows: setRuns, fetchRun });
  const labels = useDefinitionLabels(runs);

  const applyFilters = useCallback(
    (next: RunFilters) => {
      const p = filtersToSearchParams(next);
      router.replace(p.toString() ? `/runs?${p.toString()}` : "/runs");
    },
    [router],
  );

  const nowRef = useRef(Date.now());

  return (
    <div className="space-y-4">
      <div className="flex items-center justify-between">
        <h1 className="text-xl font-semibold tracking-tight">Runs</h1>
        <ConnectionPill state={connection} />
      </div>

      <div className="flex flex-wrap items-center gap-2">
        <span className="text-xs text-muted-foreground">Status</span>
        {STATUS_FILTERS.map((s) => (
          <Button
            key={s}
            size="sm"
            variant={(s === "all" ? !filters.status : filters.status === s) ? "default" : "outline"}
            onClick={() => applyFilters({ ...filters, status: s === "all" ? undefined : s })}
          >
            {s}
          </Button>
        ))}
      </div>

      <div className="flex flex-wrap items-center gap-2">
        <span className="text-xs text-muted-foreground">Created</span>
        {TIME_PRESETS.map((p) => (
          <Button
            key={p}
            size="sm"
            variant={preset === p ? "default" : "outline"}
            onClick={() => {
              setPreset(p);
              applyFilters({ ...filters, createdAfter: presetCutoff(p, nowRef.current) });
            }}
          >
            {p}
          </Button>
        ))}
        {filters.definitionId ? (
          <Button size="sm" variant="outline" onClick={() => applyFilters({ ...filters, definitionId: undefined })}>
            definition: {labels[filters.definitionId]?.name ?? filters.definitionId.slice(0, 8)} ✕
          </Button>
        ) : null}
      </div>

      {error ? (
        <p className="text-sm text-red-600 dark:text-red-400">{error}</p>
      ) : runs.length === 0 && !loading ? (
        <p className="text-sm text-muted-foreground">No runs match this filter.</p>
      ) : (
        <Table>
          <TableHeader>
            <TableRow>
              <TableHead>ID</TableHead>
              <TableHead>Status</TableHead>
              <TableHead>Definition</TableHead>
              <TableHead>Steps</TableHead>
              <TableHead>Created</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {runs.map((r) => (
              <TableRow key={r.id} data-testid="run-row" data-run-id={r.id} data-status={r.status}>
                <TableCell className="font-mono text-xs">
                  <Link href={`/runs/${r.id}`} className="text-primary hover:underline">
                    {r.id.slice(0, 8)}…
                  </Link>
                </TableCell>
                <TableCell>
                  <Badge variant={runStatusVariant(r.status)} data-testid="run-status">
                    {r.status}
                  </Badge>
                </TableCell>
                <TableCell className="text-muted-foreground">
                  {r.definition_id ? (
                    <button
                      className="hover:underline"
                      onClick={() => applyFilters({ ...filters, definitionId: r.definition_id })}
                    >
                      {labels[r.definition_id]
                        ? `${labels[r.definition_id]!.name} v${labels[r.definition_id]!.version}`
                        : `${r.definition_id.slice(0, 8)}…`}
                    </button>
                  ) : (
                    <span className="italic">inline</span>
                  )}
                </TableCell>
                <TableCell className="text-muted-foreground tabular-nums">
                  {r.steps_succeeded}/{r.steps_total} ok
                  {r.steps_failed > 0 ? `, ${r.steps_failed} failed` : ""}
                </TableCell>
                <TableCell className="text-muted-foreground">
                  {new Date(r.created_at).toLocaleString()}
                </TableCell>
              </TableRow>
            ))}
          </TableBody>
        </Table>
      )}

      <div className="flex items-center gap-3">
        {cursor ? (
          <Button variant="outline" size="sm" disabled={loading} onClick={() => void load({ reset: false, cursor })}>
            {loading ? "Loading…" : "Load more"}
          </Button>
        ) : null}
        {loading && runs.length === 0 ? <span className="text-sm text-muted-foreground">Loading…</span> : null}
      </div>
    </div>
  );
}
