"use client";

import { useCallback, useEffect, useState } from "react";
import { problem, type RunStatus, type RunView } from "@agentloom/api-client";
import { browserApi } from "@/lib/api/browser";
import { runStatusVariant } from "@/lib/status";
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

/**
 * Client component: lists runs through the same-origin proxy (no key in the
 * browser). Exercises the proxy client path, keyset "Load more" pagination, and
 * the status filter.
 */
export default function RunsPage() {
  const [runs, setRuns] = useState<RunView[]>([]);
  const [cursor, setCursor] = useState<string | undefined>(undefined);
  const [status, setStatus] = useState<RunStatus | "all">("all");
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<string | undefined>(undefined);

  const load = useCallback(
    async (opts: { reset: boolean }) => {
      setLoading(true);
      setError(undefined);
      const query: { limit: number; cursor?: string; status?: RunStatus } = { limit: 25 };
      if (!opts.reset && cursor) query.cursor = cursor;
      if (status !== "all") query.status = status;

      const { data, error: err } = await browserApi().GET("/v1/runs", { params: { query } });
      setLoading(false);
      if (err) {
        setError(problem(err)?.message ?? "failed to load runs");
        return;
      }
      setRuns((prev) => (opts.reset ? data.runs : [...prev, ...data.runs]));
      setCursor(data.next_cursor);
    },
    [cursor, status],
  );

  // Reload from the top whenever the status filter changes.
  useEffect(() => {
    void load({ reset: true });
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [status]);

  return (
    <div className="space-y-4">
      <div className="flex items-center justify-between">
        <h1 className="text-xl font-semibold tracking-tight">Runs</h1>
        <div className="flex flex-wrap gap-1">
          {STATUS_FILTERS.map((s) => (
            <Button
              key={s}
              size="sm"
              variant={s === status ? "default" : "outline"}
              onClick={() => setStatus(s)}
            >
              {s}
            </Button>
          ))}
        </div>
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
              <TableHead>Steps</TableHead>
              <TableHead>Created</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {runs.map((r) => (
              <TableRow key={r.id}>
                <TableCell className="font-mono text-xs">{r.id}</TableCell>
                <TableCell>
                  <Badge variant={runStatusVariant(r.status)}>{r.status}</Badge>
                </TableCell>
                <TableCell className="text-muted-foreground">
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
          <Button variant="outline" size="sm" disabled={loading} onClick={() => void load({ reset: false })}>
            {loading ? "Loading…" : "Load more"}
          </Button>
        ) : null}
        {loading && runs.length === 0 ? <span className="text-sm text-muted-foreground">Loading…</span> : null}
      </div>
    </div>
  );
}
