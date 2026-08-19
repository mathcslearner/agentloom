"use client";

// Save the current canvas to the definition registry (ticket 17.6). A fresh
// canvas creates version 1 under its name; a canvas opened from a stored
// definition appends the next version, guarded by an `If-Match` precondition on
// the version it opened at (a stale save is refused so two editors do not fork
// silently). A name clash on create offers "append a version instead"; a 400
// surfaces the backend's issues and selects the first offending step.

import { useEffect, useState } from "react";
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
import { toast } from "@/components/ui/toast";
import { useBuilderStore } from "@/lib/builder/store";
import {
  appendDefinitionVersion,
  createDefinition,
  type ApiIssue,
  type DefinitionValue,
  type SaveOutcome,
} from "@/lib/builder/persistence";
import { stepIndexOfPath } from "@/lib/builder/problems";

type Phase =
  | { kind: "idle" }
  | { kind: "saving" }
  | { kind: "invalid"; issues: ApiIssue[]; message: string }
  | { kind: "version_conflict"; message: string }
  | { kind: "error"; message: string };

export function SaveDialog({ open, onOpenChange }: { open: boolean; onOpenChange: (o: boolean) => void }) {
  const source = useBuilderStore((s) => s.source);
  const doc = useBuilderStore((s) => s.doc);
  const patchDoc = useBuilderStore((s) => s.patchDoc);
  const markSaved = useBuilderStore((s) => s.markSaved);
  const toDefinitionValue = useBuilderStore((s) => s.toDefinitionValue);
  const selectOnly = useBuilderStore((s) => s.selectOnly);

  const name = typeof doc["name"] === "string" ? (doc["name"] as string) : "";
  const [phase, setPhase] = useState<Phase>({ kind: "idle" });

  useEffect(() => {
    if (open) setPhase({ kind: "idle" });
  }, [open]);

  const willAppend = source !== null && source.name === name;

  function selectFirstIssueNode(issues: ApiIssue[]) {
    const { nodes } = useBuilderStore.getState();
    for (const issue of issues) {
      const idx = issue.path ? stepIndexOfPath(issue.path) : null;
      if (idx !== null && idx >= 0 && idx < nodes.length) {
        selectOnly("node", nodes[idx]!.id);
        break;
      }
    }
  }

  function handleOutcome(res: SaveOutcome) {
    switch (res.kind) {
      case "created":
      case "appended":
        markSaved({ id: res.id, name: res.name, version: res.version });
        toast({ title: `Saved ${res.name} v${res.version}`, variant: "success" });
        onOpenChange(false);
        setPhase({ kind: "idle" });
        return;
      case "name_conflict":
        // A fresh canvas whose name already exists: adopt it as the base for
        // an append. Re-run as an append (the server allocates the next
        // version); no If-Match, since we did not open a specific version.
        void doSave(true);
        return;
      case "version_conflict":
        setPhase({ kind: "version_conflict", message: res.message });
        return;
      case "invalid":
        selectFirstIssueNode(res.issues);
        setPhase({ kind: "invalid", issues: res.issues, message: res.message });
        return;
      default:
        setPhase({ kind: "error", message: res.message });
    }
  }

  // `forceAppend` appends without an If-Match precondition (used to adopt an
  // existing name from a create clash, and to force past a stale-save conflict).
  async function doSave(forceAppend = false) {
    if (!name.trim()) {
      setPhase({ kind: "error", message: "a name is required" });
      return;
    }
    setPhase({ kind: "saving" });
    const def = toDefinitionValue() as DefinitionValue;
    let res: SaveOutcome;
    if (willAppend && !forceAppend) {
      res = await appendDefinitionVersion(name, def, source!.version);
    } else if (source !== null || forceAppend) {
      res = await appendDefinitionVersion(name, def); // no precondition
    } else {
      res = await createDefinition(name, def);
    }
    handleOutcome(res);
  }

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent data-testid="save-dialog">
        <DialogHeader>
          <DialogTitle>Save definition</DialogTitle>
          <DialogDescription>
            {willAppend
              ? `Append the next version of ${name} (opened at v${source!.version}).`
              : source
                ? `Save under ${name} (renamed from ${source.name}).`
                : "Register a new definition (version 1)."}
          </DialogDescription>
        </DialogHeader>

        <div className="flex flex-col gap-2">
          <Label htmlFor="save-name">Name</Label>
          <Input
            id="save-name"
            data-testid="save-name"
            value={name}
            onChange={(e) => patchDoc({ name: e.target.value })}
            placeholder="my-workflow"
          />
        </div>

        {phase.kind === "invalid" ? (
          <div className="rounded border border-destructive/40 bg-destructive/5 p-2" data-testid="save-issues">
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

        {phase.kind === "version_conflict" ? (
          <div className="rounded border border-amber-500/50 bg-amber-500/5 p-2 text-xs" data-testid="save-conflict">
            {phase.message}. Someone appended a version since you opened this. Save anyway as the next version, or cancel
            and re-open the latest.
          </div>
        ) : null}

        {phase.kind === "error" ? (
          <p className="text-xs text-destructive" data-testid="save-error">
            {phase.message}
          </p>
        ) : null}

        <DialogFooter>
          <Button variant="outline" size="sm" onClick={() => onOpenChange(false)}>
            Cancel
          </Button>
          {phase.kind === "version_conflict" ? (
            <Button size="sm" onClick={() => void doSave(true)} data-testid="save-force">
              Save anyway
            </Button>
          ) : (
            <Button size="sm" onClick={() => void doSave()} disabled={phase.kind === "saving"} data-testid="save-confirm">
              {phase.kind === "saving" ? "Saving…" : willAppend ? "Save new version" : "Save"}
            </Button>
          )}
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
