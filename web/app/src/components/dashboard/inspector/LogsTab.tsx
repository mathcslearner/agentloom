"use client";

import { useState } from "react";
import type { StepView } from "@agentloom/api-client";
import { Button } from "@/components/ui/button";
import { useStepLogs } from "@/lib/dashboard/useStepInspector";
import { filterByLevel, type LogLevel } from "@/lib/pure/dashboard/logs";

const LEVELS: LogLevel[] = ["debug", "info", "warn", "error"];

const TERMINAL = new Set(["succeeded", "failed", "dead_lettered", "skipped", "cancelled", "collected"]);

function levelClass(level: string): string {
  switch (level) {
    case "error":
      return "text-red-600 dark:text-red-400";
    case "warn":
      return "text-amber-600 dark:text-amber-400";
    case "debug":
      return "text-muted-foreground";
    default:
      return "text-foreground";
  }
}

export function LogsTab({ runId, step }: { runId: string; step: StepView }) {
  // Attempt selector defaults to the latest.
  const attempts = (step.attempts ?? []).map((a) => a.attempt);
  const latest = attempts.length > 0 ? Math.max(...attempts) : step.attempt_count;
  const [attempt, setAttempt] = useState(latest || 1);
  const [level, setLevel] = useState<LogLevel>("info");
  const [follow, setFollow] = useState(false);
  const terminal = TERMINAL.has(step.status);

  const { state, loading, loadMore } = useStepLogs(runId, step.id, attempt, level, follow, terminal);
  const lines = filterByLevel(state.lines, level);

  return (
    <div className="flex min-h-0 flex-col gap-2 text-xs" data-testid="inspector-logs">
      <div className="flex flex-wrap items-center gap-2">
        <label className="flex items-center gap-1">
          <span className="text-muted-foreground">attempt</span>
          <select
            className="rounded border bg-background px-1 py-0.5 text-[11px]"
            value={attempt}
            onChange={(e) => setAttempt(Number(e.target.value))}
          >
            {(attempts.length > 0 ? attempts : [attempt]).map((n) => (
              <option key={n} value={n}>
                #{n}
              </option>
            ))}
          </select>
        </label>
        <label className="flex items-center gap-1">
          <span className="text-muted-foreground">level</span>
          <select
            className="rounded border bg-background px-1 py-0.5 text-[11px]"
            value={level}
            onChange={(e) => setLevel(e.target.value as LogLevel)}
          >
            {LEVELS.map((l) => (
              <option key={l} value={l}>
                {l}
              </option>
            ))}
          </select>
        </label>
        {!terminal ? (
          <label className="flex items-center gap-1">
            <input type="checkbox" checked={follow} onChange={(e) => setFollow(e.target.checked)} />
            <span className="text-muted-foreground">follow</span>
          </label>
        ) : null}
        {loading ? <span className="text-muted-foreground">loading…</span> : null}
      </div>

      {state.truncated ? (
        <p className="text-[11px] text-amber-600 dark:text-amber-400" data-testid="logs-truncated">
          ⚠ {state.droppedLines} line{state.droppedLines === 1 ? "" : "s"} dropped (capture buffer / ring cap).
        </p>
      ) : null}

      <div className="min-h-0 flex-1 overflow-auto rounded-md border bg-muted/40 p-2 font-mono text-[11px]" data-testid="log-lines">
        {lines.length === 0 ? (
          <p className="italic text-muted-foreground">No log lines at this level.</p>
        ) : (
          lines.map((l) => (
            <div key={l.seq} className={levelClass(l.level)} data-seq={l.seq}>
              <span className="text-muted-foreground">{l.level.padEnd(5)}</span> {l.message}
              {l.fields ? <span className="text-muted-foreground"> {JSON.stringify(l.fields)}</span> : null}
            </div>
          ))
        )}
      </div>

      {state.nextCursor ? (
        <Button variant="outline" size="sm" onClick={loadMore} className="self-start text-[11px]">
          Load more
        </Button>
      ) : null}
    </div>
  );
}
