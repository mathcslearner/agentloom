"use client";

/**
 * Queue-health panel (ticket 18.6): live queue depth / PEL / delayed / DLQ /
 * active-runs tiles plus the worker roster, polled from `GET /v1/system/stats`.
 * No event carries queue depth, so the panel polls; an "as of" indicator shows
 * staleness. When queue introspection is unwired the queue tiles are absent and
 * the Postgres-sourced tiles still render.
 */
import { useSystemStats } from "@/lib/dashboard/useSystemStats";
import { deriveStatTiles, deriveStaleness, type HealthTier } from "@/lib/pure/dashboard/system-stats";
import type { SystemStatsResponse } from "@agentloom/api-client";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Badge } from "@/components/ui/badge";

const TIER_CLASS: Record<HealthTier, string> = {
  ok: "text-foreground",
  warn: "text-amber-600 dark:text-amber-400",
  danger: "text-red-600 dark:text-red-400",
};

const INTERVAL_MS = 5000;

export function QueueHealthPanel() {
  const { stats, error, loading } = useSystemStats(INTERVAL_MS);

  return (
    <Card data-testid="queue-health">
      <CardHeader className="flex flex-row items-center justify-between space-y-0">
        <CardTitle className="text-base">Queue health</CardTitle>
        {stats ? <AsOf stats={stats} now={Date.now()} /> : null}
      </CardHeader>
      <CardContent>
        {error && !stats ? (
          <p className="text-sm text-red-600 dark:text-red-400">{error}</p>
        ) : !stats ? (
          <p className="text-sm text-muted-foreground">{loading ? "Loading…" : "No data"}</p>
        ) : (
          <div className="space-y-4">
            {stats.queue === null ? (
              <p className="text-xs text-amber-600 dark:text-amber-400" data-testid="queue-unavailable">
                Queue introspection unavailable{stats.queue_error ? `: ${stats.queue_error}` : ""}
              </p>
            ) : null}
            <div className="grid grid-cols-2 gap-3 sm:grid-cols-3 lg:grid-cols-4">
              {deriveStatTiles(stats).map((t) => (
                <div key={t.key} className="rounded-md border p-3" data-testid={`stat-${t.key}`}>
                  <div className="text-xs text-muted-foreground">{t.label}</div>
                  <div className={`text-xl font-semibold tabular-nums ${TIER_CLASS[t.tier]}`}>{t.value}</div>
                </div>
              ))}
            </div>
            {stats.queue && stats.queue.workers.length > 0 ? (
              <div>
                <div className="mb-1 text-xs font-medium text-muted-foreground">
                  Workers on {stats.queue.stream}/{stats.queue.group}
                </div>
                <ul className="space-y-1 text-xs" data-testid="worker-list">
                  {stats.queue.workers.map((w) => (
                    <li key={w.id} className="flex items-center gap-2 font-mono" data-testid="worker-row">
                      <Badge variant={w.active ? "running" : "muted"} className="text-[9px]">
                        {w.active ? "active" : "idle"}
                      </Badge>
                      <span className="truncate">{w.id}</span>
                      <span className="ml-auto text-muted-foreground">
                        {w.pending} pel · {Math.round(w.idle_ms / 1000)}s idle
                      </span>
                    </li>
                  ))}
                </ul>
              </div>
            ) : null}
          </div>
        )}
      </CardContent>
    </Card>
  );
}

function AsOf({ stats, now }: { stats: SystemStatsResponse; now: number }) {
  const s = deriveStaleness(stats, now, INTERVAL_MS);
  return (
    <span
      className={`text-xs ${s.stale ? "text-amber-600 dark:text-amber-400" : "text-muted-foreground"}`}
      data-testid="stats-asof"
    >
      as of {s.label}
    </span>
  );
}
