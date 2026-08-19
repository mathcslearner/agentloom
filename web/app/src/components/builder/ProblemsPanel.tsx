"use client";

// ProblemsPanel (ticket 17.5) — the document-wide list of validation problems.
// Errors sort first (they block submit), then warnings; within a severity,
// document order. Each row names the offending element (step id / from→to edge /
// document), the message, and the machine code. Clicking a row selects and
// centres the element (focusIssue); hovering previews the highlight. This is the
// DoD-3 surface: "Problems panel focuses the offending node/edge on click".

import type { Issue } from "@agentloom/graphdef";
import { useBuilderStore } from "@/lib/builder/store";
import { useProblemsCtx } from "@/lib/builder/problems-context";
import { edgeIndexOfPath, stepIndexOfPath } from "@/lib/builder/problems";
import { edgeData } from "@/lib/builder/adapter";
import { cn } from "@/lib/utils";

function useLocationLabel(): (issue: Issue) => string {
  const nodes = useBuilderStore((s) => s.nodes);
  const edges = useBuilderStore((s) => s.edges);
  return (issue: Issue) => {
    const si = stepIndexOfPath(issue.path);
    if (si !== null && si >= 0 && si < nodes.length) return nodes[si]!.id;
    const ei = edgeIndexOfPath(issue.path);
    if (ei !== null && ei >= 0 && ei < edges.length) {
      const e = edges[ei]!;
      const edge = edgeData(e).edge;
      return edge ? `${edge.from} → ${edge.to}` : `edge ${ei}`;
    }
    return "definition";
  };
}

function severityRank(i: Issue): number {
  return i.severity === "error" ? 0 : 1;
}

export function ProblemsPanel({ className }: { className?: string }) {
  const { problems, focusIssue, previewIssue } = useProblemsCtx();
  const label = useLocationLabel();

  const ordered = [...problems.issues].sort((a, b) => severityRank(a) - severityRank(b));

  return (
    <div className={cn("flex min-h-0 flex-col", className)} aria-label="Problems">
      <div className="flex items-center justify-between border-b px-3 py-2">
        <h2 className="text-xs font-semibold uppercase tracking-wide text-muted-foreground">Problems</h2>
        <span className="text-[11px] text-muted-foreground" data-testid="problems-summary">
          {problems.errors.length} {problems.errors.length === 1 ? "error" : "errors"}
          {problems.warnings.length > 0 ? `, ${problems.warnings.length} warning${problems.warnings.length === 1 ? "" : "s"}` : ""}
        </span>
      </div>
      {ordered.length === 0 ? (
        <p className="px-3 py-3 text-sm text-muted-foreground" data-testid="problems-empty">
          No problems — the workflow validates.
        </p>
      ) : (
        <ul className="min-h-0 flex-1 overflow-auto" data-testid="problems-panel" onMouseLeave={() => previewIssue(null)}>
          {ordered.map((issue, i) => (
            <li key={`${issue.path}\t${issue.code}\t${i}`}>
              <button
                type="button"
                data-testid="problem-row"
                data-severity={issue.severity}
                onClick={() => focusIssue(issue)}
                onMouseEnter={() => previewIssue(issue)}
                className="flex w-full items-start gap-2 border-b px-3 py-2 text-left hover:bg-accent"
              >
                <span
                  className={cn(
                    "mt-1 h-2 w-2 shrink-0 rounded-full",
                    issue.severity === "error" ? "bg-destructive" : "bg-amber-500",
                  )}
                  aria-hidden
                />
                <span className="min-w-0 flex-1">
                  <span className="flex items-center justify-between gap-2">
                    <span className="truncate text-xs font-medium" data-testid="problem-location" title={label(issue)}>
                      {label(issue)}
                    </span>
                    {issue.code ? (
                      <code className="shrink-0 rounded bg-muted px-1 py-0.5 text-[10px] text-muted-foreground">{issue.code}</code>
                    ) : null}
                  </span>
                  <span className="mt-0.5 block text-[11px] leading-snug text-muted-foreground">{issue.msg}</span>
                </span>
              </button>
            </li>
          ))}
        </ul>
      )}
    </div>
  );
}
