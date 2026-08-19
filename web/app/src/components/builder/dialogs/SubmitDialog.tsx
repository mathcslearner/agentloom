"use client";

// Submit a run from the builder (ticket 17.6). A params modal renders one field
// per declared `params` entry (typed + required-enforced client-side, since the
// backend stores params opaquely). When the canvas is clean and was saved, the
// run references the stored definition by id; otherwise the inline definition is
// submitted. A fresh Idempotency-Key per open lets a retry replay rather than
// double-submit. A 400 surfaces issues and selects the first offending step.

import { useEffect, useMemo, useState } from "react";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Checkbox } from "@/components/ui/checkbox";
import { JsonEditor } from "@/components/builder/config/JsonEditor";
import { toast } from "@/components/ui/toast";
import { useBuilderStore, selectIsDirty } from "@/lib/builder/store";
import { submitRun, type ApiIssue, type DefinitionValue } from "@/lib/builder/persistence";
import { stepIndexOfPath } from "@/lib/builder/problems";

type ParamType = "string" | "number" | "boolean" | "object" | "array";
interface ParamSpec {
  type: ParamType;
  required?: boolean;
}

type Phase =
  | { kind: "idle" }
  | { kind: "submitting" }
  | { kind: "done"; runId: string; reused: boolean }
  | { kind: "invalid"; issues: ApiIssue[]; message: string }
  | { kind: "error"; message: string };

function readParams(doc: Record<string, unknown>): Record<string, ParamSpec> {
  const p = doc["params"];
  if (typeof p !== "object" || p === null || Array.isArray(p)) return {};
  const out: Record<string, ParamSpec> = {};
  for (const [k, v] of Object.entries(p)) {
    if (typeof v === "object" && v !== null && "type" in v) out[k] = v as ParamSpec;
  }
  return out;
}

function newKey(): string {
  return typeof crypto !== "undefined" && "randomUUID" in crypto ? crypto.randomUUID() : `k-${Date.now()}-${Math.random()}`;
}

