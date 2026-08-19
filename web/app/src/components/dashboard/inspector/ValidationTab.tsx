"use client";

import { useMemo, useState } from "react";
import type { StepView } from "@agentloom/api-client";
import { Badge } from "@/components/ui/badge";
import { hasPromptDiff, promptDiff, verdictRows } from "@/lib/pure/dashboard/inspector";
import { diffStats } from "@/lib/pure/dashboard/diff";

function statusVariant(status?: string): "succeeded" | "failed" | "muted" {
  if (status === "pass") return "succeeded";
  if (status === "fail") return "failed";
  return "muted";
}

/** The semantic-retry prompt diff (the killer demo, DoD-2). */
function PromptDiff({ step }: { step: StepView }) {
  const attempts = (step.attempts ?? []).map((a) => a.attempt);
  const [aAttempt, setA] = useState(attempts[0] ?? 1);
  const [bAttempt, setB] = useState(attempts[1] ?? attempts[0] ?? 1);
  const hunks = useMemo(() => promptDiff(step, aAttempt, bAttempt), [step, aAttempt, bAttempt]);
  const stats = diffStats(hunks);

  return (
    <section className="space-y-2" data-testid="prompt-diff" data-added-lines={stats.add} data-deleted-lines={stats.del}>
      <div className="flex items-center gap-2">
        <h4 className="font-medium text-foreground">Prompt diff</h4>
        <select
          className="rounded border bg-background px-1 py-0.5 text-[11px]"
          value={aAttempt}
          onChange={(e) => setA(Number(e.target.value))}
          aria-label="from attempt"
        >
          {attempts.map((n) => (
            <option key={n} value={n}>
              #{n}
            </option>
          ))}
        </select>
        <span className="text-muted-foreground">→</span>
        <select
          className="rounded border bg-background px-1 py-0.5 text-[11px]"
          value={bAttempt}
          onChange={(e) => setB(Number(e.target.value))}
          aria-label="to attempt"
        >
          {attempts.map((n) => (
            <option key={n} value={n}>
              #{n}
            </option>
          ))}
        </select>
        <span className="text-[10px] text-muted-foreground">
          +{stats.add} −{stats.del}
        </span>
      </div>
      <pre className="max-h-64 overflow-auto rounded-md border bg-muted/40 p-2 font-mono text-[11px] leading-relaxed">
        {hunks.map((h, i) => (
          <div
            key={i}
            className={
              h.kind === "add"
                ? "bg-emerald-500/15 text-emerald-800 dark:text-emerald-300"
                : h.kind === "del"
                  ? "bg-red-500/15 text-red-800 line-through dark:text-red-300"
                  : "text-muted-foreground"
            }
          >
            {h.kind === "add" ? "+ " : h.kind === "del" ? "− " : "  "}
            {h.text || " "}
          </div>
        ))}
      </pre>
    </section>
  );
}

export function ValidationTab({ step }: { step: StepView }) {
  const rows = verdictRows(step);
  return (
    <div className="space-y-5 text-xs" data-testid="inspector-validation">
      <section className="space-y-2">
        <h4 className="font-medium text-foreground">Verdicts</h4>
        {rows.length === 0 ? (
          <p className="italic text-muted-foreground">This step has no validation chain.</p>
        ) : (
          <ul className="space-y-2" data-testid="verdict-list">
            {rows.map((v) => (
              <li key={v.attempt} className="rounded-md border p-2" data-attempt={v.attempt} data-status={v.status}>
                <div className="flex items-center gap-2">
                  <span className="tabular-nums text-muted-foreground">#{v.attempt}</span>
                  <Badge variant={statusVariant(v.status)} className="text-[10px]">
                    {v.status ?? "—"}
                  </Badge>
                  {v.score != null ? <span className="tabular-nums text-muted-foreground">score {v.score.toFixed(2)}</span> : null}
                </div>
                {v.issues.length > 0 ? (
                  <ul className="mt-1 space-y-0.5">
                    {v.issues.map((iss, i) => (
                      <li key={i} className="text-red-600 dark:text-red-400">
                        <span className="font-mono">{iss.validator}</span>
                        {iss.path ? <span className="text-muted-foreground"> {iss.path}</span> : null}: {iss.message}
                      </li>
                    ))}
                  </ul>
                ) : null}
                {v.results.some((r) => r.rationale) ? (
                  <ul className="mt-1 space-y-0.5 text-muted-foreground">
                    {v.results
                      .filter((r) => r.rationale)
                      .map((r, i) => (
                        <li key={i}>
                          <span className="font-mono">{r.validator}</span>: {r.rationale}
                        </li>
                      ))}
                  </ul>
                ) : null}
              </li>
            ))}
          </ul>
        )}
      </section>

      {hasPromptDiff(step) ? <PromptDiff step={step} /> : null}
    </div>
  );
}
