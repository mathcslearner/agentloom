"use client";

import type { StepView } from "@agentloom/api-client";
import type { EventEnvelope } from "@agentloom/engine-client";
import { Badge } from "@/components/ui/badge";
import {
  attemptTimeline,
  claimHistory,
  modelHistory,
  stepDuration,
} from "@/lib/pure/dashboard/inspector";

function fmtDuration(ms?: number): string {
  if (ms === undefined) return "—";
  if (ms < 1000) return `${ms}ms`;
  return `${(ms / 1000).toFixed(ms < 10000 ? 2 : 1)}s`;
}

function outcomeVariant(outcome?: string): "succeeded" | "failed" | "parked" | "muted" {
  switch (outcome) {
    case "succeeded":
      return "succeeded";
    case "permanent":
    case "transient":
    case "timeout":
    case "validation_failed":
      return "failed";
    case "lost":
    case "throttled":
    case "budget_exceeded":
    case "cancelled":
      return "parked";
    default:
      return "muted";
  }
}

export function OverviewTab({ step, events }: { step: StepView; events: EventEnvelope[] }) {
  const attempts = attemptTimeline(step);
  const claims = claimHistory(step, events);
  const models = modelHistory(step, events);
  const idempotency = step.idempotency_key;
  const isLlm = step.type === "llm" || step.type === "agent" || step.type === "planner";

  return (
    <div className="space-y-5 text-xs" data-testid="inspector-overview">
      {/* Timings */}
      <section className="space-y-1">
        <h4 className="font-medium text-foreground">Timings</h4>
        <dl className="grid grid-cols-2 gap-x-3 gap-y-1">
          <dt className="text-muted-foreground">Started</dt>
          <dd className="tabular-nums">{step.started_at ?? "—"}</dd>
          <dt className="text-muted-foreground">Finished</dt>
          <dd className="tabular-nums">{step.finished_at ?? "—"}</dd>
          <dt className="text-muted-foreground">Duration</dt>
          <dd className="tabular-nums">{fmtDuration(stepDuration(step))}</dd>
          <dt className="text-muted-foreground">Attempts</dt>
          <dd className="tabular-nums">{step.attempt_count}</dd>
        </dl>
      </section>

      {/* Model chain */}
      {isLlm ? (
        <section className="space-y-1" data-testid="inspector-model">
          <h4 className="font-medium text-foreground">Model</h4>
          <div className="flex flex-wrap items-center gap-1.5">
            <span className="font-mono">{models.authored ?? "—"}</span>
            {models.downgrades.map((d, i) => (
              <span key={i} className="flex items-center gap-1" data-testid="model-downgrade">
                <span className="text-muted-foreground">→</span>
                <span className="font-mono">{d.toModel}</span>
                <Badge variant="parked" className="text-[10px]">
                  {d.trigger}
                </Badge>
              </span>
            ))}
            {models.served && models.served !== models.authored && models.downgrades.length === 0 ? (
              <>
                <span className="text-muted-foreground">served</span>
                <span className="font-mono">{models.served}</span>
              </>
            ) : null}
          </div>
        </section>
      ) : null}

      {/* Attempts timeline */}
      <section className="space-y-1">
        <h4 className="font-medium text-foreground">Attempt timeline</h4>
        <ul className="space-y-1" data-testid="attempt-timeline">
          {attempts.map((a) => (
            <li key={a.attempt} className="flex items-center gap-2" data-attempt={a.attempt}>
              <span className="w-6 text-right tabular-nums text-muted-foreground">#{a.attempt}</span>
              <Badge variant={outcomeVariant(a.outcome)} className="text-[10px]">
                {a.outcome ?? "running"}
              </Badge>
              {a.reclaimed ? <span className="text-amber-600 dark:text-amber-400">↻ reclaimed</span> : null}
              <span className="tabular-nums text-muted-foreground">{fmtDuration(a.durationMs)}</span>
              {a.workerId ? <span className="ml-auto font-mono text-[10px] text-muted-foreground">{a.workerId}</span> : null}
            </li>
          ))}
          {attempts.length === 0 ? <li className="italic text-muted-foreground">No attempts yet.</li> : null}
        </ul>
      </section>

      {/* Claim / worker history */}
      <section className="space-y-1">
        <h4 className="font-medium text-foreground">Claim history</h4>
        <table className="w-full text-[11px]" data-testid="claim-history">
          <thead className="text-muted-foreground">
            <tr>
              <th className="text-left font-normal">#</th>
              <th className="text-left font-normal">worker</th>
              <th className="text-left font-normal">claim</th>
            </tr>
          </thead>
          <tbody>
            {claims.map((c, i) => (
              <tr key={i} className={c.displaced ? "text-amber-600 dark:text-amber-400" : ""} data-worker={c.workerId ?? ""}>
                <td className="tabular-nums">{c.attempt >= 0 ? c.attempt : "—"}</td>
                <td className="font-mono">{c.workerId ?? "—"}</td>
                <td className="truncate font-mono" title={c.claimId}>
                  {c.claimId ? `${c.claimId.slice(0, 8)}…` : "—"}
                  {c.displaced ? " (displaced)" : ""}
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      </section>

      {/* Idempotency */}
      <section className="space-y-1">
        <h4 className="font-medium text-foreground">Idempotency key</h4>
        <code className="block break-all rounded bg-muted px-2 py-1 text-[11px]" data-testid="idempotency-key">
          {idempotency ?? "—"}
        </code>
      </section>
    </div>
  );
}
