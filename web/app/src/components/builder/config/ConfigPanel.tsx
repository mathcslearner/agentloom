"use client";

// The config panel (ticket 17.4): the selected step's schema-driven config form
// plus its problems, replacing the placeholder "Selection" section in the
// Inspector. Fields are generated from the plugin's JSON Schema (GET /v1/plugins,
// with the published-schema fallback) — no per-plugin hardcoding. An "Envelope"
// section edits retry/cache/budget/validation/blackboard/context as raw JSON
// (schema-driven envelope forms are 17.5's remit).

import { useEffect, useMemo } from "react";
import type { StepType } from "@agentloom/graphdef";
import { planFields } from "@/lib/pure/builder/fields";
import { stepMeta } from "@/lib/pure/builder/catalog";
import type { RefEdge, RefNode } from "@/lib/pure/builder/refs";
import { beginInteraction, endInteraction, useBuilderStore } from "@/lib/builder/store";
import { useCatalogStore } from "@/lib/builder/catalog-store";
import { useProblems } from "@/lib/builder/problems";
import { edgeData, nodeStep, type CanvasEdge, type CanvasNode } from "@/lib/builder/adapter";
import { Badge } from "@/components/ui/badge";
import { SchemaForm, type AutocompleteContext } from "./SchemaForm";
import { JsonEditor } from "./JsonEditor";

const ENVELOPE_KEYS = ["retry", "timeout", "cache", "budget", "validation", "blackboard", "context"];

function refNodes(nodes: CanvasNode[]): RefNode[] {
  return nodes.map((n) => ({ id: n.id, type: (n.type ?? "noop") as StepType }));
}
function refEdges(edges: CanvasEdge[]): RefEdge[] {
  return edges.map((e) => ({ from: e.source, to: e.target, loop: edgeData(e).edge.type === "loop" }));
}

export function ConfigPanel({ selected }: { selected: CanvasNode }) {
  const nodes = useBuilderStore((s) => s.nodes);
  const edges = useBuilderStore((s) => s.edges);
  const doc = useBuilderStore((s) => s.doc);
  const patchStep = useBuilderStore((s) => s.patchStep);
  const setStepEnvelope = useBuilderStore((s) => s.setStepEnvelope);

  const catalog = useCatalogStore((s) => s.catalog);
  const schemas = useCatalogStore((s) => s.schemas);
  const status = useCatalogStore((s) => s.status);
  const load = useCatalogStore((s) => s.load);
  useEffect(() => {
    void load();
  }, [load]);

  const problems = useProblems();

  const step = nodeStep(selected);
  const type = step.type as StepType;
  const config = (step as { config?: Record<string, unknown> }).config ?? {};
  const schemaEntry = schemas[type];

  const fields = useMemo(() => {
    if (!schemaEntry) return [];
    return planFields(schemaEntry.schema, { stepType: type, defs: schemaEntry.defs });
  }, [schemaEntry, type]);

  const autocomplete: AutocompleteContext = {
    nodes: refNodes(nodes),
    edges: refEdges(edges),
    doc,
    currentStepId: selected.id,
  };

  const stepIssues = problems.byNode.get(selected.id) ?? [];
  const stepRecord = step as unknown as Record<string, unknown>;
  const envelope = Object.fromEntries(ENVELOPE_KEYS.filter((k) => k in stepRecord).map((k) => [k, stepRecord[k]]));
  const hasEnvelope = Object.keys(envelope).length > 0;

  return (
    <div className="flex min-h-0 flex-1 flex-col overflow-auto">
      <div className="border-b p-3">
        <div className="flex items-center justify-between">
          <div>
            <div className="font-medium" data-testid="inspector-step-id">
              {step.id}
            </div>
            <div className="text-xs text-muted-foreground">{stepMeta(type).label}</div>
          </div>
          {stepIssues.length > 0 ? (
            <Badge className="bg-destructive/10 text-destructive" data-testid="step-problem-count">
              {stepIssues.length} {stepIssues.length === 1 ? "issue" : "issues"}
            </Badge>
          ) : null}
        </div>
        {status === "error" ? (
          <p className="mt-2 text-[11px] text-amber-600">
            plugin catalog unavailable — using the published schema; tool/model pickers are free-text only.
          </p>
        ) : null}
      </div>

      <div className="flex flex-col gap-4 p-3">
        <section>
          <h3 className="mb-2 text-xs font-semibold uppercase tracking-wide text-muted-foreground">Config</h3>
          {schemaEntry ? (
            <SchemaForm
              stepType={type}
              fields={fields}
              value={config}
              catalog={catalog}
              autocomplete={autocomplete}
              beginEdit={beginInteraction}
              endEdit={endInteraction}
              onChange={(v) => patchStep(selected.id, { config: v })}
            />
          ) : (
            <JsonEditor
              value={config}
              onFocus={beginInteraction}
              onBlur={endInteraction}
              onChange={(v) => patchStep(selected.id, { config: v ?? {} })}
            />
          )}
        </section>

        {stepIssues.length > 0 ? (
          <section data-testid="step-problems">
            <h3 className="mb-2 text-xs font-semibold uppercase tracking-wide text-muted-foreground">Problems</h3>
            <ul className="flex flex-col gap-1">
              {stepIssues.map((issue, i) => (
                <li key={i} className="rounded border border-destructive/40 bg-destructive/5 px-2 py-1 text-[11px]">
                  <span className="font-mono text-muted-foreground">{issue.path}</span>
                  <div className="text-destructive">{issue.msg}</div>
                  {issue.code ? <div className="text-[10px] text-muted-foreground">{issue.code}</div> : null}
                </li>
              ))}
            </ul>
          </section>
        ) : null}

        <section>
          <h3 className="mb-2 text-xs font-semibold uppercase tracking-wide text-muted-foreground">
            Envelope {hasEnvelope ? <span className="font-normal normal-case">(retry, cache, budget, …)</span> : null}
          </h3>
          <JsonEditor
            value={hasEnvelope ? envelope : undefined}
            placeholder='e.g. {"retry": {"max_attempts": 3, "backoff": {"initial": "1s", "cap": "30s"}}}'
            rows={hasEnvelope ? 6 : 2}
            onFocus={beginInteraction}
            onBlur={endInteraction}
            onChange={(v) => setStepEnvelope(selected.id, (v ?? {}) as Record<string, unknown>)}
          />
        </section>
      </div>
    </div>
  );
}