export function SubmitDialog({ open, onOpenChange }: { open: boolean; onOpenChange: (o: boolean) => void }) {
  const source = useBuilderStore((s) => s.source);
  const doc = useBuilderStore((s) => s.doc);
  const isDirty = useBuilderStore(selectIsDirty);
  const toDefinitionValue = useBuilderStore((s) => s.toDefinitionValue);
  const selectOnly = useBuilderStore((s) => s.selectOnly);

  const specs = useMemo(() => readParams(doc), [doc]);
  const [values, setValues] = useState<Record<string, unknown>>({});
  const [key, setKey] = useState("");
  const [phase, setPhase] = useState<Phase>({ kind: "idle" });

  useEffect(() => {
    if (open) {
      setValues({});
      setPhase({ kind: "idle" });
      setKey(newKey());
    }
  }, [open]);

  // Reference the stored definition only when the canvas is unchanged since it
  // was saved (otherwise the run must carry the current, unsaved graph).
  const useStored = source !== null && !isDirty;

  const missing = Object.entries(specs)
    .filter(([k, s]) => s.required && (values[k] === undefined || values[k] === ""))
    .map(([k]) => k);

  function setValue(name: string, v: unknown) {
    setValues((prev) => ({ ...prev, [name]: v }));
  }

  async function doSubmit() {
    if (missing.length > 0) return;
    setPhase({ kind: "submitting" });
    const params = Object.keys(values).length > 0 ? values : undefined;
    const res = await submitRun({
      ...(useStored ? { definitionId: source!.id } : { def: toDefinitionValue() as DefinitionValue }),
      params,
      idempotencyKey: key,
    });
    if (res.kind === "submitted") {
      toast({ title: res.reused ? "Run replayed" : "Run submitted", description: res.runId, variant: "success" });
      setPhase({ kind: "done", runId: res.runId, reused: res.reused });
      return;
    }
    if (res.kind === "invalid") {
      const { nodes } = useBuilderStore.getState();
      for (const iss of res.issues) {
        const idx = iss.path ? stepIndexOfPath(iss.path) : null;
        if (idx !== null && idx >= 0 && idx < nodes.length) {
          selectOnly("node", nodes[idx]!.id);
          break;
        }
      }
      setPhase({ kind: "invalid", issues: res.issues, message: res.message });
      return;
    }
    setPhase({ kind: "error", message: res.message });
  }

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent data-testid="submit-dialog">
        <DialogHeader>
          <DialogTitle>Submit a run</DialogTitle>
          <DialogDescription>
            {useStored
              ? `Runs the stored definition ${source!.name} v${source!.version}.`
              : "Runs the current (unsaved) canvas definition."}
          </DialogDescription>
        </DialogHeader>

        {phase.kind === "done" ? (
          <div className="rounded border border-emerald-500/40 bg-emerald-500/5 p-3 text-sm" data-testid="submit-done">
            <p className="font-medium">{phase.reused ? "Existing run replayed" : "Run submitted"}</p>
            <p className="mt-1 font-mono text-xs" data-testid="submit-run-id">
              {phase.runId}
            </p>
          </div>
        ) : (
          <div className="flex flex-col gap-3">
            {Object.keys(specs).length === 0 ? (
              <p className="text-xs text-muted-foreground">This workflow declares no parameters.</p>
            ) : (
              Object.entries(specs).map(([pname, spec]) => (
                <div key={pname} className="flex flex-col gap-1">
                  <Label htmlFor={`param-${pname}`}>
                    {pname} <span className="text-muted-foreground">({spec.type})</span>
                    {spec.required ? <span className="text-destructive"> *</span> : null}
                  </Label>
                  <ParamField
                    id={`param-${pname}`}
                    spec={spec}
                    value={values[pname]}
                    onChange={(v) => setValue(pname, v)}
                  />
                </div>
              ))
            )}
            {phase.kind === "invalid" ? (
              <div className="rounded border border-destructive/40 bg-destructive/5 p-2" data-testid="submit-issues">
                <p className="text-xs font-medium text-destructive">{phase.message}</p>
                <ul className="mt-1 space-y-0.5">
                  {phase.issues.slice(0, 8).map((iss, i) => (
                    <li key={i} className="text-[11px] text-destructive">
                      {iss.path ? <span className="font-mono">{iss.path}</span> : null} {iss.msg}
                    </li>
                  ))}
                </ul>
              </div>
            ) : null}
            {phase.kind === "error" ? (
              <p className="text-xs text-destructive" data-testid="submit-error">
                {phase.message}
              </p>
            ) : null}
          </div>
        )}

        <DialogFooter>
          <Button variant="outline" size="sm" onClick={() => onOpenChange(false)}>
            {phase.kind === "done" ? "Close" : "Cancel"}
          </Button>
          {phase.kind !== "done" ? (
            <Button
              size="sm"
              onClick={() => void doSubmit()}
              disabled={phase.kind === "submitting" || missing.length > 0}
              data-testid="submit-confirm"
              title={missing.length > 0 ? `Required: ${missing.join(", ")}` : undefined}
            >
              {phase.kind === "submitting" ? "Submitting…" : "Submit"}
            </Button>
          ) : null}
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}

function ParamField({
  id,
  spec,
  value,
  onChange,
}: {
  id: string;
  spec: ParamSpec;
  value: unknown;
  onChange: (v: unknown) => void;
}) {
  if (spec.type === "boolean") {
    return (
      <Checkbox
        id={id}
        data-testid={id}
        checked={value === true}
        onChange={(e) => onChange((e.target as HTMLInputElement).checked)}
      />
    );
  }
  if (spec.type === "number") {
    return (
      <Input
        id={id}
        data-testid={id}
        type="number"
        value={value === undefined ? "" : String(value)}
        onChange={(e) => onChange(e.target.value === "" ? undefined : Number(e.target.value))}
      />
    );
  }
  if (spec.type === "object" || spec.type === "array") {
    return <JsonEditor value={value} onChange={onChange} placeholder={`${spec.type} JSON`} rows={4} />;
  }
  return (
    <Input
      id={id}
      data-testid={id}
      value={typeof value === "string" ? value : ""}
      onChange={(e) => onChange(e.target.value === "" ? undefined : e.target.value)}
    />
  );
}
