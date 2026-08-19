"use client";

/**
 * React glue for the run-detail controller (ticket 18.1). Builds a
 * `RunController` over a live `RunStream` (browser auth: proxy-minted ticket,
 * direct WS to the public API origin) and exposes its state via
 * `useSyncExternalStore`.
 */
import { useEffect, useMemo, useSyncExternalStore } from "react";
import { RunController, type RunDashboardState } from "@/lib/dashboard/run-controller";
import { createRunStream, fetchRun, fetchRunGraph } from "@/lib/dashboard/streams";
import { useRuntimeConfig } from "@/lib/runtime-config";

export function useRunController(runId: string): { state: RunDashboardState; controller: RunController } {
  const { apiPublicUrl } = useRuntimeConfig();
  const controller = useMemo(
    () =>
      new RunController(
        (handlers) => createRunStream(apiPublicUrl, runId, handlers),
        () => fetchRunGraph(runId),
        () => fetchRun(runId),
      ),
    [apiPublicUrl, runId],
  );
  useEffect(() => {
    controller.start();
    return () => controller.stop();
  }, [controller]);
  const state = useSyncExternalStore(controller.subscribe, controller.getSnapshot, controller.getSnapshot);
  return { state, controller };
}
