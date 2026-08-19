/**
 * Pure system-stats derivations (ticket 18.6): the health tiers and staleness
 * the queue-health panel renders from `GET /v1/system/stats`. Thresholds are
 * dev-scale by design (like the M7.5 alert rules) — the panel is a glanceable
 * operator signal, not an SLA.
 *
 * No React/UI imports — under the app's `pure/` eslint boundary.
 */
import type { SystemStatsResponse } from "@agentloom/api-client";

export type HealthTier = "ok" | "warn" | "danger";

/** A metric tile: a label, a value, and a tier for colour. */
export interface StatTile {
  key: string;
  label: string;
  value: number | string;
  tier: HealthTier;
}

/** Tier a count against a warn/danger threshold (danger wins). */
function tierAt(n: number, warn: number, danger: number): HealthTier {
  if (n >= danger) return "danger";
  if (n >= warn) return "warn";
  return "ok";
}

/**
 * Derive the panel's tiles from a stats snapshot. The queue block is present
 * only when queue introspection is available; the Postgres-sourced tiles
 * (DLQ, runs, outbox) always render.
 */
export function deriveStatTiles(stats: SystemStatsResponse): StatTile[] {
  const tiles: StatTile[] = [];
  if (stats.queue) {
    const q = stats.queue;
    tiles.push({ key: "ready", label: "Ready", value: q.ready_depth, tier: tierAt(q.ready_depth, 100, 1000) });
    tiles.push({ key: "pending", label: "In-flight (PEL)", value: q.pending, tier: "ok" });
    tiles.push({ key: "delayed", label: "Delayed", value: q.delayed, tier: "ok" });
    tiles.push({
      key: "workers",
      label: "Workers",
      value: `${q.workers_active}/${q.workers.length}`,
      tier: q.workers_active === 0 ? "danger" : "ok",
    });
  }
  tiles.push({
    key: "dead_letters",
    label: "Dead letters",
    value: stats.dead_letters.open,
    tier: tierAt(stats.dead_letters.open, 1, 25),
  });
  tiles.push({
    key: "outbox",
    label: "Outbox backlog",
    value: stats.outbox.backlog,
    tier: tierAt(stats.outbox.backlog, 50, 500),
  });
  tiles.push({ key: "runs", label: "Active runs", value: stats.runs.active, tier: "ok" });
  return tiles;
}

/** Staleness of a snapshot: how long ago it was observed, and whether that is
 * beyond the poll interval (a sign the panel stopped updating). */
export interface Staleness {
  ageMs: number;
  stale: boolean;
  label: string;
}

export function deriveStaleness(stats: SystemStatsResponse, now: number, intervalMs: number): Staleness {
  const observed = Date.parse(stats.observed_at);
  const ageMs = Math.max(0, now - observed);
  const stale = ageMs > intervalMs * 3;
  const secs = Math.round(ageMs / 1000);
  const label = secs < 1 ? "just now" : secs < 60 ? `${secs}s ago` : `${Math.round(secs / 60)}m ago`;
  return { ageMs, stale, label };
}
