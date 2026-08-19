"use client";

/**
 * Live queue-health polling (ticket 18.6). No event carries queue depth, so the
 * queue-health panel polls `GET /v1/system/stats` on an interval. Polling pauses
 * while the tab is hidden (visibilitychange) and resumes on return. The timer
 * and clock are injectable so the reducer/hook tests drive it deterministically.
 */
import { useCallback, useEffect, useRef, useState } from "react";
import type { SystemStatsResponse } from "@agentloom/api-client";
import { fetchSystemStats } from "@/lib/dashboard/streams";

export interface SystemStatsState {
  stats?: SystemStatsResponse;
  error?: string;
  loading: boolean;
  /** Fetch once now (also called by the interval). */
  refresh: () => Promise<void>;
}

export function useSystemStats(intervalMs = 5000): SystemStatsState {
  const [stats, setStats] = useState<SystemStatsResponse>();
  const [error, setError] = useState<string>();
  const [loading, setLoading] = useState(true);
  const inflight = useRef(false);

  const refresh = useCallback(async () => {
    if (inflight.current) return;
    inflight.current = true;
    try {
      const s = await fetchSystemStats();
      setStats(s);
      setError(undefined);
    } catch {
      setError("failed to load system stats");
    } finally {
      setLoading(false);
      inflight.current = false;
    }
  }, []);

  useEffect(() => {
    void refresh();
    let timer: ReturnType<typeof setInterval> | undefined;
    const start = () => {
      if (timer === undefined && document.visibilityState === "visible") {
        timer = setInterval(() => void refresh(), intervalMs);
      }
    };
    const stop = () => {
      if (timer !== undefined) {
        clearInterval(timer);
        timer = undefined;
      }
    };
    const onVisibility = () => {
      if (document.visibilityState === "visible") {
        void refresh();
        start();
      } else {
        stop();
      }
    };
    start();
    document.addEventListener("visibilitychange", onVisibility);
    return () => {
      stop();
      document.removeEventListener("visibilitychange", onVisibility);
    };
  }, [refresh, intervalMs]);

  return { stats, error, loading, refresh };
}
