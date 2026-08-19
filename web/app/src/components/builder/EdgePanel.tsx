"use client";

// EdgePanel (ticket 17.5) — the minimal edge inspector, pulled forward from 17.6
// so the loop-edge authoring flow (DoD-2) is completable in the UI: mark a normal
// edge as a loop, set its `condition` + `max_iterations` + `on_exhausted`, or edit
// a normal edge's `when` guard and (on an approval edge) its `decision`. Deeper
// editing (autocomplete in conditions, no_progress, decision routing) is 17.6.
// Writes go through store.patchEdge, which re-derives the render type and handle.

import { useBuilderStore } from "@/lib/builder/store";
import { edgeData, type CanvasEdge } from "@/lib/builder/adapter";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Checkbox } from "@/components/ui/checkbox";
import { useProblemsCtx } from "@/lib/builder/problems-context";

export function EdgePanel({ selected }: { selected: CanvasEdge }) {
  const patchEdge = useBuilderStore((s) => s.patchEdge);
  const nodes = useBuilderStore((s) => s.nodes);
  const { problems } = useProblemsCtx();
  const edge = edgeData(selected).edge ?? { from: selected.source, target: selected.target };
  const isLoop = (edge as { type?: string }).type === "loop";
  const issues = problems.byEdge.get(selected.id) ?? [];

  // A `decision` marker is meaningful only on a normal edge leaving a
  // human_approval step (ADR-017 reject routing).
  const sourceNode = nodes.find((n) => n.id === selected.source);
  const sourceStep = (sourceNode?.data as { step?: { type?: string } } | undefined)?.step;
  const fromApproval = sourceStep?.type === "human_approval";
  const noProgress = (edge as { no_progress?: Record<string, unknown> }).no_progress;

  const set = (patch: Record<string, unknown>) => patchEdge(selected.id, patch);

  return (
    <div className="border-b p-3" aria-label="Edge inspector">
      <div className="flex items-center justify-between">
        <h2 className="text-xs font-semibold uppercase tracking-wide text-muted-foreground">Edge</h2>
        <span className="text-[11px] text-muted-foreground" data-testid="inspector-edge-id">
          {(edge as { from?: string }).from} → {(edge as { to?: string }).to}
        </span>
      </div>

      <label className="mt-3 flex items-center gap-2 text-sm">
        <Checkbox
          data-testid="edge-loop-toggle"
          checked={isLoop}
          onChange={(e) => {
            if (e.target.checked) set({ type: "loop", condition: "", max_iterations: 3, when: undefined });
            else set({ type: undefined, condition: undefined, max_iterations: undefined, on_exhausted: undefined });
          }}
        />
        <span>Loop edge (iterate the body until the condition is false)</span>
      </label>

      {isLoop ? (
        <div className="mt-3 space-y-3">
          <div className="space-y-1">
            <Label htmlFor="edge-condition">Condition (CEL)</Label>
            <Input
              id="edge-condition"
              data-testid="edge-condition"
              value={String((edge as { condition?: string }).condition ?? "")}
              placeholder="output.verdict == 'revise'"
              onChange={(e) => set({ condition: e.target.value })}
            />
          </div>
          <div className="space-y-1">
            <Label htmlFor="edge-max-iterations">Max iterations</Label>
            <Input
              id="edge-max-iterations"
              data-testid="edge-max-iterations"
              type="number"
              min={1}
              value={String((edge as { max_iterations?: number }).max_iterations ?? "")}
              onChange={(e) => set({ max_iterations: e.target.value === "" ? undefined : Number(e.target.value) })}
            />
          </div>
          <div className="space-y-1">
            <Label htmlFor="edge-on-exhausted">On exhausted</Label>
            <select
              id="edge-on-exhausted"
              data-testid="edge-on-exhausted"
              className="h-9 w-full rounded-md border border-input bg-transparent px-3 text-sm"
              value={String((edge as { on_exhausted?: string }).on_exhausted ?? "proceed")}
              onChange={(e) => set({ on_exhausted: e.target.value })}
            >
              <option value="proceed">proceed (continue past the loop)</option>
              <option value="fail">fail (dead-letter the loop)</option>
            </select>
          </div>

          <label className="flex items-center gap-2 text-sm">
            <Checkbox
              data-testid="edge-no-progress-toggle"
              checked={noProgress !== undefined}
              onChange={(e) =>
                set({ no_progress: e.target.checked ? { policy: "proceed" } : undefined })
              }
            />
            <span>No-progress guard (stop if the body output stops changing)</span>
          </label>
          {noProgress !== undefined ? (
            <div className="space-y-3 border-l pl-3">
              <div className="space-y-1">
                <Label htmlFor="edge-np-step">Compared step (optional — defaults to the loop source)</Label>
                <Input
                  id="edge-np-step"
                  data-testid="edge-np-step"
                  value={String((noProgress as { step?: string }).step ?? "")}
                  placeholder="body step id"
                  onChange={(e) =>
                    set({ no_progress: { ...noProgress, step: e.target.value === "" ? undefined : e.target.value } })
                  }
                />
              </div>
              <div className="space-y-1">
                <Label htmlFor="edge-np-policy">No-progress policy</Label>
                <select
                  id="edge-np-policy"
                  data-testid="edge-np-policy"
                  className="h-9 w-full rounded-md border border-input bg-transparent px-3 text-sm"
                  value={String((noProgress as { policy?: string }).policy ?? "proceed")}
                  onChange={(e) => set({ no_progress: { ...noProgress, policy: e.target.value } })}
                >
                  <option value="proceed">proceed</option>
                  <option value="fail">fail</option>
                </select>
              </div>
            </div>
          ) : null}
        </div>
      ) : (
        <div className="mt-3 space-y-3">
          <div className="space-y-1">
            <Label htmlFor="edge-when">When (CEL guard, optional)</Label>
            <Input
              id="edge-when"
              data-testid="edge-when"
              value={String((edge as { when?: string }).when ?? "")}
              placeholder="output.category == 'refund'"
              onChange={(e) => set({ when: e.target.value === "" ? undefined : e.target.value })}
            />
          </div>
          {fromApproval ? (
            <div className="space-y-1">
              <Label htmlFor="edge-decision">Decision route (human_approval)</Label>
              <select
                id="edge-decision"
                data-testid="edge-decision"
                className="h-9 w-full rounded-md border border-input bg-transparent px-3 text-sm"
                value={String((edge as { decision?: string }).decision ?? "")}
                onChange={(e) => set({ decision: e.target.value === "" ? undefined : e.target.value })}
              >
                <option value="">(any — fires on approve)</option>
                <option value="approve">approve</option>
                <option value="reject">reject</option>
              </select>
            </div>
          ) : null}
        </div>
      )}

      {issues.length > 0 ? (
        <ul className="mt-3 space-y-1" data-testid="edge-problems">
          {issues.map((issue, i) => (
            <li key={i} className="text-[11px] text-destructive">
              {issue.msg}
            </li>
          ))}
        </ul>
      ) : null}
    </div>
  );
}
